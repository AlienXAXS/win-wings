//go:build windows

package worker

import (
	"fmt"
	"runtime/debug"

	"github.com/pterodactyl/wings/internal/wire"
)

// Panic barriers.
//
// A worker is one server's entire supervisor, and it is the only thing holding
// that server's job object open. The job carries KILL_ON_JOB_CLOSE -- on purpose,
// so a dead worker cannot leave an unreachable server holding its ports -- which
// means every panic in this process is a server outage, whatever it was actually
// about. A nil dereference while working out why a start request could not be
// satisfied should cost the request and nothing else.
//
// So every goroutine the worker runs gets a barrier, and every request handler
// turns a panic into an error reply. The alternative is what happened before:
// a bug in the failure path of Start took down a server that had not started
// yet, and destroyed the error explaining why on its way out.
//
// Recovering does not mean pretending. Each recovery is logged at error level
// with its stack, over the control pipe and into worker.log, so a worker that
// is limping is visible rather than merely quiet.

// maxStackBytes bounds what a recovery puts on the wire. Enough for the frames
// that matter without a runaway stack flooding the daemon's log.
const maxStackBytes = 8 << 10

// guard recovers a panic, records it, and optionally reports it as an error.
//
// It must be deferred directly -- defer w.guard(...) -- because recover only
// works when called by the function a defer names. Wrapping it in a closure
// silently disables it.
//
// When dst is non-nil the recovered panic is stored there as an error, so a
// handler with a named error result reports the failure to its caller instead of
// returning as though it had succeeded. That distinction matters: silently
// swallowing a panic in Start would leave the daemon believing a server was
// starting when nothing was.
func (w *Worker) guard(what string, dst *error) {
	r := recover()
	if r == nil {
		return
	}

	stack := string(debug.Stack())
	if len(stack) > maxStackBytes {
		stack = stack[:maxStackBytes] + "\n... stack truncated"
	}

	w.Log(wire.LogError, "recovered from a panic; the worker is still running",
		"during", what,
		"panic", fmt.Sprint(r),
		"stack", stack)

	if dst != nil {
		*dst = fmt.Errorf("worker: %s: internal error: %v", what, r)
	}
}
