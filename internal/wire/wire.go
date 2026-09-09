// Package wire defines the control protocol spoken between the win-wings
// daemon and the per-server worker processes it supervises.
//
// # Why a separate worker process exists
//
// Under Docker, the daemon could restart and reattach to a running container's
// console because the container's stdio belongs to the Docker daemon, not to
// wings. Windows offers no equivalent: a process's stdin/stdout handles cannot
// be reattached from outside once their creator is gone. If the daemon owned the
// game process's pipes directly, a daemon restart would permanently lose console
// output and command input for every running server.
//
// The worker exists to hold those handles. It outlives daemon restarts, and the
// daemon reconnects to it over a named pipe.
//
// # Transport
//
// One named pipe per server at \\.\pipe\winwings-<uuid>, created by the worker
// with a security descriptor granting access only to the account the daemon runs
// as. Messages are newline-delimited JSON in both directions.
//
// JSON Lines was chosen over a binary framing for debuggability — a stuck server
// can be diagnosed by attaching to the pipe and reading it. Console payloads are
// base64-encoded rather than sent as JSON strings because game servers emit
// arbitrary bytes, and invalid UTF-8 would otherwise be silently replaced.
package wire

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is incremented on any breaking change to the message set.
//
// The daemon and worker are deployed together, but a worker survives a daemon
// upgrade — an operator who updates win-wings while servers are running will
// have new daemons talking to old workers. The daemon checks this on connect and
// refuses to drive a worker it does not understand, rather than misinterpreting
// its messages.
const ProtocolVersion = 1

// PipeName returns the named pipe path for a server's worker.
func PipeName(uuid string) string {
	return `\\.\pipe\winwings-` + uuid
}

// MessageType identifies the payload carried by an Envelope.
type MessageType string

// Messages sent from the daemon to a worker.
const (
	// TypeHandshake is the first message on any connection.
	TypeHandshake MessageType = "handshake"
	// TypeStdin writes data to the server process's standard input.
	TypeStdin MessageType = "stdin"
	// TypeStop asks the worker to bring the process down gracefully.
	TypeStop MessageType = "stop"
	// TypeTerminate kills the process tree immediately via the Job Object.
	TypeTerminate MessageType = "terminate"
	// TypeStart launches the process. A worker idles until it receives this.
	TypeStart MessageType = "start"
	// TypeStatsRequest asks for an immediate resource sample.
	TypeStatsRequest MessageType = "stats_request"
	// TypeUpdateLimits adjusts Job Object limits on a running process.
	TypeUpdateLimits MessageType = "update_limits"
	// TypeShutdown asks the worker to stop the process and then exit itself.
	TypeShutdown MessageType = "shutdown"
	// TypePing checks liveness.
	TypePing MessageType = "ping"
)

// Messages sent from a worker to the daemon.
const (
	// TypeHello is the worker's handshake response, describing current state.
	TypeHello MessageType = "hello"
	// TypeConsole carries a chunk of console output.
	TypeConsole MessageType = "console"
	// TypeState reports a process state transition.
	TypeState MessageType = "state"
	// TypeStats reports a resource sample.
	TypeStats MessageType = "stats"
	// TypeExit reports that the process has exited.
	TypeExit MessageType = "exit"
	// TypeAck acknowledges a command that produced no other reply. Commands the
	// daemon waits on must always answer, success or failure, or the caller
	// blocks until its timeout.
	TypeAck MessageType = "ack"
	// TypeError reports a worker-side failure in response to a command.
	TypeError MessageType = "error"
	// TypePong answers TypePing.
	TypePong MessageType = "pong"
	// TypeLog carries a worker's own diagnostic output, as distinct from the
	// game server's console.
	//
	// A worker is spawned detached with no stdio, so anything it writes about
	// itself is otherwise lost: how it resolved the startup command, why a logon
	// failed, which desktop it granted. Relaying it up the control pipe puts it
	// in the daemon's log next to everything else about that server, which is
	// where somebody looking for it will be.
	TypeLog MessageType = "log"
)

// Envelope is the outer frame of every message.
type Envelope struct {
	Type MessageType `json:"t"`
	// ID correlates a response with the request that caused it. Zero for
	// unsolicited messages such as console output and state changes.
	ID uint64 `json:"id,omitempty"`
	// Payload is the type-specific body.
	Payload json.RawMessage `json:"p,omitempty"`
}

// ProcessState mirrors the states the daemon's environment layer tracks, so the
// worker never has to be translated into a different vocabulary.
type ProcessState string

