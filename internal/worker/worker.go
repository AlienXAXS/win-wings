//go:build windows

// Package worker implements the per-server supervisor process.
//
// One worker exists per running server. It owns the Job Object, the game
// process, and its console handles, and exposes control over a named pipe. See
// internal/wire for why this is a separate process rather than part of the
// daemon.
package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/internal/jobobject"
	"github.com/pterodactyl/wings/internal/winproc"
	"github.com/pterodactyl/wings/internal/wire"
)

// Config is the worker's on-disk configuration, written by the daemon into the
// server's instance directory before the worker is spawned.
//
// Deliberately minimal: everything that varies per start, and anything secret,
// arrives in the wire.Start message instead.
type Config struct {
	// UUID identifies the server and names the control pipe.
	UUID string `json:"uuid"`

	// Token authenticates the daemon on the control pipe. The pipe's ACL is the
	// primary control; this guards against another process running as the same
	// account connecting and driving the server.
	Token string `json:"token"`

	// WorkingDir is the server's data directory, used as the process's working
	// directory. This is the only path here the server itself can write to.
	WorkingDir string `json:"working_dir"`

	// LogPath is where console output is mirrored.
	LogPath string `json:"log_path"`

	// ConsoleBacklog is how many output chunks to retain for a reconnecting
	// daemon.
	ConsoleBacklog int `json:"console_backlog"`

	// MaxLogSizeMB and MaxLogFiles control console log rotation.
	MaxLogSizeMB int64 `json:"max_log_size_mb"`
	MaxLogFiles  int   `json:"max_log_files"`

	// StatsIntervalMS is how often resource samples are pushed.
	StatsIntervalMS int `json:"stats_interval_ms"`
}

// LoadConfig reads a worker configuration from disk.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("worker: read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("worker: parse config: %w", err)
	}
	if c.UUID == "" {
		return nil, fmt.Errorf("worker: config has no uuid")
	}
	if c.WorkingDir == "" {
		return nil, fmt.Errorf("worker: config has no working directory")
	}
	if c.StatsIntervalMS <= 0 {
		c.StatsIntervalMS = 2000
	}
	return &c, nil
}

// Worker supervises one server process.
type Worker struct {
	cfg     Config
	console *consoleBuffer

	mu        sync.Mutex
	state     wire.ProcessState
	job       *jobobject.Job
	proc      *winproc.Process
	startedAt time.Time

	exitCode       int
	memoryLimitHit bool
	terminated     bool

	// stopping signals the exit watcher that the current exit was requested,
	// so it is not misreported as a crash.
	stopping bool

	conns map[*conn]struct{}

	shutdown chan struct{}
	once     sync.Once
}

// New creates a worker from a configuration.
func New(cfg Config) *Worker {
	return &Worker{
		cfg: cfg,
		console: newConsoleBuffer(
			cfg.LogPath, cfg.ConsoleBacklog, cfg.MaxLogSizeMB, cfg.MaxLogFiles),
		state:    wire.StateOffline,
		conns:    make(map[*conn]struct{}),
		shutdown: make(chan struct{}),
	}
}

// Serve listens on the control pipe until the worker is shut down.
func (w *Worker) Serve() error {
	sddl, err := pipeSecurityDescriptor()
	if err != nil {
		return err
	}

	l, err := winio.ListenPipe(wire.PipeName(w.cfg.UUID), &winio.PipeConfig{
		SecurityDescriptor: sddl,
		// Byte mode: the protocol is newline-delimited, so message framing is
		// handled by the reader rather than the pipe.
		MessageMode:      false,
		InputBufferSize:  64 * 1024,
		OutputBufferSize: 64 * 1024,
	})
	if err != nil {
		// Pipe names are exclusive, and NPFS reports an attempt to create one that
		// already exists as ERROR_ACCESS_DENIED rather than as a collision. Said
		// plainly, that is a permissions problem and sends whoever reads it after
		// ACLs and service accounts; it is almost always a second worker for a
		// server that already has one.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("worker: listen on %s: %w. A worker for this server is "+
				"probably already running and holding that pipe -- Windows reports a name "+
				"collision as access denied. This worker is exiting; the existing one is "+
				"untouched", wire.PipeName(w.cfg.UUID), err)
		}
		return fmt.Errorf("worker: listen on %s: %w", wire.PipeName(w.cfg.UUID), err)
	}
	defer l.Close()

	go func() {
		defer w.guard("closing the control pipe on shutdown", nil)
		<-w.shutdown
		_ = l.Close()
	}()

	for {
		c, err := l.Accept()
		if err != nil {
			select {
			case <-w.shutdown:
				return nil
			default:
			}
			// A failed accept should not take down a running server.
			continue
		}
		go w.handleConn(c)
	}
}

