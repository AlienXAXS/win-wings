//go:build windows

package winproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The helper process. `set /p` and PowerShell both fall back to the redirected
// handle when there is one, so neither can distinguish the console input buffer
// from stdin. This reads CONIN$ explicitly, which is what a server opening its
// own console does, and is the only way to make the distinction the test is
// about.
const (
	consoleReaderEnv = "WINPROC_TEST_CONSOLE_READER"
	consoleReaderOut = "WINPROC_TEST_CONSOLE_OUT"
)

func TestMain(m *testing.M) {
	if os.Getenv(consoleReaderEnv) == "1" {
		os.Exit(runConsoleReader(os.Getenv(consoleReaderOut)))
	}
	os.Exit(m.Run())
}

func runConsoleReader(out string) int {
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return 2
	}
	h, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		_ = os.WriteFile(out, []byte("OPEN-FAILED: "+err.Error()), 0o600)
		return 3
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, 256)
	var read uint32
	if err := windows.ReadConsole(h, &buf[0], uint32(len(buf)), &read, nil); err != nil {
		_ = os.WriteFile(out, []byte("READ-FAILED: "+err.Error()), 0o600)
		return 4
	}
	line := strings.TrimSpace(windows.UTF16ToString(buf[:read]))
	_ = os.WriteFile(out, []byte("["+line+"]"), 0o600)
	return 0
}

// TestWriteConsoleLineReachesAProcessThatIgnoresStdin is the whole point of the
// mechanism: a server that reads its console rather than its stdin cannot be
// reached by anything written to the pipe, and this proves the console input
// buffer reaches it anyway.
func TestWriteConsoleLineReachesAProcessThatIgnoresStdin(t *testing.T) {
	if !UseOwnConsole() {
		t.Skip("could not take a private console")
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "got.txt")

	p, err := Start(Config{
		Argv: []string{self},
		Dir:  t.TempDir(),
		Env: append(os.Environ(),
			consoleReaderEnv+"=1",
			consoleReaderOut+"="+out),
	}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = p.Kill()
		_ = p.Close()
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := p.Output().Read(buf); err != nil {
				return
			}
		}
	}()

	// Let the child reach its read before typing at it.
	time.Sleep(1200 * time.Millisecond)

	// A stdin write first, which this child cannot see. If the assertion below
	// passed on the strength of this, the test would be proving nothing.
	if _, err := p.Stdin().Write([]byte("FROM-STDIN\r\n")); err != nil {
		t.Logf("stdin write failed (not fatal, it is the negative control): %v", err)
	}
	if err := WriteConsoleLine("FROM-CONSOLE"); err != nil {
		t.Fatalf("WriteConsoleLine: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			got = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	switch {
	case got == "":
		t.Fatal("the child never completed its read; nothing reached the console input buffer")
	case strings.Contains(got, "FROM-CONSOLE"):
		t.Logf("the child read %q from the console input buffer", got)
	case strings.Contains(got, "FROM-STDIN"):
		t.Fatalf("the child read stdin (%q), so this child does not exercise the console path", got)
	default:
		t.Fatalf("the child reported %q", got)
	}
}
