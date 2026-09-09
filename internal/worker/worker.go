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
	c := &conn{Conn: nc, w: w, send: make(chan []byte, 256), done: make(chan struct{})}
	defer c.close()

	go func() {
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

func (w *Worker) dispatch(c *conn, env wire.Envelope) {
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
func (w *Worker) Start(p wire.Start) error {
	w.mu.Lock()
	if w.proc != nil {
		w.mu.Unlock()
		return fmt.Errorf("worker: process is already running")
	}
	w.mu.Unlock()

	w.setState(wire.StateStarting)

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
	if len(p.Argv) > 0 {
		resolved, note := locateExecutable(p.Argv[0], w.cfg.WorkingDir, p.Env)
		w.Log(wire.LogDebug, "resolved the startup executable",
			"argv0", p.Argv[0], "resolved", resolved, "how", note)
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

	cfg := winproc.Config{
		Argv:          p.Argv,
		Dir:           w.cfg.WorkingDir,
		Env:           p.Env,
		PseudoConsole: p.PseudoConsole,
		Cols:          p.Cols,
		Rows:          p.Rows,
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
		cfg.Token = token
	}

	proc, err := winproc.Start(cfg, job)
	if err != nil {
		w.Log(wire.LogError, "could not start the server process",
			"executable", p.Argv[0], "error", err)
		if token != 0 {
			_ = token.Close()
		}
		_ = job.Close()
		w.setState(wire.StateOffline)
		return err
	}
	if token != 0 {
		_ = token.Close()
	}

	w.mu.Lock()
	w.job = job
	w.proc = proc
	w.startedAt = time.Now()
	w.exitCode = 0
	w.memoryLimitHit = false
	w.terminated = false
	w.stopping = false
	w.mu.Unlock()

	w.Log(wire.LogInfo, "server process started", "pid", proc.Pid)
	w.setState(wire.StateRunning)

	go w.pumpConsole(proc)
	go w.watchExit(proc, job)
	go w.watchJobEvents(job)
	go w.pumpStats()

	return nil
}

// pumpConsole streams process output into the console buffer and out to every
// connected daemon.
func (w *Worker) pumpConsole(proc *winproc.Process) {
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

func (w *Worker) watchExit(proc *winproc.Process, job *jobobject.Job) {
	code, err := proc.Wait()
	if err != nil {
		code = 0
	}

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
	for ev := range job.Events() {
		if ev.MemoryLimitHit {
			w.mu.Lock()
			w.memoryLimitHit = true
			w.mu.Unlock()
		}
	}
}

func (w *Worker) pumpStats() {
	t := time.NewTicker(time.Duration(w.cfg.StatsIntervalMS) * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-w.shutdown:
			return
		case <-t.C:
			s, err := w.Stats()
			if err != nil {
				return
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

	s, err := job.Stats()
	if err != nil {
		return wire.Stats{}, err
	}

	var uptime int64
	if !startedAt.IsZero() {
		uptime = time.Since(startedAt).Milliseconds()
	}

	return wire.Stats{
		MemoryBytes:    s.MemoryBytes,
		CpuAbsolute:    s.CpuAbsolute,
		Processes:      s.ActiveProcs,
		UptimeMillis:   uptime,
		DiskReadBytes:  s.DiskReadBytes,
		DiskWriteBytes: s.DiskWriteBytes,
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
func (w *Worker) Stop(p wire.Stop) {
	w.mu.Lock()
	proc := w.proc
	if proc == nil {
		w.mu.Unlock()
		return
	}
	w.stopping = true
	w.mu.Unlock()

	w.setState(wire.StateStopping)

	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	switch p.Mode {
	case wire.StopCommand:
		if p.Value != "" {
			_ = w.WriteStdin([]byte(p.Value + "\r\n"))
			if w.waitForExit(timeout) {
				return
			}
		}
		fallthrough

	case wire.StopCtrlBreak:
		if err := proc.CtrlBreak(); err == nil {
			if w.waitForExit(timeout) {
				return
			}
		}
		fallthrough

	case wire.StopTerminate:
		_ = w.Terminate()
	}
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
	w.terminated = true
	w.mu.Unlock()

	if job == nil {
		return nil
	}
	return job.Terminate(1)
}

// Shutdown stops the process and ends the worker.
func (w *Worker) Shutdown() {
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