// pipeSecurityDescriptor builds an SDDL granting pipe access only to the account
// this worker runs as, plus SYSTEM.
//
// The daemon spawns the worker, so they share an account. Anything else on the
// host — including other servers running under their own pool accounts — is
// denied, which is what stops one compromised game server from driving another.
func pipeSecurityDescriptor() (string, error) {
	tok := windows.GetCurrentProcessToken()
	user, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("worker: token user: %w", err)
	}
	sid := user.User.Sid.String()

	// D:P            discretionary ACL, protected from inheritance
	// (A;;GA;;;sid)  allow generic-all to the worker's own account
	// (A;;GA;;;SY)   allow generic-all to SYSTEM, for administrative recovery
	return fmt.Sprintf("D:P(A;;GA;;;%s)(A;;GA;;;SY)", sid), nil
}

// conn is one connected daemon.
type conn struct {
	net.Conn
	w    *Worker
	send chan []byte
	once sync.Once
	done chan struct{}
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.Conn.Close()
	})
}

// push queues a message, dropping it if the peer is not keeping up.
func (c *conn) push(b []byte) {
	select {
	case c.send <- b:
	case <-c.done:
	default:
		// A daemon that cannot keep up with console output must not be allowed
		// to block the process reading from the game server's pipe.
	}
}

func (w *Worker) handleConn(nc net.Conn) {
	defer w.guard("handling a control connection", nil)

	c := &conn{Conn: nc, w: w, send: make(chan []byte, 256), done: make(chan struct{})}
	defer c.close()

	go func() {
		defer w.guard("writing to a control connection", nil)
		for {
			select {
			case b := <-c.send:
				if _, err := c.Write(b); err != nil {
					c.close()
					return
				}
			case <-c.done:
				return
			}
		}
	}()

	r := bufio.NewReaderSize(nc, 64*1024)

	// The first message must be a handshake.
	env, err := readEnvelope(r)
	if err != nil {
		return
	}
	if env.Type != wire.TypeHandshake {
		c.replyError(0, "expected handshake")
		return
	}

	var hs wire.Handshake
	if err := wire.Unmarshal(env, &hs); err != nil {
		c.replyError(env.ID, err.Error())
		return
	}
	if hs.Version != wire.ProtocolVersion {
		c.replyError(env.ID, fmt.Sprintf(
			"protocol version mismatch: worker speaks %d, daemon speaks %d",
			wire.ProtocolVersion, hs.Version))
		return
	}
	if w.cfg.Token != "" && hs.Token != w.cfg.Token {
		c.replyError(env.ID, "authentication failed")
		return
	}

	w.mu.Lock()
	state, pid, startedAt := w.state, w.pidLocked(), w.startedAt
	w.conns[c] = struct{}{}
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		delete(w.conns, c)
		w.mu.Unlock()
	}()

	var startedMillis int64
	if !startedAt.IsZero() {
		startedMillis = startedAt.UnixMilli()
	}
	if b, err := wire.Marshal(wire.TypeHello, env.ID, wire.Hello{
		Version:   wire.ProtocolVersion,
		State:     state,
		PID:       pid,
		Sequence:  w.console.Sequence(),
		StartedAt: startedMillis,
	}); err == nil {
		c.push(b)
	}

	// Replay whatever console output the daemon missed.
	for _, ch := range w.console.Since(hs.Since) {
		if b, err := wire.Marshal(wire.TypeConsole, 0, wire.Console{
			Sequence: ch.seq, Data: ch.data,
		}); err == nil {
			c.push(b)
		}
	}

	for {
		env, err := readEnvelope(r)
		if err != nil {
			return
		}
		w.dispatch(c, env)
	}
}

func readEnvelope(r *bufio.Reader) (wire.Envelope, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return wire.Envelope{}, err
	}
	var env wire.Envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return wire.Envelope{}, err
	}
	return env, nil
}

