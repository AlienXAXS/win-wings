//go:build windows

package winproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCtrlCStopsAProcessInAPseudoConsole is the whole justification for the
// ctrl+c stop mode: Windows has no signal to send, so this proves the 0x03
// byte is genuinely translated into an interrupt by a real console driver
// rather than arriving at the process as input.
func TestCtrlCStopsAProcessInAPseudoConsole(t *testing.T) {
	exe := os.Getenv("COMSPEC")
	if exe == "" {
		exe = `C:\Windows\System32\cmd.exe`
	}

	// A ping loop is a long-running console process that ignores its stdin, so
	// nothing but a real interrupt will end it early.
	p, err := Start(Config{
		Argv:          []string{exe, "/c", "ping", "-n", "600", "127.0.0.1"},
		Dir:           t.TempDir(),
		Env:           os.Environ(),
		PseudoConsole: true,
		Cols:          120,
		Rows:          30,
	}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "0xc0000142") {
			t.Skip("conpty loader failure; see conpty_diag_test.go")
		}
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	// Drain, or the console output pipe fills and the child blocks. The output
	// is also the only proof the child actually got as far as running: a ConPTY
	// child that dies in the loader exits on its own, and a test that skipped
	// straight to the interrupt would call that a pass.
	sawOutput := make(chan struct{})
	go func() {
		var once bool
		buf := make([]byte, 4096)
		for {
			n, err := p.Output().Read(buf)
			if n > 0 && !once {
				once = true
				close(sawOutput)
			}
			if err != nil {
				return
			}
		}
	}()

	exited := make(chan uint32, 1)
	go func() {
		code, _ := p.Wait()
		exited <- code
	}()

	select {
	case <-sawOutput:
	case code := <-exited:
		if code == 0xC0000142 {
			// The pseudo console does not work on the machine this was written
			// on, for reasons bisected at length in conpty_diag_test.go and most
			// likely to do with the session these tests run in. This assertion is
			// therefore only meaningful on a real Windows host -- which is where
			// it matters, since that is where the servers run.
			t.Skip("conpty loader failure (0xC0000142) before the child ran; " +
				"see conpty_diag_test.go. Run this on a real Windows host")
		}
		t.Fatalf("the child exited with 0x%X before printing anything", code)
	case <-time.After(15 * time.Second):
		_ = p.Kill()
		t.Fatal("the child never produced output; nothing was proven about ctrl+c")
	}

	// Output is not quite enough on its own -- the child has to have installed
	// its console before an interrupt means anything to it.
	time.Sleep(500 * time.Millisecond)

	if err := p.CtrlC(); err != nil {
		t.Fatalf("CtrlC: %v", err)
	}

	select {
	case code := <-exited:
		t.Logf("process exited with 0x%X after ctrl+c", code)
		// STATUS_CONTROL_C_EXIT. ping installs no handler, so the console driver
		// tears it down itself -- which is only possible if what arrived was a
		// genuine CTRL_C_EVENT and not the byte 0x03 as ordinary input. A server
		// that does handle the interrupt exits however it likes; this child is
		// chosen precisely because it does not.
		if code != 0xC000013A {
			t.Errorf("exit code is 0x%X, want STATUS_CONTROL_C_EXIT (0xC000013A); "+
				"the process stopped, but not because of an interrupt", code)
		}
	case <-time.After(15 * time.Second):
		_ = p.Kill()
		t.Fatal("ctrl+c did not stop the process; it is not being delivered as an interrupt")
	}
}

// TestCtrlCInterruptsAProcessSharingTheWorkersConsole is the mechanism the stop
// path actually uses. No pseudo console is involved: the child inherits the
// console this process owns, and the interrupt is raised on that console.
//
// The assertion is the exit code. STATUS_CONTROL_C_EXIT is only produced by the
// console driver tearing down a process that received a real CTRL_C_EVENT, so
// it cannot be reached by the child merely exiting on its own.
func TestCtrlCInterruptsAProcessSharingTheWorkersConsole(t *testing.T) {
	// Take a private console first. Raising an interrupt on an inherited one
	// would deliver it to the test runner and the shell that started it, which
	// is not a test failure so much as a small act of vandalism.
	if !UseOwnConsole() {
		t.Skip("could not take a private console")
	}

	// ping ignores stdin and installs no Ctrl+C handler, so the console driver
	// terminates it and the exit code is unambiguous. Run directly rather than
	// through cmd.exe, which absorbs the interrupt on its child's behalf.
	exe := filepath.Join(os.Getenv("SystemRoot"), "System32", "PING.EXE")
	p, err := Start(Config{
		Argv: []string{exe, "-n", "30", "127.0.0.1"},
		Dir:  t.TempDir(),
		Env:  os.Environ(),
	}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := p.Output().Read(buf); err != nil {
				return
			}
		}
	}()

	exited := make(chan uint32, 1)
	go func() {
		code, _ := p.Wait()
		exited <- code
	}()

	// Let the child attach to the console before interrupting it.
	time.Sleep(1200 * time.Millisecond)

	if err := p.CtrlC(); err != nil {
		t.Fatalf("CtrlC: %v", err)
	}

	select {
	case code := <-exited:
		if code != 0xC000013A {
			t.Errorf("exit code is 0x%X, want STATUS_CONTROL_C_EXIT (0xC000013A)", code)
		}
	case <-time.After(10 * time.Second):
		_ = p.Kill()
		t.Fatal("ctrl+c did not reach the process")
	}
}

// TestCtrlCNeedsAConsole covers the failure the worker warns about at startup:
// with no console at all there is nothing to raise an interrupt on.
func TestCtrlCNeedsAConsole(t *testing.T) {
	if hasConsole() {
		t.Skip("this process has a console; the no-console path cannot be exercised here")
	}
	p := &Process{}
	err := p.CtrlC()
	if err == nil || !strings.Contains(err.Error(), "no console") {
		t.Errorf("error is %v, want it to name the missing console", err)
	}
}
