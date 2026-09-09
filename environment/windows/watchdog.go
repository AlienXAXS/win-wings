//go:build windows

package windows

import (
	"context"
	"time"

	"github.com/apex/log"

	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/internal/worker"
)

// Worker watchdog.
//
// The control connection dropping is not on its own a server failure. A daemon
// restart drops it, a worker shutdown drops it, and a transient pipe error drops
// it while the worker and the game process carry on perfectly well. So the
// watchdog's first move is always to try to get the connection back.
//
// What it must not do is nothing, which is what it did before. The worker holds
// the server's Job Object handle, and that job carries
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE -- deliberately, so that a dead worker
// cannot leave an unreachable server holding its ports. A worker that dies
// therefore takes the game process with it, and the daemon was left reporting a
// server as running when nothing of it existed. It stayed that way until
// somebody pressed a button.
//
// When reconnection fails, the conclusion is not "restart it" but "it is
// offline", which is the truth and which hands the decision to the crash
// detection the operator has already configured. That path knows about restart
// limits and about servers that fail immediately on every attempt; a private
// restart loop here would know about neither.

const (
	// reconnectAttempts and reconnectDelay bound how long a server is left in
	// limbo. Roughly ten seconds in total, which covers a worker that is briefly
	// too busy to accept a connection without leaving a genuinely dead one
	// misreported for long enough that somebody notices before the daemon does.
	reconnectAttempts = 10
	reconnectDelay    = time.Second
)

// onDisconnect runs the watchdog after the control connection drops.
//
// Called from the Disconnected handler, which must not block: it runs on the
// client's read loop.
func (e *Environment) onDisconnect(cause error) {
	e.mu.Lock()
	e.client = nil
	e.mu.Unlock()

	entry := e.log()
	if cause != nil {
		entry = entry.WithField("error", cause)
	}

	// A server that is not meant to be running has no worker to miss. This is
	// the ordinary case: stopping a server shuts its worker down, and the
	// connection drops immediately afterwards.
	state := e.State()
	if state != environment.ProcessRunningState && state != environment.ProcessStartingState {
		entry.Debug("worker connection closed")
		return
	}

	entry.Warn("lost the connection to this server's worker while the server was running; " +
		"attempting to reconnect")

	go e.reconnect()
}

// reconnect tries to re-establish the control connection, and declares the
// server offline if it cannot.
func (e *Environment) reconnect() {
	// Only one watchdog at a time. Without this, a connection that drops during
	// a reconnection attempt would start a second one, and the two would race to
	// adopt clients and set state.
	if !e.watchdog.SwapIf(true) {
		return
	}
	defer e.watchdog.Store(false)

	for attempt := 1; attempt <= reconnectAttempts; attempt++ {
		// The server being destroyed or the daemon shutting down cancels this;
		// there is nothing to reconnect to and nothing to report.
		select {
		case <-e.Context().Done():
			return
		case <-time.After(reconnectDelay):
		}

		e.mu.RLock()
		since := e.lastSeq
		e.mu.RUnlock()

		ctx, cancel := context.WithTimeout(e.Context(), 5*time.Second)
		c, err := worker.Dial(ctx, e.Id, e.token, since, e.handlers())
		cancel()
		if err == nil {
			// adoptClient takes the worker's own view of the server's state,
			// which is authoritative: it owns the process.
			e.adoptClient(c)
			e.log().WithField("attempts", attempt).
				Info("reconnected to this server's worker; the server was not interrupted")
			return
		}

		e.log().WithFields(log.Fields{"attempt": attempt, "error": err}).
			Debug("worker is still unreachable")
	}

	// Out of attempts. The worker is gone, and with it the job object handle
	// that was keeping the server's processes alive.
	e.log().Error("this server's worker is gone and could not be reached after " +
		"repeated attempts; the server's processes were terminated with it, because the " +
		"job object holding them is released when the worker exits. Marking the server " +
		"offline so crash detection can decide whether to restart it. Look in the " +
		"instance directory's worker.log and worker-stderr.log for why the worker exited")

	e.mu.Lock()
	e.startedAt = time.Time{}
	e.mu.Unlock()

	// Through stopping first, the same way a failed start does, so that the
	// transition looks like every other route to offline rather than a server
	// spontaneously changing state.
	e.SetState(environment.ProcessStoppingState)
	e.SetState(environment.ProcessOfflineState)
}
