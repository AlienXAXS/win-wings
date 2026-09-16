//go:build windows

package worker

import (
	"strconv"
	"time"

	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/internal/jobobject"
	"github.com/pterodactyl/wings/internal/winproc"
	"github.com/pterodactyl/wings/internal/wire"
)

// preStopKillGrace is how long a pre-stop command that overran its timeout is
// given to go away after its job is terminated. Terminating a job does not
// fail silently, so this is only ever waited out by something stuck in the
// kernel.
const preStopKillGrace = 5 * time.Second

// runPreStop runs a stop's pre-stop command to completion and reports whether
// the server had exited by the time it finished -- in which case the stop is
// done and nothing needs escalating to.
//
// The command gets a job of its own rather than the server's. The server's job
// is torn down the moment the server exits, and a script whose RCON command has
// just worked would be killed mid-sentence by its own success; and the stop
// escalating to killing the server's job must not take the script with it
// either. Its own job still means it cannot leave anything behind: the job is
// closed on the way out, and it is created kill-on-close.
//
// It runs in the data directory, like a pre-start command and for a related
// reason: it is the egg's script, not the game, and the working directory an
// egg moves the game into may not be where the script's tools were installed.
//
// Nothing here fails the stop. A command that cannot be started, or that
// overruns its timeout and is killed, is logged and the stop carries on with
// the mechanism it would have used anyway.
func (w *Worker) runPreStop(cmd *wire.PreStopCommand, pid int, running bool, stopTimeout time.Duration, started time.Time) bool {
	label := preStopLabel(cmd)

	if !running {
		// The current process is a pre-start command, not the server. There is
		// no server to ask to stop, and the script would be talking to nothing.
		w.Log(wire.LogInfo, "skipping the pre-stop command; the server has not started yet",
			"command", label)
		return false
	}

	timeout := time.Duration(cmd.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = stopTimeout
	}

	job, err := jobobject.Create()
	if err != nil {
		w.Log(wire.LogWarn, "could not create a job for the pre-stop command; skipping it",
			"command", label, "error", err.Error())
		return false
	}
	// Kill-on-close: anything the script started and did not wait for goes
	// with it.
	defer func() { _ = job.Close() }()

	var token windows.Token
	if cmd.Username != "" {
		token, err = winproc.LogonUser(cmd.Username, cmd.Password)
		if err != nil {
			w.Log(wire.LogWarn, "could not log on the server's account for the pre-stop command; skipping it",
				"command", label, "account", cmd.Username, "error", err.Error())
			return false
		}
		defer func() { _ = token.Close() }()
	}

	// The script's most likely first move is to wait for this process to go
	// away after asking it to, so it is told which process that is.
	env := append(append([]string(nil), cmd.Env...), "SERVER_PID="+strconv.Itoa(pid))

	proc, err := winproc.Start(winproc.Config{
		Argv:          cmd.Argv,
		Dir:           w.cfg.WorkingDir,
		Env:           env,
		Token:         token,
		PseudoConsole: cmd.PseudoConsole,
	}, job)
	if err != nil {
		w.Log(wire.LogWarn, "could not start the pre-stop command; skipping it",
			"command", label, "executable", cmd.Argv[0], "error", err.Error())
		return false
	}
	defer func() { _ = proc.Close() }()

	// The label and the executable only, as for a pre-start command: the
	// arguments may carry a password.
	w.Log(wire.LogInfo, "running the pre-stop command before asking the server to stop",
		"command", label, "executable", cmd.Argv[0], "timeout", timeout.String(),
		"pid", proc.Pid, "server_pid", pid)

	go w.pumpConsole(proc)

	exited := make(chan uint32, 1)
	go func() {
		defer w.guard("waiting for the pre-stop command", nil)
		code, err := proc.Wait()
		if err != nil {
			code = 0
		}
		exited <- code
	}()

	select {
	case code := <-exited:
		if code != 0 {
			w.Log(wire.LogWarn, "the pre-stop command failed; continuing with the stop regardless",
				"command", label, "exit_code", code, "elapsed", elapsed(started))
		} else {
			w.Log(wire.LogInfo, "the pre-stop command finished",
				"command", label, "exit_code", code, "elapsed", elapsed(started))
		}
	case <-time.After(timeout):
		w.Log(wire.LogWarn, "the pre-stop command did not finish in time; killing it and "+
			"continuing with the stop", "command", label, "timeout", timeout.String(),
			"elapsed", elapsed(started))
		if err := job.Terminate(1); err != nil {
			w.Log(wire.LogError, "failed to kill the pre-stop command",
				"command", label, "pid", proc.Pid, "error", err.Error())
		}
		select {
		case <-exited:
		case <-time.After(preStopKillGrace):
			w.Log(wire.LogError, "the pre-stop command's job was killed but the process is still present",
				"command", label, "pid", proc.Pid)
		}
	}

	w.mu.Lock()
	stillRunning := w.proc != nil
	w.mu.Unlock()
	if !stillRunning {
		w.Log(wire.LogInfo, "the server exited during the pre-stop command",
			"command", label, "server_pid", pid, "elapsed", elapsed(started))
		return true
	}
	return false
}

// preStopLabel names a pre-stop command for the log, falling back to the
// executable as preStartLabel does.
func preStopLabel(cmd *wire.PreStopCommand) string {
	if cmd.Label != "" {
		return cmd.Label
	}
	if len(cmd.Argv) > 0 {
		return cmd.Argv[0]
	}
	return "(empty)"
}