func (c *conn) reply(t wire.MessageType, id uint64, payload any) {
	if b, err := wire.Marshal(t, id, payload); err == nil {
		c.push(b)
	}
}

func (c *conn) replyError(id uint64, msg string) {
	c.reply(wire.TypeError, id, wire.Error{Message: msg})
}

// dispatch handles one request from a connected daemon.
//
// A panic here is answered rather than propagated: the daemon gets an error for
// the request it made, the worker keeps running, and the server it is
// supervising is not affected by a bug in handling something unrelated to it.
func (w *Worker) dispatch(c *conn, env wire.Envelope) {
	// Registered first so it runs last, once guard has turned any panic into an
	// error worth sending back.
	var failure error
	defer func() {
		if failure != nil {
			c.replyError(env.ID, failure.Error())
		}
	}()
	defer w.guard("handling a "+string(env.Type)+" request", &failure)

	w.handle(c, env)
}

func (w *Worker) handle(c *conn, env wire.Envelope) {
	switch env.Type {
	case wire.TypePing:
		c.reply(wire.TypePong, env.ID, struct{}{})

	case wire.TypeStart:
		var p wire.Start
		if err := wire.Unmarshal(env, &p); err != nil {
			c.replyError(env.ID, err.Error())
			return
		}
		if err := w.Start(p); err != nil {
			c.replyError(env.ID, err.Error())
			return
		}
		c.reply(wire.TypeAck, env.ID, struct{}{})

	case wire.TypeStdin:
		var p wire.Stdin
		if err := wire.Unmarshal(env, &p); err != nil {
			c.replyError(env.ID, err.Error())
			return
		}
		if err := w.WriteStdin(p.Data); err != nil {
			c.replyError(env.ID, err.Error())
			return
		}
		if env.ID != 0 {
			c.reply(wire.TypeAck, env.ID, struct{}{})
		}

	case wire.TypeStop:
		var p wire.Stop
		if err := wire.Unmarshal(env, &p); err != nil {
			c.replyError(env.ID, err.Error())
			return
		}
		go w.Stop(p)

	case wire.TypeTerminate:
		if err := w.Terminate(); err != nil {
			c.replyError(env.ID, err.Error())
			return
		}
		if env.ID != 0 {
			c.reply(wire.TypeAck, env.ID, struct{}{})
		}

	case wire.TypeUpdateLimits:
		var p wire.UpdateLimits
		if err := wire.Unmarshal(env, &p); err != nil {
			c.replyError(env.ID, err.Error())
			return
		}
		if err := w.UpdateLimits(p.Limits); err != nil {
			c.replyError(env.ID, err.Error())
			return
		}
		c.reply(wire.TypeAck, env.ID, struct{}{})

	case wire.TypeStatsRequest:
		if s, err := w.Stats(); err == nil {
			c.reply(wire.TypeStats, env.ID, s)
		} else {
			c.replyError(env.ID, err.Error())
		}

	case wire.TypeShutdown:
		go w.Shutdown()

	default:
		c.replyError(env.ID, fmt.Sprintf("unknown message type %q", env.Type))
	}
}

// broadcast sends a message to every connected daemon.
func (w *Worker) broadcast(t wire.MessageType, payload any) {
	b, err := wire.Marshal(t, 0, payload)
	if err != nil {
		return
	}
	w.mu.Lock()
	conns := make([]*conn, 0, len(w.conns))
	for c := range w.conns {
		conns = append(conns, c)
	}
	w.mu.Unlock()

	for _, c := range conns {
		c.push(b)
	}
}

func (w *Worker) setState(s wire.ProcessState) {
	w.mu.Lock()
	if w.state == s {
		// Not a transition. Repeating it would have the daemon announce a state
		// change to the Panel that did not happen, and the failure paths here
		// deliberately overlap -- a panic barrier setting "offline" over an error
		// path that already did should be silent.
		w.mu.Unlock()
		return
	}
	w.state = s
	pid := w.pidLocked()
	w.mu.Unlock()
	w.broadcast(wire.TypeState, wire.State{State: s, PID: pid})
}

func (w *Worker) pidLocked() int {
	if w.proc == nil {
		return 0
	}
	return w.proc.Pid
}

