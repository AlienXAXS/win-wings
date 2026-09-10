//go:build windows

package netstat

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The ETW structures are declared by hand. A wrong size does not fail loudly;
// it makes OpenTrace reject the structure with an invalid parameter, or puts
// the callback pointer in the wrong slot.
func TestStructLayouts(t *testing.T) {
	check := func(name string, got, want uintptr) {
		t.Helper()
		if got != want {
			t.Errorf("sizeof(%s) = %d, want %d", name, got, want)
		}
	}
	check("WNODE_HEADER", unsafe.Sizeof(wnodeHeader{}), 48)
	check("EVENT_TRACE_PROPERTIES", unsafe.Sizeof(eventTraceProperties{}), sizeofEventTraceProperties)
	check("EVENT_HEADER", unsafe.Sizeof(eventHeader{}), 80)
	check("EVENT_RECORD", unsafe.Sizeof(eventRecord{}), sizeofEventRecord)
	check("EVENT_TRACE_HEADER", unsafe.Sizeof(eventTraceHeader{}), 48)
	check("EVENT_TRACE", unsafe.Sizeof(eventTrace{}), 88)
	check("TRACE_LOGFILE_HEADER", unsafe.Sizeof(traceLogfileHeader{}), 280)
	check("EVENT_TRACE_LOGFILEW", unsafe.Sizeof(eventTraceLogfile{}), sizeofEventTraceLogfile)

	// The callback pointer is the field that matters most.
	check("EVENT_TRACE_LOGFILEW.EventRecordCallback offset",
		unsafe.Offsetof(eventTraceLogfile{}.EventRecordCallback), 424)
	check("EVENT_RECORD.UserData offset", unsafe.Offsetof(eventRecord{}.UserData), 96)
}

// startTestSession starts a session or skips the test when the account cannot.
func startTestSession(t *testing.T) (*Session, *Accountant) {
	t.Helper()
	acct := NewAccountant()
	s, err := StartSession("winwings-network-test", acct)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Skipf("cannot start an ETW session as this user; add it to Performance "+
				"Log Users or run elevated: %v", err)
		}
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return s, acct
}

// This is the test that decides whether the approach works at all. It sends
// real datagrams between two sockets in this process and checks that the
// kernel attributes both directions to this process's PID, with byte counts
// that cover the payload. In particular it settles whether receive events
// carry the socket owner's PID rather than that of whatever thread the stack
// was running on when the packet arrived.
func TestSessionAttributesUDPToOwningProcess(t *testing.T) {
	_, acct := startTestSession(t)

	pid := uint32(os.Getpid())
	acct.Claim("me", []uint32{pid})

	a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	const (
		count   = 200
		payload = 512
		want    = count * payload
	)
	buf := make([]byte, payload)
	recv := make([]byte, 2048)
	_ = b.SetReadDeadline(time.Now().Add(5 * time.Second))
	received := 0
	for i := 0; i < count; i++ {
		if _, err := a.WriteToUDP(buf, b.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
		n, _, err := b.ReadFromUDP(recv)
		if err != nil {
			t.Fatal(err)
		}
		received += n
	}
	if received != want {
		t.Fatalf("socket pair moved %d bytes, want %d", received, want)
	}

	// Events arrive on the flush timer, which is one second.
	deadline := time.Now().Add(6 * time.Second)
	var got Counters
	for time.Now().Before(deadline) {
		got = acct.Totals("me")
		if got.Tx >= want && got.Rx >= want {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.Tx < want {
		t.Errorf("tx attributed to this process = %d, want at least %d", got.Tx, want)
	}
	if got.Rx < want {
		t.Errorf("rx attributed to this process = %d, want at least %d", got.Rx, want)
	}
	t.Logf("attributed rx=%d tx=%d for %d bytes each way", got.Rx, got.Tx, want)
}

func TestSessionAttributesTCPToOwningProcess(t *testing.T) {
	_, acct := startTestSession(t)

	pid := uint32(os.Getpid())
	acct.Claim("me", []uint32{pid})

	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const want = 256 * 1024
	done := make(chan int, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- -1
			return
		}
		defer c.Close()
		total := 0
		buf := make([]byte, 32*1024)
		for total < want {
			n, err := c.Read(buf)
			total += n
			if err != nil {
				break
			}
		}
		done <- total
	}()

	c, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(make([]byte, want)); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if n := <-done; n != want {
		t.Fatalf("received %d bytes, want %d", n, want)
	}

	deadline := time.Now().Add(6 * time.Second)
	var got Counters
	for time.Now().Before(deadline) {
		got = acct.Totals("me")
		if got.Tx >= want && got.Rx >= want {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.Tx < want {
		t.Errorf("tx = %d, want at least %d", got.Tx, want)
	}
	if got.Rx < want {
		t.Errorf("rx = %d, want at least %d", got.Rx, want)
	}
	t.Logf("attributed rx=%d tx=%d for %d bytes", got.Rx, got.Tx, want)
}

// A session left behind by a crashed daemon must not stop the next one.
func TestStartSessionReplacesStaleSession(t *testing.T) {
	const name = "winwings-network-test"
	s, err := StartSession(name, NewAccountant())
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Skipf("cannot start an ETW session as this user: %v", err)
		}
		t.Fatalf("StartSession: %v", err)
	}

	// Simulate the crash: drop the consumer and forget the session without
	// stopping it. The kernel object stays behind.
	_ = closeTrace(s.trace)
	<-s.done
	active.Store(nil)

	s2, err := StartSession(name, NewAccountant())
	if err != nil {
		_ = stopByName(name)
		t.Fatalf("second StartSession over a stale session: %v", err)
	}
	if err := s2.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	// Nothing should be left.
	if err := stopByName(name); err == nil {
		t.Error("a session was still running after Stop")
	}
}
