//go:build windows

// Package jobobject wraps Windows Job Objects, which are the closest thing the
// platform offers to the cgroup controls Docker gave upstream wings.
//
// A job provides, in one kernel object: a memory cap, CPU rate control,
// processor affinity, a cap on active processes, aggregate CPU and I/O
// accounting, notification when a limit is breached, and — critically — a way to
// kill an entire process tree atomically. A game server that spawns helpers
// cannot escape its job, which is what makes this a usable containment boundary
// for stopping servers.
//
// What it does not provide is filesystem or network isolation. Those are handled
// by running servers under separate accounts with NTFS ACLs; see
// config.AccountConfiguration.
package jobobject

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Information classes not bound by golang.org/x/sys/windows.
const (
	jobObjectMemoryUsageInformation = 28
)

// CPU rate control flags. Not exported by x/sys/windows.
const (
	cpuRateControlEnable      = 0x1
	cpuRateControlWeightBased = 0x2
	cpuRateControlHardCap     = 0x4
)

// Completion port message codes delivered for a job's limit events.
const (
	msgEndOfJobTime        = 1
	msgActiveProcessLimit  = 3
	msgActiveProcessZero   = 4
	msgNewProcess          = 6
	msgExitProcess         = 7
	msgAbnormalExitProcess = 8
	msgProcessMemoryLimit  = 9
	msgJobMemoryLimit      = 10
	msgNotificationLimit   = 11
)

// jobObjectCpuRateControlInformation mirrors
// JOBOBJECT_CPU_RATE_CONTROL_INFORMATION. The C definition carries a union of
// CpuRate, Weight, and a MinRate/MaxRate pair; all three are four bytes, so a
// single field suffices and the interpretation follows ControlFlags.
type jobObjectCpuRateControlInformation struct {
	ControlFlags uint32
	Value        uint32
}

// jobObjectBasicAccountingInformation mirrors
// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// jobObjectBasicAndIoAccountingInformation mirrors
// JOBOBJECT_BASIC_AND_IO_ACCOUNTING_INFORMATION.
type jobObjectBasicAndIoAccountingInformation struct {
	BasicInfo jobObjectBasicAccountingInformation
	IoInfo    windows.IO_COUNTERS
}

// jobObjectMemoryUsage mirrors JOBOBJECT_MEMORY_USAGE_INFORMATION, which is the
// only way to read a job's *current* committed memory. The extended limit
// information struct reports peak usage only, which is not what a live resource
// graph needs.
type jobObjectMemoryUsage struct {
	JobMemory     uint64
	PeakJobMemory uint64
}

// jobObjectAssociateCompletionPort mirrors JOBOBJECT_ASSOCIATE_COMPLETION_PORT.
type jobObjectAssociateCompletionPort struct {
	CompletionKey  uintptr
	CompletionPort windows.Handle
}

// Limits describes the resource constraints applied to a job.
type Limits struct {
	// MemoryBytes caps total committed memory for the job. Zero disables the cap.
	MemoryBytes int64

	// CpuRate is a share of total host CPU in 1/100ths of a percent, where 10000
	// is every processor. Zero disables CPU rate control.
	CpuRate uint32

	// CpuHardCap enforces CpuRate even on an idle host. Without it, the rate
	// behaves as a relative weight that only bites under contention.
	CpuHardCap bool

	// AffinityMask restricts the job to specific processors. Zero means all.
	AffinityMask uint64

	// ProcessLimit caps concurrently active processes. Zero disables the cap.
	ProcessLimit uint32
}

// Event is a notification delivered by the kernel about a job.
type Event struct {
	// MemoryLimitHit reports that the job reached its memory cap. This is the
	// nearest analogue to Docker's OOMKilled flag; note that unlike an OOM kill
	// nothing is reaped — allocations simply begin to fail.
	MemoryLimitHit bool
	// ProcessLimitHit reports that the active process cap was reached.
	ProcessLimitHit bool
	// AllProcessesExited reports that the job became empty.
	AllProcessesExited bool
}

// Job is a handle on a Windows Job Object.
type Job struct {
	handle windows.Handle
	port   windows.Handle

	mu       sync.Mutex
	closed   bool
	events   chan Event
	lastCPU  int64
	lastPoll time.Time
}