// Start launches the server process.
//
// Nothing a start request can do may be allowed to reach the top of the
// goroutine handling it. The worker holds this server's job object, and that job
// carries KILL_ON_JOB_CLOSE, so a panic here does not merely fail the request --
// it exits the worker, releases the job, and terminates a server that may have
// been running quite happily before the request arrived. A bad request must cost
// the request, not the server.
func (w *Worker) Start(p wire.Start) (err error) {
	// Registered first so that it runs last, after guard below has turned any
	// panic into an error. The ordinary failure paths already report offline;
	// setState ignores a repeat.
	defer func() {
		if err != nil {
			w.setState(wire.StateOffline)
		}
	}()
	defer w.guard("starting the server process", &err)

	return w.start(p)
}

func (w *Worker) start(p wire.Start) error {
	w.mu.Lock()
	if w.proc != nil {
		w.mu.Unlock()
		return fmt.Errorf("worker: process is already running")
	}
	w.mu.Unlock()

	w.setState(wire.StateStarting)

	// A run gets its own console log. Done before the process is launched so
	// that the file holds this run and nothing else, including the diagnostics
	// below if the launch fails.
	w.console.Roll()

	// Logged before anything is attempted, because every failure below leaves
	// the daemon with a bare error and no record of what was being run.
	w.Log(wire.LogInfo, "starting server process",
		"argv", strings.Join(p.Argv, " "),
		"dir", w.cfg.WorkingDir,
		"account", accountOrSelf(p.Username),
		"pseudo_console", p.PseudoConsole,
		"env_vars", len(p.Env))

	// The daemon resolved and split that command line; the worker only executes
	// it. Saying where argv[0] actually points separates "the startup command is
	// wrong" from "the startup command is right and the file is missing", which
	// are otherwise the same CreateProcess error.
	//
	// This asks the same function that Start will use, deliberately. A diagnostic
	// with its own idea of where the executable is can report a file that exists
	// while CreateProcess looks somewhere else entirely, which is precisely how
	// this lookup bug survived being logged.
	if len(p.Argv) > 0 {
		resolved, rerr := winproc.ResolveExecutable(p.Argv[0], w.cfg.WorkingDir, p.Env)
		if rerr != nil {
			w.Log(wire.LogWarn, "could not locate the startup executable",
				"argv0", p.Argv[0], "dir", w.cfg.WorkingDir, "error", rerr)
		} else {
			w.Log(wire.LogDebug, "located the startup executable",
				"argv0", p.Argv[0], "resolved", resolved)
		}
	}

	job, err := jobobject.Create()
	if err != nil {
		w.Log(wire.LogError, "could not create the job object", "error", err)
		w.setState(wire.StateOffline)
		return err
	}
	if err := job.SetLimits(limitsFromWire(p.Limits)); err != nil {
		w.Log(wire.LogError, "could not apply job object limits", "error", err)
		_ = job.Close()
		w.setState(wire.StateOffline)
		return err
	}

	var token windows.Token
	if p.Username != "" {
		token, err = winproc.LogonUser(p.Username, p.Password)
		if err != nil {
			w.Log(wire.LogError, "could not log on the server's account",
				"account", p.Username, "error", err)
			_ = job.Close()
			w.setState(wire.StateOffline)
			return err
		}
	}

	if len(p.PreStart) > 0 {
		return w.startPreStart(p, job, token)
	}
	return w.launchServer(p, job, token, false)
}

// processConfig is the launch configuration for one of a run's processes. The
// pre-start command and the server share everything but the command line.
func (w *Worker) processConfig(p wire.Start, argv []string, token windows.Token) winproc.Config {
	return winproc.Config{
		Argv:          argv,
		Dir:           w.cfg.WorkingDir,
		Env:           p.Env,
		Token:         token,
		PseudoConsole: p.PseudoConsole,
		Cols:          p.Cols,
		Rows:          p.Rows,
	}
}

