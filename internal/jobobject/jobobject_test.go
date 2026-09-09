//go:build windows

package jobobject

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// startLongRunning launches a process that stays alive until killed, suspended
// so the caller can place it in a job before it can spawn anything.
func startLongRunning(t *testing.T) (*exec.Cmd, windows.Handle) {
	t.Helper()

	// ping with a large count is available on every Windows install and does not
	// need a console to keep running.
	cmd := exec.Command("cmd.exe", "/c", "ping -n 600 127.0.0.1 > nul")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("OpenProcess: %v", err)
	}

	t.Cleanup(func() {
		_ = windows.CloseHandle(h)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd, h
}

// resume releases a process started with CREATE_SUSPENDED.
//
// Processes must be assigned to a job before they run, otherwise a process that
// spawns children immediately can produce grandchildren outside the job — which
// would escape both the limits and Terminate.
func resume(t *testing.T, pid int) {
	t.Helper()

	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer windows.CloseHandle(snap)

	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	if err := windows.Thread32First(snap, &te); err != nil {
		t.Fatalf("Thread32First: %v", err)
	}
	for {
		if te.OwnerProcessID == uint32(pid) {
			th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
			if err == nil {
				_, _ = windows.ResumeThread(th)
				_ = windows.CloseHandle(th)
			}
		}
		if err := windows.Thread32Next(snap, &te); err != nil {
			break
		}
	}
}

func TestJobAssignAndStats(t *testing.T) {
	j, err := Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer j.Close()

	if err := j.SetLimits(Limits{ProcessLimit: 16}); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	cmd, h := startLongRunning(t)
	if err := j.Assign(h); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	resume(t, cmd.Process.Pid)

	// Prime the CPU delta, then sample.
	if _, err := j.Stats(); err != nil {
		t.Fatalf("Stats (priming): %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	s, err := j.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	t.Logf("procs=%d mem=%d cpu=%.2f%% read=%d write=%d",
		s.ActiveProcs, s.MemoryBytes, s.CpuAbsolute, s.DiskReadBytes, s.DiskWriteBytes)

	if s.ActiveProcs == 0 {
		t.Error("expected at least one active process in the job")
	}
	if s.MemoryBytes == 0 {
		t.Error("expected non-zero job memory; JOBOBJECT_MEMORY_USAGE_INFORMATION may be unavailable")
	}
	if s.CpuAbsolute < 0 {
		t.Errorf("CpuAbsolute must never be negative, got %v", s.CpuAbsolute)
	}
}

func TestJobTerminateKillsTree(t *testing.T) {
	j, err := Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer j.Close()

	if err := j.SetLimits(Limits{}); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}

	cmd, h := startLongRunning(t)
	if err := j.Assign(h); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	resume(t, cmd.Process.Pid)

	// cmd.exe spawns ping as a child; both must die.
	time.Sleep(200 * time.Millisecond)
	before, _ := j.Stats()
	t.Logf("active processes before terminate: %d", before.ActiveProcs)
	if before.ActiveProcs < 2 {
		t.Logf("note: expected cmd.exe plus ping, saw %d — child may not have spawned yet",
			before.ActiveProcs)
	}

	if err := j.Terminate(1); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, err := j.Stats()
		if err != nil {
			t.Fatalf("Stats after terminate: %v", err)
		}
		if s.ActiveProcs == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("processes still active 5s after TerminateJobObject")
}

// The CPU rate weight path is only valid for values 1..9; anything outside that
// range makes SetInformationJobObject fail, so the mapping has to clamp.
func TestWeightFromRateClamps(t *testing.T) {
	cases := []struct {
		rate uint32
		want uint32
	}{
		{0, 1},
		{1, 1},
		{5000, 4},
		{10000, 9},
		{50000, 9},
	}
	for _, c := range cases {
		if got := weightFromRate(c.rate); got != c.want {
			t.Errorf("weightFromRate(%d) = %d, want %d", c.rate, got, c.want)
		}
	}
}

func TestSetLimitsCpuHardCap(t *testing.T) {
	j, err := Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer j.Close()

	// Both paths must be accepted by the kernel.
	if err := j.SetLimits(Limits{CpuRate: 2500, CpuHardCap: true}); err != nil {
		t.Errorf("hard cap: %v", err)
	}
	if err := j.SetLimits(Limits{CpuRate: 2500, CpuHardCap: false}); err != nil {
		t.Errorf("weight based: %v", err)
	}
}

func TestSetLimitsMemoryAndProcesses(t *testing.T) {
	j, err := Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer j.Close()

	if err := j.SetLimits(Limits{
		MemoryBytes:  512 << 20,
		ProcessLimit: 64,
		AffinityMask: 0x3,
	}); err != nil {
		t.Fatalf("SetLimits: %v", err)
	}
}
