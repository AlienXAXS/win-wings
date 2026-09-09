//go:build windows

package windows

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/internal/accounts"
	"github.com/pterodactyl/wings/internal/winenv"
	"github.com/pterodactyl/wings/internal/winproc"
	"github.com/pterodactyl/wings/internal/wire"
	"github.com/pterodactyl/wings/internal/worker"
	"github.com/pterodactyl/wings/remote"
)

// WorkerExecutable is the name of the supervisor binary, expected alongside the
// daemon's own executable.
const WorkerExecutable = "winwings-worker.exe"

var (
	workerPathOnce sync.Once
	workerPath     string
	workerPathErr  error
)

// resolveWorkerPath locates the worker binary next to the running daemon.
func resolveWorkerPath() (string, error) {
	workerPathOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			workerPathErr = errors.Wrap(err, "environment/windows: cannot determine daemon path")
			return
		}
		candidate := filepath.Join(filepath.Dir(exe), WorkerExecutable)
		if _, err := os.Stat(candidate); err != nil {
			workerPathErr = errors.Wrapf(err,
				"environment/windows: %s not found next to the daemon at %s",
				WorkerExecutable, filepath.Dir(exe))
			return
		}
		workerPath = candidate
	})
	return workerPath, workerPathErr
}

// connect attaches to the worker already supervising this server.
//
// It never starts one. A worker outliving the daemon is the ordinary case, and
// attaching to it is what lets the daemon restart without disturbing servers: the
// worker reports the server still running and replays the console it buffered
// while nobody was listening. A server with no worker is simply offline, and
// saying so is this function's job — starting a process is Start's.
func (e *Environment) connect(ctx context.Context) (*worker.Client, error) {
	e.mu.RLock()
	if c := e.client; c != nil {
		e.mu.RUnlock()
		return c, nil
	}
	since := e.lastSeq
	e.mu.RUnlock()

	c, err := worker.Dial(ctx, e.Id, e.token, since, e.handlers())
	if err != nil {
		return nil, err
	}
	e.adoptClient(c)
	return c, nil
}

// connectOrSpawn attaches to this server's worker, starting one if there is none.
//
// Only a power action should reach this: everything else wants connect, which
// reports an absent worker rather than manufacturing one.
func (e *Environment) connectOrSpawn(ctx context.Context) (*worker.Client, error) {
	// Serialised, because two callers racing here would each find no worker and
	// each spawn one. Only one of them can own the control pipe; the other dies
	// on a name collision, and which of the two that is comes down to timing.
	e.spawnMu.Lock()
	defer e.spawnMu.Unlock()

	c, err := e.connect(ctx)
	if err == nil {
		return c, nil
	}

	// A worker that is listening but will not talk to us is a fault to report,
	// not a reason to start a second one. Named pipe names are exclusive: the
	// duplicate would fail its own listen with "Access is denied" and exit,
	// leaving the original in place and the daemon none the wiser.
	if worker.PipeExists(e.Id) {
		return nil, errors.WrapIf(err, "environment/windows: a worker for this server is "+
			"already listening but would not accept a connection; it was left alone rather "+
			"than starting a second one. Look in the instance directory's worker.log and "+
			"worker-stderr.log, or stop the stray winwings-worker process for this server")
	}

	exe, perr := resolveWorkerPath()
	if perr != nil {
		return nil, perr
	}
	if err := e.Create(); err != nil {
		return nil, err
	}
	if err := worker.SpawnWorker(ctx, exe, e.ServerRoot(), e.Id, 20*time.Second); err != nil {
		return nil, err
	}

	c, err = worker.Dial(ctx, e.Id, e.token, e.lastSequence(), e.handlers())
	if err != nil {
		return nil, errors.WrapIf(err, "environment/windows: failed to reach the worker after spawning it")
	}
	e.adoptClient(c)
	return c, nil
}

// lastSequence is the last console sequence number this daemon has seen.
func (e *Environment) lastSequence() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastSeq
}

// adoptClient records a connected client and syncs state from its handshake.
func (e *Environment) adoptClient(c *worker.Client) {
	hello := c.Hello()

	e.mu.Lock()
	e.client = c
	if hello.Sequence > e.lastSeq {
		e.lastSeq = hello.Sequence
	}
	if hello.StartedAt > 0 {
		e.startedAt = time.UnixMilli(hello.StartedAt)
	}
	e.mu.Unlock()

	// Adopt whatever state the worker reports, so a daemon restart does not
	// present a running server as offline.
	switch hello.State {
	case wire.StateRunning:
		e.SetState(environment.ProcessRunningState)
	case wire.StateStarting:
		e.SetState(environment.ProcessStartingState)
	case wire.StateStopping:
		e.SetState(environment.ProcessStoppingState)
	default:
		e.SetState(environment.ProcessOfflineState)
	}
}