// startPreStart runs the pre-start command -- a steamcmd update, typically --
// and arranges for the server to be launched once it finishes.
//
// It returns as soon as the command is running. The daemon is waiting on this
// request over the worker's only connection, and an update can take minutes,
// so it cannot be waited for here: the run stays in the starting state and
// watchPreStart carries it forward.
func (w *Worker) startPreStart(p wire.Start, job *jobobject.Job, token windows.Token) error {
	proc, err := winproc.Start(w.processConfig(p, p.PreStart, token), job)
	if err != nil {
		w.Log(wire.LogError, "could not start the pre-start command",
			"executable", p.PreStart[0], "error", err)
		w.abandon(job, token)
		return err
	}

	// Held as the current process so that a stop request during the update
	// finds something to act on.
	w.mu.Lock()
	w.job = job
	w.proc = proc
	w.startedAt = time.Now()
	w.exitCode = 0
	w.memoryLimitHit = false
	w.terminated = false
	w.stopping = false
	w.mu.Unlock()

	// Only the executable: the arguments may carry an account password.
	w.Log(wire.LogInfo, "running the pre-start command; the server starts when it finishes",
		"executable", p.PreStart[0], "pid", proc.Pid)

	go w.pumpConsole(proc)
	go w.watchPreStart(p, proc, job, token)
	return nil
}

// watchPreStart waits for the pre-start command and then launches the server,
// unless a stop arrived in the meantime.
func (w *Worker) watchPreStart(p wire.Start, proc *winproc.Process, job *jobobject.Job, token windows.Token) {
	// A panic here would otherwise leave the worker holding a process that has
	// gone, refusing every later start as "already running".
	var failure error
	defer func() {
		if failure != nil {
			if token != 0 {
				_ = token.Close()
			}
			w.finish(proc, job, 0)
		}
	}()
	defer w.guard("waiting for the pre-start command", &failure)

	code, err := proc.Wait()
	if err != nil {
		code = 0
	}

	w.mu.Lock()
	interrupted := w.stopping || w.terminated
	w.mu.Unlock()
	if interrupted {
		w.Log(wire.LogInfo, "the pre-start command was interrupted by a stop request; "+
			"the server will not be started", "exit_code", code)
		if token != 0 {
			_ = token.Close()
		}
		w.finish(proc, job, code)
		return
	}

	if code != 0 {
		// The container entrypoint did not check either. A failed update
		// usually leaves the previous files in place, and a server that starts
		// stale is more useful than one that refuses to.
		w.Log(wire.LogWarn, "the pre-start command failed; starting the server regardless",
			"exit_code", code)
	} else {
		w.Log(wire.LogInfo, "the pre-start command finished", "exit_code", code)
	}
	_ = proc.Close()

	// Reports its own failure.
	_ = w.launchServer(p, job, token, true)
}

// launchServer starts the server process into job and hands the run to the
// goroutines that follow it. The token is consumed. On failure the run is torn
// down and reported offline.
//
// afterPreStart says a pre-start command ran first, in which case the stop
// flags are live for this run and a set one means a stop arrived during the
// handover. Otherwise they are stale from the previous run and are reset.
func (w *Worker) launchServer(p wire.Start, job *jobobject.Job, token windows.Token, afterPreStart bool) error {
	proc, err := winproc.Start(w.processConfig(p, p.Argv, token), job)
	if token != 0 {
		_ = token.Close()
	}
	if err != nil {
		w.Log(wire.LogError, "could not start the server process",
			"executable", p.Argv[0], "error", err)
		w.abandon(job, 0)
		return err
	}

	startedAt := time.Now()

	w.mu.Lock()
	if afterPreStart && (w.stopping || w.terminated) {
		// A stop arrived while the pre-start command was handing over: what it
		// acted on has already gone, and this process must not outlive the
		// request that was meant to end the run.
		w.mu.Unlock()
		w.Log(wire.LogInfo, "a stop request arrived as the server was launching; killing it",
			"pid", proc.Pid)
		_ = proc.Kill()
		_ = proc.Close()
		w.abandon(job, 0)
		return fmt.Errorf("worker: start interrupted by a stop request")
	}
	w.job = job
	w.proc = proc
	w.startedAt = startedAt
	w.exitCode = 0
	w.memoryLimitHit = false
	w.terminated = false
	w.stopping = false
	w.mu.Unlock()

	w.Log(wire.LogInfo, "server process started", "pid", proc.Pid)
	w.setState(wire.StateRunning)

	// Everything spawned for this run is tied to it, not to the worker. The
	// stats pump in particular must end with the process: one that survived
	// would broadcast empty samples forever, and every later start would add
	// another, until the daemon was receiving several overlapping streams.
	done := make(chan struct{})

	go w.pumpConsole(proc)
	go w.watchExit(proc, job, done)
	go w.watchJobEvents(job)
	go w.pumpStats(job, startedAt, done)

	return nil
}

