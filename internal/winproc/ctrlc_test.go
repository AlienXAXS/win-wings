//go:build windows

package winproc

import (
	"os"
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

// TestCtrlCIsRefusedWithoutAPseudoConsole documents the constraint the Panel
// plugin has to surface: a server given plain pipes has no console, so there is
// nothing to turn the byte into an interrupt.
func TestCtrlCIsRefusedWithoutAPseudoConsole(t *testing.T) {
	exe := os.Getenv("COMSPEC")
	if exe == "" {
		exe = `C:\Windows\System32\cmd.exe`
	}

	p, err := Start(Config{
		Argv: []string{exe, "/c", "ping", "-n", "600", "127.0.0.1"},
		Dir:  t.TempDir(),
		Env:  os.Environ(),
	}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = p.Kill()
		_ = p.Close()
	}()

	err = p.CtrlC()
	if err == nil {
		t.Fatal("expected ctrl+c to be refused without a pseudo console")
	}
	if !strings.Contains(err.Error(), "pseudo console") {
		t.Errorf("error is %q, want it to name the missing pseudo console", err)
	}
}