const (
	StateOffline  ProcessState = "offline"
	StateStarting ProcessState = "starting"
	StateRunning  ProcessState = "running"
	StateStopping ProcessState = "stopping"
)

// StopMode selects how a graceful stop is attempted.
type StopMode string

const (
	// StopCommand writes a string to the process's stdin — "stop", "end", "quit".
	// This is what most eggs use and the only mechanism that works reliably.
	StopCommand StopMode = "command"

	// StopCtrlC delivers a Ctrl+C the way a keyboard does: the byte 0x03 is
	// written to the process's console input and the console driver turns it into
	// a CTRL_C_EVENT. Windows has no signal to send, and GenerateConsoleCtrlEvent
	// cannot target CTRL_C_EVENT at a process group, so this is the only way to
	// deliver one to a single server.
	//
	// It requires the server to have been given a pseudo console: without one
	// there is no console input to write to and no driver to translate the byte.
	// An egg that stops this way must enable the pseudo console in its Windows
	// profile.
	StopCtrlC StopMode = "ctrl_c"

	// StopCtrlBreak sends CTRL_BREAK_EVENT to the process group.
	//
	// This is the closest Windows analogue to SIGTERM, and it is a poor one.
	// CTRL_C_EVENT cannot be targeted at a specific process group at all, and
	// CTRL_BREAK reaches every process attached to the console — including the
	// worker, which must ignore it. Many servers install no handler and die
	// uncleanly, which is no better than a kill.
	StopCtrlBreak StopMode = "ctrl_break"

	// StopTerminate kills the Job Object outright. Used as the timeout fallback.
	StopTerminate StopMode = "terminate"
)

// --- Daemon to worker payloads ----------------------------------------------

// Handshake opens a connection and asserts the protocol the daemon speaks.
type Handshake struct {
	Version int `json:"version"`
	// Token authenticates the daemon to the worker. The pipe ACL is the primary
	// control; this guards against a same-account process on the host connecting
	// to the pipe and driving the server.
	Token string `json:"token"`
	// Since requests console backlog from this sequence number onward, so a
	// reconnecting daemon can recover output produced while it was gone. Zero
	// requests the whole retained buffer.
	Since uint64 `json:"since"`
}

// Stdin writes to the process's standard input.
type Stdin struct {
	// Data is raw bytes, base64-encoded by encoding/json.
	Data []byte `json:"data"`
}

// Stop requests a graceful shutdown.
type Stop struct {
	Mode StopMode `json:"mode"`
	// Value is the command written to stdin when Mode is StopCommand.
	Value string `json:"value,omitempty"`
	// TimeoutSeconds bounds the graceful attempt. When it elapses the worker
	// escalates to terminating the Job Object. Zero means wait indefinitely,
	// which the daemon should avoid.
	TimeoutSeconds int `json:"timeout_seconds"`
}