// abandon gives up on a run that never reached the running state. The job goes,
// killing anything still in it, the account token goes, and the daemon is told
// the server is offline.
func (w *Worker) abandon(job *jobobject.Job, token windows.Token) {
	w.mu.Lock()
	w.proc = nil
	w.job = nil
	w.startedAt = time.Time{}
	w.mu.Unlock()

	if token != 0 {
		_ = token.Close()
	}
	_ = job.Close()
	w.setState(wire.StateOffline)
}

// pumpConsole streams process output into the console buffer and out to every
// connected daemon.
func (w *Worker) pumpConsole(proc *winproc.Process) {
	defer w.guard("pumping console output", nil)

	buf := make([]byte, 32*1024)
	out := proc.Output()
	for {
		n, err := out.Read(buf)
		if n > 0 {
			seq := w.console.Append(buf[:n])
			w.broadcast(wire.TypeConsole, wire.Console{Sequence: seq, Data: buf[:n]})
		}
		if err != nil {
			return
		}
	}
}

// watchExit waits for the process to end, then tears the run down. Closing done
// tells everything else spawned for this run that it is over.
func (w *Worker) watchExit(proc *winproc.Process, job *jobobject.Job, done chan<- struct{}) {
	// The only thing that reports the process ending. If it dies quietly the
	// daemon waits forever on a server that has already gone, so a recovery here
	// has to say so even though it does not know the exit code.
	var failure error
	defer func() {
		if failure != nil {
			w.setState(wire.StateOffline)
		}
	}()
	defer w.guard("watching for the server process to exit", &failure)

	// Closed on every path out, including a panic, so that nothing tied to this
	// run can outlive it.
	defer close(done)

	code, err := proc.Wait()
	if err != nil {
		code = 0
	}
	w.finish(proc, job, code)
}

// finish tears a run down once its process has gone and tells the daemon how
// it ended.
func (w *Worker) finish(proc *winproc.Process, job *jobobject.Job, code uint32) {
	w.mu.Lock()
	w.exitCode = int(code)
	memHit := w.memoryLimitHit
	terminated := w.terminated
	w.proc = nil
	w.job = nil
	w.startedAt = time.Time{}
	w.mu.Unlock()

	_ = proc.Close()
	// Closing the job kills anything the server left behind, since the job is
	// created with kill-on-close.
	_ = job.Close()

	w.setState(wire.StateOffline)
	w.broadcast(wire.TypeExit, wire.Exit{
		Code:           int(code),
		MemoryLimitHit: memHit,
		Terminated:     terminated,
	})
}

func (w *Worker) watchJobEvents(job *jobobject.Job) {
	defer w.guard("watching job object events", nil)

	for ev := range job.Events() {
		if ev.MemoryLimitHit {
			w.mu.Lock()
			w.memoryLimitHit = true
			w.mu.Unlock()
		}
	}
}

// pumpStats pushes a resource sample for one run every stats interval, and
// ends when that run does.
//
// It samples the job it was given rather than whatever the worker currently
// holds, so a pump can never report the next run's process as this one's, and it
// returns on done rather than on the job going away, so a pump can never outlive
// its run. Both matter: a pump that carried on after exit used to broadcast
// empty samples indefinitely, and each restart stacked another on top of it.
func (w *Worker) pumpStats(job *jobobject.Job, startedAt time.Time, done <-chan struct{}) {
	defer w.guard("sampling resource usage", nil)

	t := time.NewTicker(time.Duration(w.cfg.StatsIntervalMS) * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-w.shutdown:
			return
		case <-done:
			return
		case <-t.C:
			s, err := sample(job, startedAt)
			if err != nil {
				// The job is closed once the process exits; a sample racing that
				// close fails here, which is the run ending.
				return
			}
			// A sample taken just before the exit must not be broadcast just
			// after it: the daemon would see a live reading for a server it has
			// already been told is offline.
			select {
			case <-done:
				return
			default:
			}
			w.broadcast(wire.TypeStats, s)
		}
	}
}

// Stats samples current resource usage.
func (w *Worker) Stats() (wire.Stats, error) {
	w.mu.Lock()
	job, startedAt := w.job, w.startedAt
	w.mu.Unlock()

	if job == nil {
		return wire.Stats{}, nil
	}
	return sample(job, startedAt)
}

