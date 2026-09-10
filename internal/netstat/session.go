//go:build windows

package netstat

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// kernelNetworkProvider is Microsoft-Windows-Kernel-Network, the provider the
// TCP/IP stack writes a send and a receive event to for every segment and
// datagram, each stamped with the owning process and the payload size. It is
// the only per-process network accounting Windows offers outside a container.
var kernelNetworkProvider = windows.GUID{
	Data1: 0x7dd42a49, Data2: 0x5329, Data3: 0x4832,
	Data4: [8]byte{0x8d, 0xfd, 0x43, 0xd9, 0x79, 0x15, 0x3a, 0x88},
}

// Event identifiers from the provider's manifest. Only data-carrying events are
// listed; connect, accept, disconnect and retransmit events exist too and are
// ignored.
const (
	evTCPv4Send = 10
	evTCPv4Recv = 11
	evTCPv6Send = 26
	evTCPv6Recv = 27
	evUDPv4Send = 42
	evUDPv4Recv = 43
	evUDPv6Send = 58
	evUDPv6Recv = 59
)

// direction classifies a data event. Zero means not a data event.
type direction uint8

const (
	dirNone direction = iota
	dirSend
	dirRecv
)

func classify(id uint16) direction {
	switch id {
	case evTCPv4Send, evTCPv6Send, evUDPv4Send, evUDPv6Send:
		return dirSend
	case evTCPv4Recv, evTCPv6Recv, evUDPv4Recv, evUDPv6Recv:
		return dirRecv
	}
	return dirNone
}

// dataEventHeader is the prefix every data event shares. The fields after it
// differ between templates (addresses, ports, sequence numbers) and are not
// needed, so only this much is read.
type dataEventHeader struct {
	PID  uint32
	Size uint32
}

// Session is a real-time ETW session consuming kernel-network events and
// feeding them to an Accountant.
//
// One is enough for the whole daemon, and one is also close to the most the
// host allows: a provider can be enabled by at most eight sessions at once,
// and the machine has a hard cap of 64. A session per worker would exhaust
// both on a modest node, which is why attribution happens here by PID rather
// than each worker counting its own.
type Session struct {
	name   string
	acct   *Accountant
	handle uint64 // the session, from StartTrace
	trace  uint64 // the consumer, from OpenTrace

	done    chan struct{}
	procErr error

	stopOnce sync.Once
}

// One callback for the process. windows.NewCallback allocates a trampoline
// that is never released, so it is made once and reads the active session
// from here rather than closing over one.
var (
	callbackOnce   sync.Once
	recordCallback uintptr
	active         atomic.Pointer[Session]
)

func eventRecordCallback() uintptr {
	callbackOnce.Do(func() {
		recordCallback = windows.NewCallback(onEvent)
	})
	return recordCallback
}

// onEvent runs on the ProcessTrace thread once per event. It is the hot path:
// classify, read eight bytes, hand off.
func onEvent(rec *eventRecord) uintptr {
	s := active.Load()
	if s == nil || rec.Header.ProviderID != kernelNetworkProvider {
		return 0
	}
	dir := classify(rec.Header.Descriptor.ID)
	if dir == dirNone || uintptr(rec.UserDataLength) < unsafe.Sizeof(dataEventHeader{}) {
		return 0
	}
	h := (*dataEventHeader)(rec.UserData)
	var c Counters
	if dir == dirSend {
		c.Tx = uint64(h.Size)
	} else {
		c.Rx = uint64(h.Size)
	}
	s.acct.Record(h.PID, c)
	return 0
}