// Start launches the configured process.
type Start struct {
	// Argv is the resolved command, already split into arguments by the daemon.
	// Sent per-start rather than stored on disk because it embeds substituted
	// egg variables that can change between restarts.
	Argv []string `json:"argv"`

	// Env is the process environment as "KEY=VALUE" strings.
	Env []string `json:"env"`

	// Limits to apply to the Job Object before the process runs.
	Limits Limits `json:"limits"`

	// PseudoConsole allocates a ConPTY instead of pipes for this run.
	PseudoConsole bool   `json:"pseudo_console"`
	Cols          uint16 `json:"cols,omitempty"`
	Rows          uint16 `json:"rows,omitempty"`

	// Username and Password name the local account to run the process as. Empty
	// runs it as the account the worker itself uses.
	//
	// These travel in this message rather than the worker's on-disk config
	// deliberately: credentials for a server account should not be written to
	// the filesystem at all, even in a directory the daemon owns.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// UpdateLimits adjusts resource limits without restarting the process. This is
// the equivalent of upstream's in-situ container update.
type UpdateLimits struct {
	Limits Limits `json:"limits"`
}

// Limits describes the Job Object limits applied to a server.
type Limits struct {
	// MemoryBytes is the hard limit applied via JOB_OBJECT_LIMIT_JOB_MEMORY.
	//
	// Unlike a cgroup memory limit there is no OOM killer involved: allocations
	// simply start failing once the job is at its cap. A runtime that handles
	// allocation failure badly will crash in a less obvious way than being
	// reaped, which is why the configured overhead multiplier matters here.
	MemoryBytes int64 `json:"memory_bytes"`

	// CpuRate is the share of total host CPU expressed in 1/100ths of a percent,
	// so 10000 means every processor. Zero disables CPU rate control.
	//
	// Note this differs from the Panel's notion of a CPU limit, where 200 means
	// two full cores. Conversion depends on the host's processor count and is
	// done by the daemon.
	CpuRate uint32 `json:"cpu_rate"`

	// CpuHardCap enforces CpuRate even when the host is idle. Without it the
	// rate acts as a relative weight under contention only.
	CpuHardCap bool `json:"cpu_hard_cap"`

	// AffinityMask restricts the job to specific processors. Zero means all.
	AffinityMask uint64 `json:"affinity_mask"`

	// ProcessLimit caps concurrently active processes in the job.
	ProcessLimit uint32 `json:"process_limit"`
}

// --- Worker to daemon payloads ----------------------------------------------

// Hello answers a Handshake.
type Hello struct {
	Version int          `json:"version"`
	State   ProcessState `json:"state"`
	// PID of the supervised process, zero when not running.
	PID int `json:"pid"`
	// Sequence is the sequence number of the most recent console chunk, so the
	// daemon knows how far the backlog reaches.
	Sequence uint64 `json:"sequence"`
	// StartedAt is the Unix millisecond timestamp of the current run, zero when
	// not running. Used to compute uptime without the worker and daemon needing
	// synchronised clocks beyond the host's own.
	StartedAt int64 `json:"started_at"`
}

// Console carries a chunk of output from the process.
type Console struct {
	// Sequence increases monotonically across the worker's lifetime, letting a
	// reconnecting daemon resume without duplicating or dropping output.
	Sequence uint64 `json:"seq"`
	// Data is raw console bytes, base64-encoded by encoding/json. It is not
	// guaranteed to be valid UTF-8, nor to end on a line boundary.
	Data []byte `json:"data"`
}

// State reports a process state transition.
type State struct {
	State ProcessState `json:"state"`
	PID   int          `json:"pid,omitempty"`
}

// Stats is a resource usage sample.
type Stats struct {
	// MemoryBytes is the job's current committed memory.
	MemoryBytes uint64 `json:"memory_bytes"`
	// CpuAbsolute is CPU use as a percentage of a single processor, matching the
	// units the Panel expects, where 200 means two cores fully used.
	CpuAbsolute float64 `json:"cpu_absolute"`
	// Processes currently active in the job.
	Processes uint32 `json:"processes"`
	// UptimeMillis since the process started.
	UptimeMillis int64 `json:"uptime_ms"`
	// DiskReadBytes and DiskWriteBytes are cumulative for the job's lifetime.
	DiskReadBytes  uint64 `json:"disk_read_bytes"`
	DiskWriteBytes uint64 `json:"disk_write_bytes"`
}

// Exit reports process termination.
type Exit struct {
	Code int `json:"code"`
	// MemoryLimitHit reports that the job exceeded its memory limit before
	// exiting. This is the closest available analogue to Docker's OOMKilled
	// flag, which the Panel surfaces to users.
	MemoryLimitHit bool `json:"memory_limit_hit"`
	// Terminated reports that the worker killed the process rather than it
	// exiting on its own.
	Terminated bool `json:"terminated"`
}

// LogLevel is the severity of a Log message. Deliberately the same vocabulary
// the daemon's logger uses, so relaying one needs no translation.
type LogLevel string

const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// Log is one line of a worker's own diagnostic output.
type Log struct {
	Level   LogLevel `json:"level"`
	Message string   `json:"message"`
	// Fields carries structured context. Kept as strings because this crosses a
	// process boundary as JSON and arrives at a logger that will render it as
	// text anyway; typed values would only survive to be flattened again.
	Fields map[string]string `json:"fields,omitempty"`
}

// Error reports a worker-side failure.
type Error struct {
	Message string `json:"message"`
}

// --- Encoding helpers -------------------------------------------------------

// Marshal wraps a payload in an envelope.
func Marshal(t MessageType, id uint64, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("wire: marshal %s payload: %w", t, err)
		}
		raw = b
	}
	b, err := json.Marshal(Envelope{Type: t, ID: id, Payload: raw})
	if err != nil {
		return nil, fmt.Errorf("wire: marshal %s envelope: %w", t, err)
	}
	return append(b, '\n'), nil
}

// Unmarshal decodes an envelope's payload into v.
func Unmarshal(e Envelope, v any) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("wire: %s message has no payload", e.Type)
	}
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("wire: unmarshal %s payload: %w", e.Type, err)
	}
	return nil
}
