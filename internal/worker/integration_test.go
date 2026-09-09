//go:build windows

package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pterodactyl/wings/internal/wire"
)

var workerExe string

// TestMain builds the worker binary once so the integration tests exercise the
// real detached-process path rather than an in-process shortcut.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "winwings-worker-build")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	workerExe = filepath.Join(dir, "winwings-worker.exe")
	cmd := exec.Command("go", "build", "-o", workerExe, "github.com/pterodactyl/wings/cmd/winwings-worker")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("building worker binary: " + err.Error() + "\n" + string(out))
	}

	os.Exit(m.Run())
}

func comspec() string {
	if c := os.Getenv("COMSPEC"); c != "" {
		return c
	}
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
}

// collector accumulates the events a daemon would receive.
type collector struct {
	mu      sync.Mutex
	console strings.Builder
	states  []wire.ProcessState
	exits   []wire.Exit
	stats   []wire.Stats
	logs    []wire.Log
}

func (c *collector) handlers() Handlers {
	return Handlers{
		Console: func(p wire.Console) {
			c.mu.Lock()
			c.console.Write(p.Data)
			c.mu.Unlock()
		},
		State: func(p wire.State) {
			c.mu.Lock()
			c.states = append(c.states, p.State)
			c.mu.Unlock()
		},
		Exit: func(p wire.Exit) {
			c.mu.Lock()
			c.exits = append(c.exits, p)
			c.mu.Unlock()
		},
		Stats: func(p wire.Stats) {
			c.mu.Lock()
			c.stats = append(c.stats, p)
			c.mu.Unlock()
		},
		Log: func(p wire.Log) {
			c.mu.Lock()
			c.logs = append(c.logs, p)
			c.mu.Unlock()
		},
	}
}

// logText renders the worker's own diagnostics the way an operator reading the
// wings log sees them, so a test can assert on what was actually reported.
func (c *collector) logText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, l := range c.logs {
		b.WriteString(string(l.Level))
		b.WriteString(" ")
		b.WriteString(l.Message)
		for k, v := range l.Fields {
			b.WriteString(" " + k + "=" + v)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (c *collector) consoleText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.console.String()
}

func (c *collector) exitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.exits)
}