// sample reads one resource sample from a job.
func sample(job *jobobject.Job, startedAt time.Time) (wire.Stats, error) {
	s, err := job.Stats()
	if err != nil {
		return wire.Stats{}, err
	}

	var uptime int64
	if !startedAt.IsZero() {
		uptime = time.Since(startedAt).Milliseconds()
	}

	// Best effort. The daemon attributes network traffic by these; without
	// them this sample carries no network figures, and the next one will.
	pids, _ := job.ProcessIDs()

	return wire.Stats{
		MemoryBytes:    s.MemoryBytes,
		CpuAbsolute:    s.CpuAbsolute,
		Processes:      s.ActiveProcs,
		UptimeMillis:   uptime,
		DiskReadBytes:  s.DiskReadBytes,
		DiskWriteBytes: s.DiskWriteBytes,
		PIDs:           pids,
	}, nil
}

// WriteStdin writes to the process's standard input.
func (w *Worker) WriteStdin(data []byte) error {
	w.mu.Lock()
	proc := w.proc
	w.mu.Unlock()

	if proc == nil {
		return fmt.Errorf("worker: process is not running")
	}
	if _, err := proc.Stdin().Write(data); err != nil {
		return fmt.Errorf("worker: write stdin: %w", err)
	}
	return nil
}

// UpdateLimits adjusts Job Object limits without restarting the process.
func (w *Worker) UpdateLimits(l wire.Limits) error {
	w.mu.Lock()
	job := w.job
	w.mu.Unlock()

	if job == nil {
		return fmt.Errorf("worker: process is not running")
	}
	return job.SetLimits(limitsFromWire(l))
}

// Stop brings the process down, escalating through the available mechanisms.
//
// The order matters and is not interchangeable. A stdin command is the only
// mechanism most game servers actually implement; CTRL_BREAK reaches everything
// sharing the console and is widely unhandled; terminating the job is immediate
// and unclean. Each step is given the full timeout before the next is tried.
//
// Every step is logged. A stop that goes wrong is close to impossible to
// diagnose after the fact — the process is gone either way — so the log has to
// say which mechanisms were tried, what each one did, and how long the process
// was given before the next was reached for.
func (w *Worker) Stop(p wire.Stop) {
	defer w.guard("stopping the server process", nil)

	started := time.Now()

	w.mu.Lock()
	proc := w.proc
	if proc == nil {
		w.mu.Unlock()
		w.Log(wire.LogInfo, "stop requested but no process is running; nothing to do",
			"mode", string(p.Mode))
		return
	}
	pid := proc.Pid
	w.stopping = true
	w.mu.Unlock()

	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
		w.Log(wire.LogDebug, "no stop timeout was supplied; using the worker's default",
			"timeout", timeout.String())
	}

	w.Log(wire.LogInfo, "stopping the server process",
		"mode", string(p.Mode), "command", p.Value, "timeout", timeout.String(), "pid", pid)

	w.setState(wire.StateStopping)

	switch p.Mode {
	case wire.StopCommand:
		if p.Value == "" {
			w.Log(wire.LogWarn, "the stop mode is a console command but no command was supplied; "+
				"escalating straight to ctrl+break")
		} else {
			w.Log(wire.LogInfo, "writing the stop command to the process's stdin",
				"command", p.Value)
			if err := w.WriteStdin([]byte(p.Value + "\r\n")); err != nil {
				w.Log(wire.LogWarn, "could not write the stop command to stdin",
					"command", p.Value, "error", err.Error())
			} else if w.awaitExit("the stop command", timeout, started) {
				return
			}

			// Not every server reads stdin. One that opens a console of its own
			// -- a modloader started with -console, say -- reads the console's
			// input buffer instead, which a redirected pipe never reaches. The
			// same text typed into that buffer is indistinguishable from somebody
			// at a keyboard, so it is worth a try before escalating to something
			// the server has to be killed by.
			w.Log(wire.LogInfo, "the server did not act on the stop command; typing it into "+
				"the console input buffer instead", "command", p.Value)
			if err := proc.TypeLine(p.Value); err != nil {
				w.Log(wire.LogWarn, "could not type the stop command into the console",
					"command", p.Value, "error", err.Error())
			} else if w.awaitExit("typing the stop command", timeout, started) {
				return
			}
		}
		fallthrough

	case wire.StopCtrlC:
		w.Log(wire.LogInfo, "sending ctrl+c to the server's console", "pid", pid)
		if err := proc.CtrlC(); err != nil {
			w.Log(wire.LogWarn, "could not send ctrl+c; this worker has no console to raise "+
				"an interrupt on, so the server can only be killed",
				"pid", pid, "error", err.Error())
		} else if w.awaitExit("ctrl+c", timeout, started) {
			return
		}
		fallthrough

	case wire.StopCtrlBreak:
		w.Log(wire.LogInfo, "sending ctrl+break to the process group", "pid", pid)
		if err := proc.CtrlBreak(); err != nil {
			// Nothing was delivered, so waiting out the timeout would achieve
			// nothing but delay the kill. Say plainly that no graceful mechanism
			// remains, because the next line is the server being killed and the
			// operator will read that as the daemon being trigger-happy.
			w.Log(wire.LogWarn, "could not send ctrl+break to the process group; no graceful "+
				"stop is available for this server. Give its egg a stop command in the "+
				"windows profile", "pid", pid, "error", err.Error())
		} else if w.awaitExit("ctrl+break", timeout, started) {
			return
		}
		fallthrough

	case wire.StopTerminate:
		w.Log(wire.LogWarn, "the process did not stop on its own; killing the job object",
			"pid", pid, "elapsed", elapsed(started))
		if err := w.Terminate(); err != nil {
			w.Log(wire.LogError, "failed to kill the job object; the process may still be running",
				"pid", pid, "error", err.Error())
			return
		}
		if w.waitForExit(5 * time.Second) {
			w.Log(wire.LogInfo, "the process was killed", "pid", pid, "elapsed", elapsed(started))
		} else {
			// Terminating a job object does not fail silently, so reaching here
			// means something is holding the process in the kernel: an
			// unkillable state, usually a stuck driver or an I/O wait.
			w.Log(wire.LogError, "the job object was killed but the process is still present",
				"pid", pid, "elapsed", elapsed(started))
		}
	}
}