// StartSession begins tracing into acct.
//
// A session with the same name left over from a previous daemon that did not
// stop cleanly is stopped first. ETW sessions are kernel objects that outlive
// their creator; a stale one would make StartTrace fail with "already exists"
// forever.
//
// Starting a session needs the caller to be an administrator or a member of
// the Performance Log Users group; anything else fails here with access
// denied.
func StartSession(name string, acct *Accountant) (*Session, error) {
	if len(name) == 0 || len(name) >= maxSessionName {
		return nil, fmt.Errorf("netstat: invalid session name")
	}
	if prev := active.Load(); prev != nil {
		return nil, fmt.Errorf("netstat: a session is already active in this process")
	}

	// Ignore the result: the usual outcome is that nothing was there.
	_ = stopByName(name)

	props, keep := newProperties()
	props.Wnode.ClientContext = 1 // timestamps from QueryPerformanceCounter
	props.LogFileMode = eventTraceRealTimeMode
	// Buffering. Each event is around a hundred bytes and a busy game server
	// produces tens of thousands a second, so the buffers have to absorb a
	// second or two of that while the consumer thread catches up; anything
	// that does not fit is dropped and shows up as a low reading.
	props.BufferSize = 64 // KB
	props.MinimumBuffers = 16
	props.MaximumBuffers = 128
	props.FlushTimer = 1 // seconds; bounds latency at low packet rates

	handle, err := startTrace(name, props)
	runtime.KeepAlive(keep)
	if err != nil {
		return nil, fmt.Errorf("netstat: start session %q: %w", name, err)
	}

	s := &Session{name: name, acct: acct, handle: handle, done: make(chan struct{})}

	// Every keyword. The data events sit behind the IPv4 and IPv6 keywords,
	// but there is nothing else in this provider worth excluding and a wrong
	// mask here means silently counting nothing.
	if err := enableProvider(handle, &kernelNetworkProvider, traceLevelAll, ^uint64(0)); err != nil {
		_ = stopByName(name)
		return nil, fmt.Errorf("netstat: enable kernel-network provider: %w", err)
	}

	logname, err := windows.UTF16PtrFromString(name)
	if err != nil {
		_ = stopByName(name)
		return nil, err
	}
	var lf eventTraceLogfile
	lf.LoggerName = uintptr(unsafe.Pointer(logname))
	lf.ProcessTraceMode = processTraceModeRealTime | processTraceModeEventRecord
	lf.EventRecordCallback = eventRecordCallback()

	active.Store(s)
	trace, err := openTrace(&lf)
	runtime.KeepAlive(logname)
	if err != nil {
		active.Store(nil)
		_ = stopByName(name)
		return nil, fmt.Errorf("netstat: open trace: %w", err)
	}
	s.trace = trace

	go s.run()
	return s, nil
}

// run pumps events until the session stops. ProcessTrace calls back on the
// thread that invoked it, so that thread is pinned for the duration.
func (s *Session) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.done)
	s.procErr = processTrace(s.trace)
}

// Stop ends the session. Events buffered but not yet delivered are lost, which
// is the correct outcome for a daemon on its way out.
func (s *Session) Stop() error {
	var err error
	s.stopOnce.Do(func() {
		// Stopping the session ends the real-time stream, which makes
		// ProcessTrace return; closing the consumer handle afterwards releases
		// it. The order matters: closed first, ProcessTrace can hang in some
		// releases.
		err = controlTraceStop(s.handle, s.name)
		if cerr := closeTrace(s.trace); err == nil {
			err = cerr
		}
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			if err == nil {
				err = errors.New("netstat: trace thread did not exit after the session was stopped")
			}
		}
		active.CompareAndSwap(s, nil)
	})
	return err
}

// Lost reports how many events the kernel has dropped because the session's
// buffers were full. A nonzero figure means the byte counts are low.
func (s *Session) Lost() (uint32, error) {
	props, keep := newProperties()
	err := controlTrace(s.handle, "", props, eventTraceControlQuery)
	runtime.KeepAlive(keep)
	if err != nil {
		return 0, err
	}
	return props.EventsLost + props.RealTimeBuffersLost, nil
}

func controlTraceStop(handle uint64, name string) error {
	props, keep := newProperties()
	err := controlTrace(handle, name, props, eventTraceControlStop)
	runtime.KeepAlive(keep)
	return err
}

// stopByName stops a session this process does not hold a handle for.
func stopByName(name string) error {
	return controlTraceStop(0, name)
}
