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
const ProtocolVersion = 2

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

	// StopCtrlC raises a real CTRL_C_EVENT on the server's console. Windows has
	// no signal to send, and this is the closest thing that exists: it is exactly
	// what pressing Ctrl+C in a terminal does, and most console servers shut down
	// cleanly on it.
	//
	// It needs no pseudo console. The server shares the worker's console, and the
	// event is raised there; the worker survives it by handling it. See
	// winproc/console.go for the conditions that makes this work.
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

	// WorkingDir is the server process's working directory, relative to the
	// server's data directory. Empty means the data directory itself, which is
	// what almost every egg wants.
	//
	// It exists for games that build their own paths by climbing out of the
	// directory they were started in -- a log root at "..\Logs" is the common
	// shape. Started from the data directory that lands in the server's root,
	// which the account is denied by design; started from the subdirectory the
	// game was installed into it lands back inside the sandbox.
	//
	// Resolved and confined by the daemon and checked again here. It applies to
	// the server process only: pre-start commands keep the data directory,
	// because they run before the install content that this names exists.
	WorkingDir string `json:"working_dir,omitempty"`

	// PreStart are commands to run to completion, in order, before Argv, in the
	// same job, directory, environment and account, with their output on the
	// console. Empty means none.
	//
	// This is what the Docker image's entrypoint did ahead of the startup
	// command -- a steamcmd update, typically -- plus whatever preparation the
	// egg's own pre-start script does. The daemon decides which are due and what
	// they are; the worker only runs them in the order given. They may carry
	// credentials, so the worker logs a command's label rather than its
	// arguments.
	PreStart []PreStartCommand `json:"pre_start,omitempty"`

	// Limits to apply to the Job Object before the process runs.
	Limits Limits `json:"limits"`

	// PseudoConsole allocates a ConPTY instead of pipes for this run.
	PseudoConsole bool   `json:"pseudo_console"`
	Cols          uint16 `json:"cols,omitempty"`
	Rows          uint16 `json:"rows,omitempty"`

	// Console redirects where the server's output is read from and where its
	// commands are written to. The zero value is the ordinary arrangement: the
	// process's own stdout and stdin.
	Console ConsoleConfig `json:"console,omitempty"`

	// Username and Password name the local account to run the process as. Empty
	// runs it as the account the worker itself uses.
	//
	// These travel in this message rather than the worker's on-disk config
	// deliberately: credentials for a server account should not be written to
	// the filesystem at all, even in a directory the daemon owns.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// PreStartCommand is one command run to completion ahead of the server.
type PreStartCommand struct {
	// Argv is the resolved command, already split by the daemon.
	Argv []string `json:"argv"`
	// Label names the command for the log, because Argv cannot be logged: a
	// steamcmd update carries the account password in it.
	Label string `json:"label,omitempty"`
}

// ConsoleConfig describes where a run's console output comes from and where the
// commands typed into the Panel go.
//
// Both default to the process's own stdio, which is what almost every egg wants.
// They exist for the servers that want neither: a game that logs only to a file
// and accepts commands only over a TCP port of its own. On Linux those eggs are
// started through a shell pipeline -- tail -F into the container's stdout and a
// telnet client on its stdin -- which has no equivalent here and would in any
// case put a shell between the daemon and the process it supervises. Doing it in
// the worker keeps the game process the process: its exit code is still the
// server's, and a stop still acts on it.
type ConsoleConfig struct {
	// Source says where console output is read from.
	Source LogSource `json:"source,omitempty"`
	// Commands says where console input is written to.
	Commands CommandChannel `json:"commands,omitempty"`
}

// SourceType selects where a run's console output comes from.
type SourceType string

const (
	// SourceStdout reads the process's own output. The default.
	SourceStdout SourceType = "stdout"
	// SourceFile additionally follows a log file the server writes.
	SourceFile SourceType = "file"
)

// LogSource configures following a server's log file.
//
// The process's own output is streamed either way. A server that logs to a file
// usually says nothing on stdout, but when it does -- a startup banner, a fatal
// error raised before the log is opened -- that is exactly the output somebody
// needs, and there is no reason to suppress it.
type LogSource struct {
	Type SourceType `json:"type,omitempty"`

	// Path is the log file, relative to the server's data directory. Absolute
	// paths and any path escaping that directory are refused by the daemon; the
	// worker checks again, because it is the one holding the privileges.
	Path string `json:"path,omitempty"`

	// Encoding names the file's character encoding. Empty and "utf-8" are passed
	// through unchanged; "utf-16le" and "utf-16be" are decoded to UTF-8, because
	// a .NET server writing a log with a default StreamWriter produces UTF-16
	// that a console renders as text interleaved with NUL bytes.
	Encoding string `json:"encoding,omitempty"`
}

// ChannelType selects where console input is written.
type ChannelType string

const (
	// ChannelStdin writes to the process's standard input. The default.
	ChannelStdin ChannelType = "stdin"
	// ChannelTelnet writes to a TCP port the server itself listens on.
	ChannelTelnet ChannelType = "telnet"
)

// CommandChannel configures a TCP console for a server that does not read
// stdin.
//
// The channel is a byte pipe in both directions: commands are written as lines,
// and everything the server sends back goes on the console, which is where the
// answer to a command belongs. "telnet" names what the server is speaking rather
// than a client being run: only option negotiation is handled, and only by
// refusing every option, which is what a line-oriented game console wants
// anyway.
type CommandChannel struct {
	Type ChannelType `json:"type,omitempty"`

	// Host to connect to. Empty means 127.0.0.1, which is the only address one
	// of these consoles should ever be reachable on.
	Host string `json:"host,omitempty"`

	// Port the server listens on.
	Port int `json:"port,omitempty"`

	// Password is sent as the first line after connecting, when set. Several
	// game consoles open with a password prompt and accept nothing until it is
	// answered.
	Password string `json:"password,omitempty"`

	// ConnectTimeoutSeconds bounds how long the worker keeps trying to reach the
	// port after the server starts. A game that takes two minutes to load a world
	// does not open its console until it has, so this is a wait rather than a
	// retry budget. Zero means the worker's default.
	ConnectTimeoutSeconds int `json:"connect_timeout_seconds,omitempty"`
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
	// PIDs are the processes currently in the job. Network traffic is not
	// counted by the worker: the daemon runs one host-wide kernel trace and
	// attributes each packet to a server by the PID that sent or received it,
	// and this list is what it attributes against. Empty when nothing is
	// running. Additive, so a worker predating it simply reports no network.
	PIDs []uint32 `json:"pids,omitempty"`
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