// awaitExit waits out the graceful timeout for one stop mechanism, reporting
// whether it worked and logging either way.
func (w *Worker) awaitExit(what string, d time.Duration, started time.Time) bool {
	w.Log(wire.LogDebug, "waiting for the process to exit",
		"after", what, "timeout", d.String())
	if w.waitForExit(d) {
		w.Log(wire.LogInfo, "the process exited",
			"after", what, "elapsed", elapsed(started))
		return true
	}
	w.Log(wire.LogWarn, "the process is still running; escalating",
		"after", what, "waited", d.String(), "elapsed", elapsed(started))
	return false
}

// elapsed renders the time since the stop began, rounded to something a human
// reading a console wants to see.
func elapsed(started time.Time) string {
	return time.Since(started).Round(100 * time.Millisecond).String()
}

// waitForExit reports whether the process exited within d.
func (w *Worker) waitForExit(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		running := w.proc != nil
		w.mu.Unlock()
		if !running {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// Terminate kills the process tree immediately.
func (w *Worker) Terminate() error {
	w.mu.Lock()
	job := w.job
	pid := 0
	if w.proc != nil {
		pid = w.proc.Pid
	}
	w.terminated = true
	w.mu.Unlock()

	if job == nil {
		w.Log(wire.LogInfo, "terminate requested but no job object is held; nothing to kill")
		return nil
	}
	w.Log(wire.LogWarn, "terminating the server's process tree", "pid", pid)
	return job.Terminate(1)
}

// Shutdown stops the process and ends the worker.
func (w *Worker) Shutdown() {
	defer w.guard("shutting the worker down", nil)

	w.mu.Lock()
	running := w.proc != nil
	w.mu.Unlock()

	if running {
		w.Stop(wire.Stop{Mode: wire.StopTerminate})
	}
	w.once.Do(func() { close(w.shutdown) })
	_ = w.console.Close()
}

// Wait blocks until the worker is shut down.
func (w *Worker) Wait() { <-w.shutdown }

func limitsFromWire(l wire.Limits) jobobject.Limits {
	return jobobject.Limits{
		MemoryBytes:  l.MemoryBytes,
		CpuRate:      l.CpuRate,
		CpuHardCap:   l.CpuHardCap,
		AffinityMask: l.AffinityMask,
		ProcessLimit: l.ProcessLimit,
	}
}

var _ io.Closer = (*consoleBuffer)(nil)
