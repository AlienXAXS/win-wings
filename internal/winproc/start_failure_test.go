//go:build windows

package winproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A failed start must return its error, not panic.
//
// Start used to declare a named result for the process and return nil for it on
// every failure path. That nil was assigned before the deferred cleanup ran, so
// the cleanup dereferenced it and panicked -- inside the worker, which meant a
// server that would not start took its worker down and destroyed the real error
// on the way. Any failure after the Process value is created reaches that
// cleanup; a missing executable is the cheapest way to get there.
func TestStartReturnsAnErrorRatherThanPanicking(t *testing.T) {
	dir := t.TempDir()

	_, err := Start(Config{
		Argv: []string{filepath.Join(dir, "no-such-program.exe")},
		Dir:  dir,
	}, nil)
	if err == nil {
		t.Fatal("expected an error for a missing executable")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error did not name the failing step: %v", err)
	}
}

// The same, reaching CreateProcess itself rather than failing the lookup first.
// This is the path the StarRupture start actually took.
func TestStartFailingInsideCreateProcessReturnsAnError(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "not-really-a-program.exe")
	if err := os.WriteFile(exe, []byte("this is not a PE image"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Start(Config{Argv: []string{exe}, Dir: dir}, nil)
	if err == nil {
		t.Fatal("expected an error for a file that is not an executable")
	}
	if !strings.Contains(err.Error(), "create process") {
		t.Fatalf("error did not name the failing step: %v", err)
	}
	if !strings.Contains(err.Error(), exe) {
		t.Fatalf("error does not name the path it tried: %v", err)
	}
}

// The same, with a pseudo console attached, which is the path that leaves the
// most behind to clean up.
func TestStartWithPseudoConsoleReturnsAnErrorRatherThanPanicking(t *testing.T) {
	dir := t.TempDir()

	_, err := Start(Config{
		Argv:          []string{filepath.Join(dir, "no-such-program.exe")},
		Dir:           dir,
		PseudoConsole: true,
		Cols:          80,
		Rows:          25,
	}, nil)
	if err == nil {
		t.Fatal("expected an error for a missing executable")
	}
}

// Cleanup on a value that was never populated must be a no-op.
func TestCloseAllIsNilTolerant(t *testing.T) {
	var p *Process
	p.closeAll()
}
