//go:build windows

package worker

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pterodactyl/wings/internal/wire"
)

func TestPreStartRunsBeforeTheServer(t *testing.T) {
	c, col, _ := startWorker(t, "prestart-order")

	if err := c.Start(wire.Start{
		PreStart: []wire.PreStartCommand{
			{Argv: []string{comspec(), "/c", "echo PRESTART-RAN"}, Label: "the test's first step"},
		},
		Argv:     []string{comspec(), "/c", "echo SERVER-RAN"},
		Env:      os.Environ(),
		Limits:   wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 20*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})

	out := col.consoleText()
	pre, srv := strings.Index(out, "PRESTART-RAN"), strings.Index(out, "SERVER-RAN")
	if pre < 0 || srv < 0 || pre > srv {
		t.Errorf("console = %q; want the pre-start output before the server's", out)
	}

	col.mu.Lock()
	states := append([]wire.ProcessState(nil), col.states...)
	exits := len(col.exits)
	col.mu.Unlock()
	t.Logf("state transitions: %v", states)

	if exits != 1 {
		t.Errorf("got %d exit events, want exactly one for the run", exits)
	}
	// The pre-start command ending must not look like the server ending.
	seenRunning := false
	for _, s := range states {
		if s == wire.StateRunning {
			seenRunning = true
		}
		if s == wire.StateOffline && !seenRunning {
			t.Errorf("went offline before running: %v", states)
			break
		}
	}
	if !seenRunning {
		t.Errorf("never reported running: %v", states)
	}
}

// Two pre-start commands must run in the order given, and the server after both.
// The chain is what a Steam egg with its own preparation script gets: the update
// rewrites the files, then the script patches one of them, then the game reads it.
func TestPreStartCommandsRunInOrder(t *testing.T) {
	c, col, _ := startWorker(t, "prestart-chain")

	if err := c.Start(wire.Start{
		PreStart: []wire.PreStartCommand{
			{Argv: []string{comspec(), "/c", "echo STEP-ONE"}, Label: "the first step"},
			// Empty argv is how the daemon says a step is not due this boot. It
			// must be skipped without taking the steps after it with it.
			{Label: "a step that is not due"},
			{Argv: []string{comspec(), "/c", "echo STEP-TWO"}, Label: "the second step"},
		},
		Argv:   []string{comspec(), "/c", "echo SERVER-RAN"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 25*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})

	out := col.consoleText()
	one, two, srv := strings.Index(out, "STEP-ONE"), strings.Index(out, "STEP-TWO"), strings.Index(out, "SERVER-RAN")
	if one < 0 || two < 0 || srv < 0 {
		t.Fatalf("console = %q; want all three steps", out)
	}
	if !(one < two && two < srv) {
		t.Errorf("console = %q; want STEP-ONE, then STEP-TWO, then the server", out)
	}
}

func TestStopDuringPreStartSkipsTheServer(t *testing.T) {
	c, col, _ := startWorker(t, "prestart-stop")

	if err := c.Start(wire.Start{
		// ping is the portable sleep on Windows: one probe per second.
		PreStart: []wire.PreStartCommand{
			{Argv: []string{comspec(), "/c", "ping", "-n", "30", "127.0.0.1"}, Label: "the test's slow step"},
		},
		Argv:     []string{comspec(), "/c", "echo SERVER-RAN"},
		Env:      os.Environ(),
		Limits:   wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 10*time.Second, "the pre-start command to be running", func() bool {
		return strings.Contains(col.logText(), "running a pre-start command")
	})

	if err := c.Stop(wire.Stop{Mode: wire.StopTerminate, TimeoutSeconds: 5}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitFor(t, 15*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})

	if out := col.consoleText(); strings.Contains(out, "SERVER-RAN") {
		t.Errorf("the server ran after a stop during pre-start: %q", out)
	}

	col.mu.Lock()
	exit := col.exits[0]
	states := append([]wire.ProcessState(nil), col.states...)
	col.mu.Unlock()
	t.Logf("state transitions: %v", states)

	if !exit.Terminated {
		t.Error("a terminated pre-start should report the run as terminated")
	}
	for _, s := range states {
		if s == wire.StateRunning {
			t.Errorf("reported running although the server was never started: %v", states)
		}
	}

	// The worker must be usable again afterwards.
	if err := c.Start(wire.Start{
		Argv:   []string{comspec(), "/c", "echo SECOND-RUN"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
	}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	waitFor(t, 15*time.Second, "second run output", func() bool {
		return strings.Contains(col.consoleText(), "SECOND-RUN")
	})
}