// handlers wires worker notifications into the environment's event bus.
func (e *Environment) handlers() worker.Handlers {
	return worker.Handlers{
		Console: func(p wire.Console) {
			e.mu.Lock()
			if p.Sequence > e.lastSeq {
				e.lastSeq = p.Sequence
			}
			e.mu.Unlock()

			e.logCallbackMx.Lock()
			cb := e.logCallback
			e.logCallbackMx.Unlock()
			if cb != nil {
				cb(p.Data)
			}
		},

		State: func(p wire.State) {
			switch p.State {
			case wire.StateRunning:
				e.mu.Lock()
				if e.startedAt.IsZero() {
					e.startedAt = time.Now()
				}
				e.mu.Unlock()
				e.SetState(environment.ProcessRunningState)
			case wire.StateStarting:
				e.SetState(environment.ProcessStartingState)
			case wire.StateStopping:
				e.SetState(environment.ProcessStoppingState)
			case wire.StateOffline:
				e.SetState(environment.ProcessOfflineState)
			}
		},

		// Worker diagnostics, logged against this server so that "why did it not
		// start" is answered in the same place the question is asked. The worker
		// chose the level; it uses the same vocabulary this logger does.
		Log: func(p wire.Log) {
			entry := e.log().WithField("source", "worker")
			for k, v := range p.Fields {
				entry = entry.WithField(k, v)
			}
			switch p.Level {
			case wire.LogError:
				entry.Error(p.Message)
			case wire.LogWarn:
				entry.Warn(p.Message)
			case wire.LogInfo:
				entry.Info(p.Message)
			default:
				entry.Debug(p.Message)
			}
		},

		Stats: func(p wire.Stats) {
			e.Events().Publish(environment.ResourceEvent, environment.Stats{
				Memory:      p.MemoryBytes,
				MemoryLimit: uint64(e.Config().Limits().BoundedMemoryLimit()),
				CpuAbsolute: p.CpuAbsolute,
				Uptime:      p.UptimeMillis,
				// Per-server network counters have no Windows equivalent without a
				// network namespace. Reported as zero rather than omitted so the
				// Panel's console graphs still render.
				Network: environment.NetworkStats{},
			})
		},

		Exit: func(p wire.Exit) {
			e.mu.Lock()
			e.exitCode = uint32(p.Code)
			e.memoryLimitHit = p.MemoryLimitHit
			e.startedAt = time.Time{}
			e.mu.Unlock()
			e.SetState(environment.ProcessOfflineState)
		},

		Disconnected: e.onDisconnect,
	}
}

// OnBeforeStart ensures the worker environment exists before a boot attempt.
func (e *Environment) OnBeforeStart(ctx context.Context) error {
	return e.Create()
}

