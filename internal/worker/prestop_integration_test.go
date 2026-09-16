//go:build windows

package worker

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pterodactyl/wings/internal/wire"
)

// A server that ignores its stdin, so that only the pre-stop command can bring
// it down gracefully.
func startDeafServer(t *testing.T, name string) (*Client, *collector) {
	t.Helper()
	c, col, _ := startWorker(t, name)
	if err := c.Start(wire.Start{
		Argv:   []string{comspec(), "/c", "ping", "-n", "600", "127.0.0.1"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 10*time.Second, "console output", func() bool {
		return len(col.consoleText()) > 0
	})
	return c, col
}

// The case the feature exists for: the egg's own script stops the server, and
// the generic mechanism is never reached.
func TestPreStopScriptStopsTheServer(t *testing.T) {
	c, col := startDeafServer(t, "prestop-stops")

	if err := c.Stop(wire.Stop{
		Mode:           wire.StopCommand,
		Value:          "this-is-not-a-stop-command",
		TimeoutSeconds: 10,
		PreStop: &wire.PreStopCommand{
			// SERVER_PID is the worker's addition to the environment it was
			// given; the script is expected to lean on it.
			Argv:  []string{comspec(), "/c", "echo PRESTOP-RAN && taskkill /F /T /PID %SERVER_PID%"},
			Label: "the test's pre-stop step",
			Env:   os.Environ(),
		},
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitFor(t, 20*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})

	if out := col.consoleText(); !strings.Contains(out, "PRESTOP-RAN") {
		t.Errorf("the pre-stop command's output never reached the console: %q", out)
	}

	logs := col.logText()
	t.Logf("worker diagnostics:\n%s", logs)
	for _, want := range []string{
		"running the pre-stop command before asking the server to stop",
		"command=the test's pre-stop step",
		"the server exited during the pre-stop command",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("stop diagnostics never mentioned %q", want)
		}
	}
	for _, unwanted := range []string{
		"sending the stop command",
		"sending ctrl+c",
		"killing the job object",
	} {
		if strings.Contains(logs, unwanted) {
			t.Errorf("the stop escalated past a pre-stop command that had worked: %q", unwanted)
		}
	}

	col.mu.Lock()
	exit := col.exits[0]
	col.mu.Unlock()
	if exit.Terminated {
		t.Error("a server stopped by its pre-stop script should not report as terminated")
	}
}

// A pre-stop command that runs and does not stop the server must leave the
// ordinary stop to follow it, in that order.
func TestPreStopScriptFallsThroughToTheStopMechanism(t *testing.T) {
	c, col := startDeafServer(t, "prestop-fallthrough")

	if err := c.Stop(wire.Stop{
		Mode:           wire.StopCommand,
		Value:          "this-is-not-a-stop-command",
		TimeoutSeconds: 2,
		PreStop: &wire.PreStopCommand{
			Argv:  []string{comspec(), "/c", "echo PRESTOP-RAN"},
			Label: "the test's harmless step",
			Env:   os.Environ(),
		},
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitFor(t, 30*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})

	if out := col.consoleText(); !strings.Contains(out, "PRESTOP-RAN") {
		t.Errorf("the pre-stop command's output never reached the console: %q", out)
	}

	logs := col.logText()
	t.Logf("worker diagnostics:\n%s", logs)
	finished := strings.Index(logs, "the pre-stop command finished")
	sent := strings.Index(logs, "sending the stop command")
	if finished < 0 || sent < 0 {
		t.Fatalf("stop diagnostics should mention the pre-stop finishing and then the stop command; finished=%d sent=%d", finished, sent)
	}
	if finished > sent {
		t.Error("the stop command was sent before the pre-stop command had finished")
	}
}

// A pre-stop command that hangs is killed at its timeout and the stop carries on
// without it, rather than the server becoming impossible to stop.
func TestPreStopScriptIsKilledAtItsTimeout(t *testing.T) {
	c, col := startDeafServer(t, "prestop-timeout")

	started := time.Now()
	if err := c.Stop(wire.Stop{
		Mode:           wire.StopTerminate,
		TimeoutSeconds: 10,
		PreStop: &wire.PreStopCommand{
			Argv:           []string{comspec(), "/c", "ping", "-n", "120", "127.0.0.1"},
			Label:          "the test's hung step",
			Env:            os.Environ(),
			TimeoutSeconds: 2,
		},
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitFor(t, 30*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})
	took := time.Since(started)

	logs := col.logText()
	t.Logf("worker diagnostics (stop took %s):\n%s", took, logs)
	for _, want := range []string{
		"the pre-stop command did not finish in time; killing it",
		"timeout=2s",
		"killing the job object",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("stop diagnostics never mentioned %q", want)
		}
	}
	// The hung script was a 120s ping. If it was not actually killed the stop
	// would have waited it out.
	if took > 20*time.Second {
		t.Errorf("the stop took %s; the hung pre-stop command was not cut short", took)
	}

	// A killed pre-stop must not leave the worker unusable.
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

// A stop that arrives while a pre-start command is still running has no server
// to talk to, so the pre-stop command is skipped rather than run against
// whatever is there.
func TestPreStopScriptIsSkippedDuringPreStart(t *testing.T) {
	c, col, _ := startWorker(t, "prestop-during-prestart")

	if err := c.Start(wire.Start{
		PreStart: []wire.PreStartCommand{
			{Argv: []string{comspec(), "/c", "ping", "-n", "30", "127.0.0.1"}, Label: "the test's slow step"},
		},
		Argv:   []string{comspec(), "/c", "echo SERVER-RAN"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 10*time.Second, "the pre-start command to be running", func() bool {
		return strings.Contains(col.logText(), "running a pre-start command")
	})

	if err := c.Stop(wire.Stop{
		Mode:           wire.StopTerminate,
		TimeoutSeconds: 5,
		PreStop: &wire.PreStopCommand{
			Argv:  []string{comspec(), "/c", "echo PRESTOP-RAN"},
			Label: "the test's pre-stop step",
			Env:   os.Environ(),
		},
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitFor(t, 15*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})

	if !strings.Contains(col.logText(), "skipping the pre-stop command; the server has not started yet") {
		t.Errorf("the worker did not say it skipped the pre-stop command:\n%s", col.logText())
	}
	if out := col.consoleText(); strings.Contains(out, "PRESTOP-RAN") {
		t.Errorf("the pre-stop command ran although the server had not started: %q", out)
	}
}
