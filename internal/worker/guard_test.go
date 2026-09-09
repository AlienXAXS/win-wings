//go:build windows

package worker

import (
	"strings"
	"testing"
)

// A panic inside a request handler must become an error, not an exit.
//
// The worker holds the server's job object, and that job kills its contents when
// it is released. A crash while handling a request therefore terminates a
// running server, which is a wildly disproportionate response to a bug in, say,
// the failure path of a start that was going to fail anyway.
func TestGuardTurnsAPanicIntoAnError(t *testing.T) {
	w := New(Config{UUID: "guard-test", WorkingDir: t.TempDir()})

	err := func() (err error) {
		defer w.guard("doing something", &err)
		var v *nilReceiver
		_ = v.deref()
		return nil
	}()

	if err == nil {
		t.Fatal("the panic did not surface as an error")
	}
	if !strings.Contains(err.Error(), "doing something") {
		t.Fatalf("error does not say what was being done: %v", err)
	}
	if !strings.Contains(err.Error(), "nil pointer") {
		t.Fatalf("error does not carry the panic: %v", err)
	}
}

// Guarding a goroutine that reports nowhere still has to swallow the panic.
func TestGuardWithNoDestinationStillRecovers(t *testing.T) {
	w := New(Config{UUID: "guard-test-nodst", WorkingDir: t.TempDir()})

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer w.guard("a background task", nil)
		panic("something went wrong")
	}()
	<-done
}

// A guard on a function that does not panic must not invent an error.
func TestGuardIsInertWithoutAPanic(t *testing.T) {
	w := New(Config{UUID: "guard-test-clean", WorkingDir: t.TempDir()})

	err := func() (err error) {
		defer w.guard("doing something", &err)
		return nil
	}()
	if err != nil {
		t.Fatalf("invented an error where nothing panicked: %v", err)
	}
}

// nilReceiver stands in for any value that can still be nil at the point
// deferred cleanup runs -- which is exactly how the crash this guards against
// happened.
type nilReceiver struct{ n int }

func (r *nilReceiver) deref() int { return r.n }