// Start boots the server.
func (e *Environment) Start(ctx context.Context) error {
	sawError := false
	defer func() {
		if sawError {
			// Pass through stopping first so crash detection does not immediately
			// retry the action that just failed.
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
	}()

	c, err := e.connectOrSpawn(ctx)
	if err != nil {
		sawError = true
		return err
	}

	if c.Hello().State == wire.StateRunning {
		// Already running; adopt rather than starting a second copy.
		e.SetState(environment.ProcessRunningState)
		return nil
	}

	e.SetState(environment.ProcessStartingState)

	e.mu.RLock()
	pty, runtimeName := e.meta.PseudoConsole, e.meta.Runtime
	e.mu.RUnlock()

	// Put the egg's runtime ahead of the host PATH so that a startup line saying
	// "java" gets the version this egg asked for rather than whichever JRE was
	// installed most recently.
	// The Panel's variables layered over a working Windows environment. Without
	// the base, the process gets no SystemRoot, no TEMP and no PATH, because a
	// non-NULL environment block replaces the parent's rather than extending it.
	envVars := winenv.Merge(
		winenv.Base(winenv.Paths{Data: e.workingDirectory(), Temp: e.tempDirectory()}),
		e.Config().EnvironmentVariables(),
	)
	envVars = config.Get().Runtime.ApplyRuntime(runtimeName, envVars)

	argv, err := e.resolveStartup(envVars)
	if err != nil {
		sawError = true
		return err
	}

	// Reapplied on every start rather than only at creation: the Panel can
	// change a server's allocations while it is stopped, and a rule left over
	// from the previous set would leave the new ports closed and the old ones
	// open.
	e.applyFirewall()

	console := config.Get().Runtime.Console
	username, password, err := e.account()
	if err != nil {
		sawError = true
		return err
	}

	if err := c.Start(wire.Start{
		Argv:          argv,
		Env:           envVars,
		Limits:        e.Config().Limits().AsJobLimits(),
		PseudoConsole: pty || console.PseudoConsole,
		Cols:          console.Columns,
		Rows:          console.Rows,
		Username:      username,
		Password:      password,
	}); err != nil {
		sawError = true
		return errors.WrapIf(err, "environment/windows: failed to start server process")
	}

	e.mu.Lock()
	e.startedAt = time.Now()
	e.mu.Unlock()

	return nil
}

// resolveStartup turns the egg's startup string into an argv.
//
// Upstream never had to do this. It exported the startup line as the STARTUP
// environment variable and the Docker image's entrypoint performed the variable
// substitution and handed the result to a shell. There is no entrypoint here, so
// both steps are ours.
//
// Substitution accepts the {{VAR}} form eggs use as well as ${VAR}. Splitting is
// done with CommandLineToArgvW rules and the result is executed directly — never
// through cmd.exe, which would run partly user-controlled text with the server
// account's full privileges. Shell operators are consequently not interpreted.
func (e *Environment) resolveStartup(envVars []string) ([]string, error) {
	// The egg's own STARTUP, as the Panel sent it.
	eggStartup := ""
	for _, v := range envVars {
		if strings.HasPrefix(v, "STARTUP=") {
			eggStartup = strings.TrimPrefix(v, "STARTUP=")
			break
		}
	}

	// The Windows profile wins when it sets one. It is read from the metadata
	// rather than from STARTUP because the environment variables are rebuilt from
	// the Panel's configuration on every sync, and rebuilding them restores the
	// Linux command.
	e.mu.RLock()
	invocation, source := e.meta.Startup, "windows profile"
	e.mu.RUnlock()
	if strings.TrimSpace(invocation) == "" {
		invocation, source = eggStartup, "the egg's STARTUP variable"
	}

	if strings.TrimSpace(invocation) == "" {
		return nil, errors.New("environment/windows: server has no startup command configured")
	}

	e.log().WithFields(log.Fields{
		"source":   source,
		"template": invocation,
	}).Debug("resolving the startup command")

	if source != "windows profile" && looksLikeShell(invocation) {
		e.log().WithField("template", invocation).Warn(
			"this server is starting with the egg's Linux startup command because its egg " +
				"has no Windows startup override in the Panel plugin; the command uses shell " +
				"syntax that is not interpreted here, so it will almost certainly fail")
	}

	lookup := make(map[string]string, len(envVars))
	for _, v := range envVars {
		if k, val, ok := strings.Cut(v, "="); ok {
			lookup[k] = val
		}
	}

	// Normalise {{VAR}} to ${VAR}, matching what the yolks entrypoint did.
	expanded := strings.NewReplacer("{{", "${", "}}", "}").Replace(invocation)
	expanded = os.Expand(expanded, func(k string) string { return lookup[k] })

	argv, err := winproc.ParseCommandLine(expanded)
	if err != nil {
		return nil, errors.WrapIf(err,
			"environment/windows: could not parse the resolved startup command")
	}

	e.log().WithFields(log.Fields{
		"source":   source,
		"expanded": expanded,
		"argc":     len(argv),
	}).Debug("resolved the startup command")

	return argv, nil
}

// looksLikeShell reports whether a startup line depends on shell features.
//
// Nothing here interprets them: the command is split with CommandLineToArgvW
// rules and executed directly, deliberately, because running partly
// user-controlled text through cmd.exe under the server's account is a shell
// injection with a service behind it. A line built out of $(...) and pipes will
// therefore be handed to CreateProcess verbatim and fail in a way that looks
// nothing like the cause, so it is worth saying so out loud.
func looksLikeShell(s string) bool {
	for _, tok := range []string{"$(", "&&", "||", "|", ";", "`", "$'"} {
		if strings.Contains(s, tok) {
			return true
		}
	}
	return false
}

// account returns the local account credentials this server runs under.
//
// Under managed isolation this creates the account on first use, so it can fail
// and can be slow the first time. Both are acceptable at the point it is called:
// starting a server already talks to the account database to obtain a token.
func (e *Environment) account() (string, string, error) {
	return accounts.For(e.Id)
}

// Attach begins streaming console output. The worker is already buffering it, so
// this only needs to ensure a connection exists.
func (e *Environment) Attach(ctx context.Context) error {
	_, err := e.connect(ctx)
	return err
}

// Stop asks the server to shut down gracefully.
func (e *Environment) Stop(ctx context.Context) error {
	e.mu.RLock()
	c, stop := e.client, e.meta.Stop
	e.mu.RUnlock()

	if c == nil {
		// Nothing is running, so the server is already in the desired state.
		e.SetState(environment.ProcessOfflineState)
		return nil
	}

	e.SetState(environment.ProcessStoppingState)

	msg := wire.Stop{TimeoutSeconds: 30}
	switch stop.Type {
	case remote.ProcessStopCommand:
		msg.Mode = wire.StopCommand
		msg.Value = stop.Value

	case remote.ProcessStopSignal, remote.ProcessStopNativeStop:
		// Neither of these exists here. A POSIX signal has no Windows equivalent,
		// and a "native stop" means `docker stop`, which is SIGTERM followed by a
		// kill. CTRL_BREAK is the nearest thing to both; it is widely unhandled by
		// game servers, so it is attempted and then escalated rather than relied
		// upon.
		//
		// Neither is a good outcome. An egg that stops this way has no graceful
		// path on Windows at all, and the operator wants to know that rather than
		// discover it when a save is lost, so it is a warning and not a note.
		msg.Mode = wire.StopCtrlBreak
		e.log().WithField("panel_type", stop.Type).
			Warn("this egg has no windows stop command; the server can only be asked to " +
				"stop with ctrl+break, and will be killed if it does not. Set a stop " +
				"command on the egg's windows profile")

	default:
		msg.Mode = wire.StopTerminate
		if stop.Type != "" {
			e.log().WithField("panel_type", stop.Type).
				Warn("unrecognised stop type; the server will be killed rather than asked to stop")
		} else {
			e.log().Warn("this egg defines no stop method at all; the server will be killed " +
				"rather than asked to stop. Set a stop command on the egg's windows profile")
		}
	}

	// The stop configuration is one of the things the egg's Windows profile can
	// override, so record what was actually resolved rather than leaving the
	// operator to guess whether the profile took effect.
	e.log().WithFields(log.Fields{
		"panel_type": stop.Type,
		"mode":       msg.Mode,
		"command":    msg.Value,
		"timeout":    msg.TimeoutSeconds,
	}).Debug("asking the worker to stop the server")

	return c.Stop(msg)
}

// WaitForStop stops the server and waits for it to exit, optionally killing it
// if the deadline passes.
func (e *Environment) WaitForStop(ctx context.Context, duration time.Duration, terminate bool) error {
	if e.State() == environment.ProcessOfflineState {
		return nil
	}

	if err := e.Stop(ctx); err != nil {
		return err
	}

	tctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	started := time.Now()
	e.log().WithFields(log.Fields{"deadline": duration.String(), "kill_after": terminate}).
		Debug("waiting for the server to stop")

	for {
		select {
		case <-tctx.Done():
			if terminate {
				e.log().WithField("waited", duration.String()).
					Warn("the server did not stop within the deadline; killing it")
				return e.Terminate(ctx, "SIGKILL")
			}
			e.log().WithField("waited", duration.String()).
				Warn("the server did not stop within the deadline")
			return tctx.Err()

		case <-ticker.C:
			if e.State() == environment.ProcessOfflineState {
				e.log().WithField("elapsed", time.Since(started).Round(100*time.Millisecond).String()).
					Debug("the server stopped")
				return nil
			}
		}
	}
}

// Terminate kills the server's process tree immediately.
//
// The signal argument is accepted for interface compatibility and ignored:
// Windows has no signals, and this always terminates the Job Object.
func (e *Environment) Terminate(ctx context.Context, _ string) error {
	e.mu.RLock()
	c := e.client
	e.mu.RUnlock()

	if c == nil {
		e.SetState(environment.ProcessOfflineState)
		return nil
	}

	e.SetState(environment.ProcessStoppingState)
	return c.Terminate()
}
