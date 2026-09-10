//go:build windows

// Package netstat produces per-server network byte counts on Windows.
//
// Docker gave upstream wings a network namespace per container, and with it an
// interface whose counters were the server's alone. Windows has no equivalent
// without containers. What it does have is Event Tracing for Windows: the
// TCP/IP stack reports every send and receive, with the owning process and the
// byte count, to any session that asks. This package runs one such session for
// the daemon and adds the bytes up per server, using the process lists the
// workers report.
//
// The figures differ from Docker's in two ways worth knowing. They count
// transport payload rather than bytes on the wire, so they run a few percent
// below an interface counter. And under sustained heavy load the kernel can
// drop events if the session's buffers fill, which reads as a low figure rather
// than an error; Lost reports whether that has happened.
package netstat

import (
	"sync"
)

// SessionName is the ETW session the daemon runs. Names are host-global, so it
// is specific enough not to collide with anything an operator might start.
const SessionName = "winwings-network"

var (
	mu      sync.Mutex
	session *Session
	acct    = NewAccountant()
)

// Start begins the daemon's tracing session. Until it is called, or if it
// fails, the attribution functions still work but every total is zero.
func Start() error {
	mu.Lock()
	defer mu.Unlock()
	if session != nil {
		return nil
	}
	s, err := StartSession(SessionName, acct)
	if err != nil {
		return err
	}
	session = s
	return nil
}

// Stop ends the daemon's session. Safe to call when none is running.
func Stop() error {
	mu.Lock()
	s := session
	session = nil
	mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Stop()
}

// Enabled reports whether a session is collecting.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return session != nil
}

// Lost reports events dropped by the running session, or zero without one.
func Lost() uint32 {
	mu.Lock()
	s := session
	mu.Unlock()
	if s == nil {
		return 0
	}
	n, _ := s.Lost()
	return n
}

// Claim records that a server's job currently consists of pids.
func Claim(server string, pids []uint32) { acct.Claim(server, pids) }

// Totals returns a server's cumulative counters since its last Reset.
func Totals(server string) Counters { return acct.Totals(server) }

// Reset zeroes a server's counters, for the start of a new run.
func Reset(server string) { acct.Reset(server) }

// Release forgets a server.
func Release(server string) { acct.Release(server) }
