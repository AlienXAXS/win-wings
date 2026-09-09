//go:build windows

package worker

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pterodactyl/wings/internal/wire"
)

// Worker diagnostics.
//
// A worker is spawned detached, with no stdin, stdout or stderr. Anything it
// wants to say about itself -- how it resolved the startup command, why a logon
// failed, which desktop it granted a server account -- had nowhere to go, and an
// operator watching the daemon's log saw a server go from "starting" to nothing
// with no explanation in between.
//
// Two destinations, because neither is sufficient alone:
//
//   - The control pipe, so that a connected daemon interleaves worker messages
//     with its own and an operator has one log to read.
//   - A file in the instance directory, because the most interesting failures
//     happen before the daemon has connected or after it has gone away, and a
//     message sent to nobody is not a diagnostic.
//
// The file sits beside worker.json rather than in the server's data directory:
// a server must not be able to rewrite the record of what its own worker did.

// maxLogFileBytes caps the diagnostic log. It is written a line at a time by the
// worker itself rather than by the server, so it grows slowly; the cap exists so
// that a worker stuck in a restart loop cannot fill the volume the servers are
// on.
const maxLogFileBytes = 4 << 20

var (
	logMu   sync.Mutex
	logFile *os.File
	logSize int64
)

// SetDiagnosticLog points worker diagnostics at a file. Called once at startup.
//
// Failure is not reported: a worker that cannot write its log must still run the
// server. The relay over the control pipe is unaffected either way.
func SetDiagnosticLog(path string) {
	logMu.Lock()
	defer logMu.Unlock()

	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	if st, err := f.Stat(); err == nil {
		logSize = st.Size()
	}
	logFile = f
}

func writeDiagnostic(level wire.LogLevel, line string) {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile == nil {
		return
	}

	// Truncate rather than rotate. There is one consumer for this file and it is
	// a person reading it after something went wrong; keeping generations of it
	// would be more machinery than the problem deserves.
	if logSize >= maxLogFileBytes {
		if err := logFile.Truncate(0); err != nil {
			return
		}
		if _, err := logFile.Seek(0, 0); err != nil {
			return
		}
		logSize = 0
	}

	n, err := fmt.Fprintf(logFile, "%s %-5s %s\n",
		time.Now().Format("2006-01-02 15:04:05.000"), strings.ToUpper(string(level)), line)
	if err != nil {
		return
	}
	logSize += int64(n)
}

// Log records a worker diagnostic and relays it to any connected daemon.
//
// Fields are optional and come in pairs: Log(wire.LogInfo, "started", "pid", 42).
// An odd trailing argument is dropped rather than panicking -- a logging call is
// not worth taking a server down for.
func (w *Worker) Log(level wire.LogLevel, msg string, fields ...any) {
	f := pairs(fields)
	writeDiagnostic(level, msg+formatFields(f))
	w.broadcast(wire.TypeLog, wire.Log{Level: level, Message: msg, Fields: f})
}

func (w *Worker) Debugf(format string, args ...any) {
	w.Log(wire.LogDebug, fmt.Sprintf(format, args...))
}

func pairs(fields []any) map[string]string {
	if len(fields) < 2 {
		return nil
	}
	out := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			continue
		}
		out[key] = fmt.Sprint(fields[i+1])
	}
	return out
}

// formatFields renders fields for the log file in a stable order, so that two
// runs of the same code produce comparable lines.
func formatFields(f map[string]string) string {
	if len(f) == 0 {
		return ""
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, f[k])
	}
	return b.String()
}

// accountOrSelf describes which account a process will run as, for logging.
// An empty username means the worker's own account, which is worth saying
// explicitly: it is the case where server isolation is not in effect.
func accountOrSelf(username string) string {
	if username == "" {
		return "(the worker's own account -- no isolation)"
	}
	return username
}