// Create makes a new anonymous job object.
//
// Limits, including the kill-on-close behaviour that ties the job to its
// creator, are applied by SetLimits.
func Create() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("jobobject: create: %w", err)
	}

	j := &Job{handle: h, events: make(chan Event, 16)}

	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("jobobject: create completion port: %w", err)
	}
	j.port = port

	assoc := jobObjectAssociateCompletionPort{
		CompletionKey:  uintptr(h),
		CompletionPort: port,
	}
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectAssociateCompletionPortInformation,
		uintptr(unsafe.Pointer(&assoc)),
		uint32(unsafe.Sizeof(assoc)),
	); err != nil {
		_ = windows.CloseHandle(port)
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("jobobject: associate completion port: %w", err)
	}

	go j.pumpEvents()
	return j, nil
}

// Handle exposes the raw job handle, for passing to process creation.
func (j *Job) Handle() windows.Handle { return j.handle }

// Events returns the channel on which limit notifications are delivered.
func (j *Job) Events() <-chan Event { return j.events }

// SetLimits applies resource limits to the job. It is safe to call on a running
// job, which is how in-situ limit updates are performed.
func (j *Job) SetLimits(l Limits) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION

	if l.MemoryBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(l.MemoryBytes)
	}
	if l.ProcessLimit > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = l.ProcessLimit
	}
	if l.AffinityMask != 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_AFFINITY
		info.BasicLimitInformation.Affinity = uintptr(l.AffinityMask)
	}

	// Tie the job's lifetime to the handle held by the worker. If the worker
	// dies the server dies with it.
	//
	// The alternative — leaving the process running — produces an orphan with no
	// console, no stdin, and no supervisor, which the daemon would then try to
	// start a second copy of. Two instances sharing one server directory and one
	// port is worse than a stopped server.
	//
	// This does not couple servers to the *daemon's* lifetime, which is the
	// property that mattered: the worker holds this handle, not the daemon, so
	// win-wings can restart freely without touching running servers.
	info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE

	// Deliberately NOT set: JOB_OBJECT_LIMIT_BREAKAWAY_OK and
	// JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK. Either would let a server spawn a
	// child outside the job, escaping both its resource limits and Terminate's
	// kill-the-tree guarantee.

	// Kill a process that faults rather than letting Windows Error Reporting
	// park it on a dialog nobody will ever click, holding the job open.
	info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION

	if _, err := windows.SetInformationJobObject(
		j.handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fmt.Errorf("jobobject: set extended limits: %w", err)
	}

	if l.CpuRate > 0 {
		rate := jobObjectCpuRateControlInformation{
			ControlFlags: cpuRateControlEnable,
			Value:        l.CpuRate,
		}
		if l.CpuHardCap {
			rate.ControlFlags |= cpuRateControlHardCap
		} else {
			// Without a hard cap the field is interpreted as a weight in 1..9
			// rather than a rate, so the value has to be rescaled or the call
			// fails outright.
			rate.ControlFlags |= cpuRateControlWeightBased
			rate.Value = weightFromRate(l.CpuRate)
		}

		if _, err := windows.SetInformationJobObject(
			j.handle,
			windows.JobObjectCpuRateControlInformation,
			uintptr(unsafe.Pointer(&rate)),
			uint32(unsafe.Sizeof(rate)),
		); err != nil {
			return fmt.Errorf("jobobject: set cpu rate control: %w", err)
		}
	}

	return nil
}

// weightFromRate maps a CPU rate in 1/100ths of a percent onto the 1..9 weight
// scale used when a hard cap is not requested. 5 is the scheduler's default.
func weightFromRate(rate uint32) uint32 {
	w := (rate * 9) / 10000
	if w < 1 {
		return 1
	}
	if w > 9 {
		return 9
	}
	return w
}

// Assign places a process into the job. Every child it later spawns is placed in
// the job automatically.
func (j *Job) Assign(process windows.Handle) error {
	if err := windows.AssignProcessToJobObject(j.handle, process); err != nil {
		return fmt.Errorf("jobobject: assign process: %w", err)
	}
	return nil
}

// Terminate kills every process in the job. This is the stop of last resort,
// used when a graceful shutdown times out.
func (j *Job) Terminate(exitCode uint32) error {
	if err := windows.TerminateJobObject(j.handle, exitCode); err != nil {
		return fmt.Errorf("jobobject: terminate: %w", err)
	}
	return nil
}