// startWorker spins up a real detached worker and returns a connected client.
func startWorker(t *testing.T, uuid string) (*Client, *collector, string) {
	t.Helper()

	base := t.TempDir()
	instanceDir := filepath.Join(base, "instance")
	workingDir := filepath.Join(base, "volume")
	if err := os.MkdirAll(workingDir, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		UUID:            uuid,
		Token:           "test-token-" + uuid,
		WorkingDir:      workingDir,
		LogPath:         filepath.Join(instanceDir, "console.log"),
		ConsoleBacklog:  256,
		MaxLogSizeMB:    1,
		MaxLogFiles:     1,
		StatsIntervalMS: 300,
	}
	if err := WriteConfig(instanceDir, cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := SpawnWorker(ctx, workerExe, instanceDir, uuid, 15*time.Second); err != nil {
		t.Fatalf("SpawnWorker: %v", err)
	}

	col := &collector{}
	c, err := Dial(ctx, uuid, cfg.Token, 0, col.handlers())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	t.Cleanup(func() {
		_ = c.Shutdown()
		time.Sleep(300 * time.Millisecond)
		_ = c.Close()
	})

	return c, col, instanceDir
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWorkerLifecycle(t *testing.T) {
	c, col, instanceDir := startWorker(t, "lifecycle-test")

	if got := c.Hello().State; got != wire.StateOffline {
		t.Errorf("initial state = %q, want offline", got)
	}

	if err := c.Start(wire.Start{
		Argv:   []string{comspec()},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// cmd.exe prints a banner as soon as it starts.
	waitFor(t, 10*time.Second, "console output", func() bool {
		return len(col.consoleText()) > 0
	})
	t.Logf("console after start: %q", truncate(col.consoleText(), 200))

	// The stdin path is what every command-stop egg depends on.
	if err := c.Stdin([]byte("echo WORKER-E2E-MARKER\r\n")); err != nil {
		t.Fatalf("Stdin: %v", err)
	}
	waitFor(t, 10*time.Second, "stdin echo", func() bool {
		return strings.Contains(col.consoleText(), "WORKER-E2E-MARKER")
	})

	s, err := c.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	t.Logf("stats: mem=%d procs=%d cpu=%.2f uptime=%dms",
		s.MemoryBytes, s.Processes, s.CpuAbsolute, s.UptimeMillis)
	if s.Processes == 0 {
		t.Error("expected at least one process in the job")
	}
	if s.UptimeMillis <= 0 {
		t.Error("expected a positive uptime")
	}

	// Graceful stop by stdin command, the primary path.
	if err := c.Stop(wire.Stop{
		Mode:           wire.StopCommand,
		Value:          "exit",
		TimeoutSeconds: 10,
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitFor(t, 15*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})

	col.mu.Lock()
	exit := col.exits[0]
	states := append([]wire.ProcessState(nil), col.states...)
	col.mu.Unlock()

	t.Logf("exit: code=%d terminated=%v memLimit=%v", exit.Code, exit.Terminated, exit.MemoryLimitHit)
	t.Logf("state transitions: %v", states)

	if exit.Terminated {
		t.Error("a graceful stdin stop should not report as terminated")
	}

	// The console log must exist and hold what we saw.
	logBytes, err := os.ReadFile(filepath.Join(instanceDir, "console.log"))
	if err != nil {
		t.Fatalf("read console log: %v", err)
	}
	if !strings.Contains(string(logBytes), "WORKER-E2E-MARKER") {
		t.Error("console log does not contain the echoed marker")
	}
}

func TestWorkerTerminate(t *testing.T) {
	c, col, _ := startWorker(t, "terminate-test")

	if err := c.Start(wire.Start{
		Argv:   []string{comspec(), "/c", "ping -n 600 127.0.0.1 > nul"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 10*time.Second, "running state", func() bool {
		col.mu.Lock()
		defer col.mu.Unlock()
		for _, s := range col.states {
			if s == wire.StateRunning {
				return true
			}
		}
		return false
	})

	if err := c.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	waitFor(t, 15*time.Second, "exit after terminate", func() bool {
		return col.exitCount() > 0
	})

	col.mu.Lock()
	exit := col.exits[0]
	col.mu.Unlock()

	if !exit.Terminated {
		t.Error("expected the exit to be reported as terminated")
	}
	t.Logf("terminate exit: code=%d terminated=%v", exit.Code, exit.Terminated)
}

// A daemon restart must not disturb a running server, and must be able to
// recover the console output it missed. This is the entire reason the worker
// exists as a separate process.
func TestWorkerSurvivesDaemonReconnect(t *testing.T) {
	c, col, _ := startWorker(t, "reconnect-test")

	if err := c.Start(wire.Start{
		Argv:   []string{comspec()},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 10*time.Second, "console output", func() bool {
		return len(col.consoleText()) > 0
	})

	if err := c.Stdin([]byte("echo BEFORE-DISCONNECT\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "pre-disconnect marker", func() bool {
		return strings.Contains(col.consoleText(), "BEFORE-DISCONNECT")
	})

	// Drop the connection, as a daemon restart would.
	_ = c.Close()
	time.Sleep(500 * time.Millisecond)

	// Reconnect from sequence 0 to request the full retained backlog.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	col2 := &collector{}
	c2, err := Dial(ctx, "reconnect-test", "test-token-reconnect-test", 0, col2.handlers())
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer func() {
		_ = c2.Shutdown()
		time.Sleep(300 * time.Millisecond)
		_ = c2.Close()
	}()

	hello := c2.Hello()
	t.Logf("after reconnect: state=%q pid=%d seq=%d", hello.State, hello.PID, hello.Sequence)

	if hello.State != wire.StateRunning {
		t.Errorf("server should still be running after the daemon reconnects, got %q", hello.State)
	}
	if hello.PID == 0 {
		t.Error("expected a live PID after reconnect")
	}

	waitFor(t, 10*time.Second, "replayed backlog", func() bool {
		return strings.Contains(col2.consoleText(), "BEFORE-DISCONNECT")
	})
	t.Log("console backlog was replayed to the reconnected daemon")

	// And the server must still be controllable.
	if err := c2.Stdin([]byte("echo AFTER-RECONNECT\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "post-reconnect marker", func() bool {
		return strings.Contains(col2.consoleText(), "AFTER-RECONNECT")
	})
}

func TestWorkerRejectsBadToken(t *testing.T) {
	_, _, _ = startWorker(t, "auth-test")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := Dial(ctx, "auth-test", "wrong-token", 0, Handlers{}); err == nil {
		t.Fatal("expected the handshake to be rejected with a bad token")
	} else {
		t.Logf("rejected as expected: %v", err)
	}
}

func TestWorkerUpdateLimits(t *testing.T) {
	c, _, _ := startWorker(t, "limits-test")

	if err := c.Start(wire.Start{
		Argv:   []string{comspec(), "/c", "ping -n 600 127.0.0.1 > nul"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 256 << 20},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 10*time.Second, "process running", func() bool {
		s, err := c.Stats()
		return err == nil && s.Processes > 0
	})

	// In-situ limit change, the equivalent of upstream's container update.
	if err := c.UpdateLimits(wire.Limits{
		ProcessLimit: 64,
		MemoryBytes:  512 << 20,
		CpuRate:      2500,
		CpuHardCap:   true,
	}); err != nil {
		t.Errorf("UpdateLimits on a running process: %v", err)
	}

	_ = c.Terminate()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestStopEscalationIsLogged drives a process that ignores its stop command all
// the way down to the kill, and asserts the worker narrated each step. Without
// this the only evidence of how a stop went is that the process is gone.
func TestStopEscalationIsLogged(t *testing.T) {
	c, col, _ := startWorker(t, "stop-logging-test")

	// ping ignores its stdin entirely, so the stop command is delivered and
	// has no effect -- exactly the case that has to escalate. (pause is no
	// good here: any keystroke, including a stop command, ends it.)
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

	if err := c.Stop(wire.Stop{
		Mode:           wire.StopCommand,
		Value:          "this-is-not-a-stop-command",
		TimeoutSeconds: 2,
	}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitFor(t, 30*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})

	logs := col.logText()
	t.Logf("worker diagnostics:\n%s", logs)

	for _, want := range []string{
		"stopping the server process",
		"mode=command",
		"command=this-is-not-a-stop-command",
		"timeout=2s",
		"writing the stop command to the process's stdin",
		"the process is still running; escalating",
		"sending ctrl+c to the server's console",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("stop diagnostics never mentioned %q", want)
		}
	}

	// The escalation may end at ctrl+c, at ctrl+break, or at the kill, depending
	// on what this particular child honours. What matters is that the log says
	// which, rather than leaving it to be inferred from the process being gone.
	if !strings.Contains(logs, "killing the job object") &&
		!strings.Contains(logs, "after=ctrl+c") &&
		!strings.Contains(logs, "after=ctrl+break") {
		t.Error("stop diagnostics never reported how the process finally went away")
	}

	// The elapsed time is the number an operator needs to tell "it hung" from
	// "the egg's timeout is too short", so it must actually be reported.
	if !strings.Contains(logs, "elapsed=") {
		t.Error("stop diagnostics never reported how long the stop took")
	}
}