// Close releases the job handle.
//
// Because SetLimits sets JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, this kills every
// process in the job once the last handle goes away. Call it only when the
// server is meant to stop.
func (j *Job) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true

	// Closing the completion port unblocks the event pump.
	if j.port != 0 {
		_ = windows.CloseHandle(j.port)
	}
	return windows.CloseHandle(j.handle)
}

// Stats is a sample of a job's aggregate resource usage.
type Stats struct {
	MemoryBytes uint64
	// CpuAbsolute is CPU use as a percentage of one processor, so 200 means two
	// processors fully used. This matches the units the Panel displays.
	CpuAbsolute    float64
	ActiveProcs    uint32
	DiskReadBytes  uint64
	DiskWriteBytes uint64
}

// Stats samples the job's current resource usage.
//
// CPU is derived from the delta in accumulated processor time between calls, so
// the first call after creation reports zero and callers should poll on a fixed
// interval.
func (j *Job) Stats() (Stats, error) {
	var acct jobObjectBasicAndIoAccountingInformation
	if err := windows.QueryInformationJobObject(
		j.handle,
		windows.JobObjectBasicAndIoAccountingInformation,
		uintptr(unsafe.Pointer(&acct)),
		uint32(unsafe.Sizeof(acct)),
		nil,
	); err != nil {
		return Stats{}, fmt.Errorf("jobobject: query accounting: %w", err)
	}

	s := Stats{
		ActiveProcs:    acct.BasicInfo.ActiveProcesses,
		DiskReadBytes:  acct.IoInfo.ReadTransferCount,
		DiskWriteBytes: acct.IoInfo.WriteTransferCount,
	}

	// Processor time is reported in 100ns units, as is wall time here, so the
	// ratio is directly a fraction of one processor.
	totalCPU := acct.BasicInfo.TotalUserTime + acct.BasicInfo.TotalKernelTime
	now := time.Now()

	j.mu.Lock()
	if !j.lastPoll.IsZero() {
		wall := now.Sub(j.lastPoll).Nanoseconds() / 100
		if wall > 0 {
			s.CpuAbsolute = float64(totalCPU-j.lastCPU) / float64(wall) * 100
		}
	}
	j.lastCPU = totalCPU
	j.lastPoll = now
	j.mu.Unlock()

	if s.CpuAbsolute < 0 {
		s.CpuAbsolute = 0
	}

	var mem jobObjectMemoryUsage
	if err := windows.QueryInformationJobObject(
		j.handle,
		jobObjectMemoryUsageInformation,
		uintptr(unsafe.Pointer(&mem)),
		uint32(unsafe.Sizeof(mem)),
		nil,
	); err == nil {
		s.MemoryBytes = mem.JobMemory
	} else {
		// JOBOBJECT_MEMORY_USAGE_INFORMATION requires Windows 10 or Server 2016.
		// Fall back to peak usage from the extended limit information, which is
		// available everywhere but overstates current use. Better an inflated
		// figure than none: the memory graph is informational, and the actual
		// enforcement is the kernel's.
		var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		if err := windows.QueryInformationJobObject(
			j.handle,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&ext)),
			uint32(unsafe.Sizeof(ext)),
			nil,
		); err == nil {
			s.MemoryBytes = uint64(ext.PeakJobMemoryUsed)
		}
	}

	return s, nil
}

// pumpEvents translates completion port notifications into Event values.
func (j *Job) pumpEvents() {
	for {
		var bytes uint32
		var key uintptr
		var overlapped *windows.Overlapped

		err := windows.GetQueuedCompletionStatus(j.port, &bytes, &key, &overlapped, windows.INFINITE)
		if err != nil {
			// The port was closed, or the job went away. Either way we are done.
			close(j.events)
			return
		}

		var ev Event
		switch bytes {
		case msgJobMemoryLimit, msgProcessMemoryLimit:
			ev.MemoryLimitHit = true
		case msgActiveProcessLimit:
			ev.ProcessLimitHit = true
		case msgActiveProcessZero:
			ev.AllProcessesExited = true
		case msgEndOfJobTime, msgNewProcess, msgExitProcess, msgAbnormalExitProcess, msgNotificationLimit:
			// Not currently acted on. Process exit is observed directly by
			// waiting on the process handle, which carries the exit code.
			continue
		default:
			continue
		}

		select {
		case j.events <- ev:
		default:
			// A slow consumer must not stall the pump; limit events are
			// advisory and the latest state is re-derived on exit anyway.
		}
	}
}
