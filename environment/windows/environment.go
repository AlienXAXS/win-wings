//go:build windows

// Package windows implements environment.ProcessEnvironment on top of a
// per-server worker process.
//
// The daemon does not own game processes directly. Each server is supervised by
// a winwings-worker holding its Job Object and console handles; this package is
// the client side of that relationship. See internal/wire for why.
package windows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/internal/accounts"
	"github.com/pterodactyl/wings/internal/netstat"
	"github.com/pterodactyl/wings/internal/winacl"
	"github.com/pterodactyl/wings/internal/winfw"
	"github.com/pterodactyl/wings/internal/wire"
	"github.com/pterodactyl/wings/internal/worker"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/system"
)

// Ensure the Windows environment implements the full interface.
var _ environment.ProcessEnvironment = (*Environment)(nil)

// Metadata carries the per-server settings that came from the egg's Windows
// profile rather than from the server's build configuration.
type Metadata struct {
	// Runtime replaces Docker's image reference. It names what the server needs
	// present on the host — a JRE version, a .NET runtime, or nothing at all.
	// The Panel-side Blueprint plugin supplies it.
	Runtime string

	// Startup replaces the egg's startup command, which is written for a Linux
	// shell and generally cannot be made to work here. Empty means fall back to
	// the Panel's STARTUP value.
	//
	// This is held here rather than folded into the server's environment
	// variables because those are rebuilt from the Panel's configuration every
	// time the server is synced, which would put the Linux command straight back.
	Startup string

	// Stop describes how the server should be brought down gracefully.
	Stop remote.ProcessStopConfiguration

	// PseudoConsole requests a ConPTY rather than pipes for this egg. Needed only
	// by processes that detect a non-console stdout and change behaviour.
	PseudoConsole bool
}

// Environment supervises one server via its worker.
type Environment struct {
	mu sync.RWMutex

	// Id is the server UUID. It names the instance directory and the control pipe.
	Id string

	Configuration *environment.Configuration

	meta *Metadata

	client *worker.Client

	emitter *events.Bus
	st      *system.AtomicString

	logCallbackMx sync.Mutex
	logCallback   func([]byte)

	// lastSeq tracks console progress so a reconnect resumes without gaps.
	lastSeq uint64

	exitCode       uint32
	memoryLimitHit bool
	startedAt      time.Time

	// token authenticates this daemon to the worker.
	token string

	// watchdog guards against more than one reconnection attempt running at a
	// time. See watchdog.go.
	watchdog system.AtomicBool

	// spawnMu serialises worker creation. Held across the whole check-then-spawn
	// in connectOrSpawn, so two callers cannot both conclude there is no worker
	// and start one each. It is separate from mu because it is held across a
	// process launch, which mu must never be.
	spawnMu sync.Mutex

	// ctx bounds work that outlives a single call -- the reconnection watchdog
	// being the only such work today. Cancelled when the server is destroyed, so
	// that a watchdog does not keep talking about a server that no longer exists.
	ctx    context.Context
	cancel context.CancelFunc
}

// Context returns the environment's lifetime context.
func (e *Environment) Context() context.Context { return e.ctx }

// New creates a Windows environment for a server. The worker is not started
// here; that happens on Create.
func New(id string, m *Metadata, c *environment.Configuration) (*Environment, error) {
	if m == nil {
		m = &Metadata{}
	}
	token, err := workerToken(id)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Environment{
		Id:            id,
		Configuration: c,
		meta:          m,
		st:            system.NewAtomicString(environment.ProcessOfflineState),
		emitter:       events.NewBus(),
		token:         token,
		ctx:           ctx,
		cancel:        cancel,
	}, nil
}

// workerToken returns the shared secret that authenticates this daemon to the
// server's worker over its control pipe, reusing the one already on disk when
// there is one.
//
// The token belongs to the server, not to a run of the daemon. A worker reads it
// out of worker.json once at startup and holds it for its lifetime, and a worker
// outlives the daemon by design — that is the whole point of the split. Minting a
// fresh token on every boot therefore locked the daemon out of every worker that
// was still running: the handshake was rejected, the daemon concluded no worker
// was there, and it spawned a second one that could not have the pipe either. See
// connect.
//
// The pipe's ACL is the primary control — only the daemon's own account can
// connect at all. The token is defence in depth against another process running
// as that same account, and a persisted secret in a file only the daemon can read
// serves that just as well as a fresh one.
func workerToken(id string) (string, error) {
	if c, err := worker.LoadConfig(config.Get().System.ServerWorkerConfig(id)); err == nil && c.Token != "" {
		return c.Token, nil
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", errors.Wrap(err, "environment/windows: failed to generate worker token")
	}
	return hex.EncodeToString(b), nil
}

func (e *Environment) log() *log.Entry {
	return log.WithField("environment", e.Type()).WithField("server", e.Id)
}

// Type identifies this environment to the Panel.
func (e *Environment) Type() string { return "windows" }

// Events returns the environment's event bus.
func (e *Environment) Events() *events.Bus { return e.emitter }

// Config returns the environment configuration.
func (e *Environment) Config() *environment.Configuration {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Configuration
}

// SetStopConfiguration updates how the server is asked to stop.
func (e *Environment) SetStopConfiguration(c remote.ProcessStopConfiguration) {
	e.mu.Lock()
	e.meta.Stop = c
	e.mu.Unlock()
}

// SetRuntime updates the runtime selector for this server.
func (e *Environment) SetRuntime(r string) {
	e.mu.Lock()
	e.meta.Runtime = r
	e.mu.Unlock()
}

// SetStartup updates the Windows startup command override. Empty restores the
// fallback to the Panel's own STARTUP value.
func (e *Environment) SetStartup(s string) {
	e.mu.Lock()
	e.meta.Startup = s
	e.mu.Unlock()
}

// SetPseudoConsole updates whether this egg's process gets a ConPTY.
func (e *Environment) SetPseudoConsole(v bool) {
	e.mu.Lock()
	e.meta.PseudoConsole = v
	e.mu.Unlock()
}

// SetImage exists so callers written against the Docker environment keep
// working; the value is treated as a runtime selector.
func (e *Environment) SetImage(i string) { e.SetRuntime(i) }

// State returns the current process state.
func (e *Environment) State() string { return e.st.Load() }

// SetState records a state transition and notifies listeners.
func (e *Environment) SetState(state string) {
	if state != environment.ProcessOfflineState &&
		state != environment.ProcessStartingState &&
		state != environment.ProcessRunningState &&
		state != environment.ProcessStoppingState {
		panic(errors.New(fmt.Sprintf("invalid server state received: %s", state)))
	}

	if e.State() != state {
		e.st.Store(state)
		e.Events().Publish(environment.StateChangeEvent, state)
	}
}

// SetLogCallback sets the sink for console output.
func (e *Environment) SetLogCallback(f func([]byte)) {
	e.logCallbackMx.Lock()
	defer e.logCallbackMx.Unlock()
	e.logCallback = f
}

// ServerRoot is the server's top-level directory, which the daemon owns.
//
// It holds the worker configuration and console log alongside — not inside —
// the server's own files. A server able to write here could rewrite what its
// worker executes, so the sandbox is rooted one level down. See config/paths.go.
func (e *Environment) ServerRoot() string {
	return config.Get().System.ServerRoot(e.Id)
}

// tempDirectory is the server's scratch space, a sibling of its data directory.
func (e *Environment) tempDirectory() string {
	return config.Get().System.ServerTemp(e.Id)
}

// workingDirectory is the server's own files: the sandbox and SFTP root, and
// what the Panel thinks of as /home/container.
func (e *Environment) workingDirectory() string {
	return config.Get().System.ServerData(e.Id)
}

// Exists reports whether the server's worker environment has been created.
func (e *Environment) Exists() (bool, error) {
	if _, err := os.Stat(config.Get().System.ServerWorkerConfig(e.Id)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Create prepares the instance directory and worker configuration.
//
// It does not start the worker; Start does that, so that a server which is not
// running consumes no processes.
func (e *Environment) Create() error {
	cfg := config.Get()

	if err := os.MkdirAll(e.workingDirectory(), 0o700); err != nil {
		return errors.Wrap(err, "environment/windows: failed to create server data directory")
	}
	if err := os.MkdirAll(e.tempDirectory(), 0o700); err != nil {
		return errors.Wrap(err, "environment/windows: failed to create server temp directory")
	}

	wc := worker.Config{
		UUID:            e.Id,
		Token:           e.token,
		WorkingDir:      e.workingDirectory(),
		LogPath:         cfg.System.ServerConsoleLog(e.Id),
		ConsoleBacklog:  cfg.System.WebsocketLogCount * 2,
		MaxLogSizeMB:    cfg.Runtime.Console.MaxSize,
		MaxLogFiles:     cfg.Runtime.Console.MaxFiles,
		StatsIntervalMS: 2000,
	}
	if wc.ConsoleBacklog < 256 {
		wc.ConsoleBacklog = 256
	}

	if err := worker.WriteConfig(e.ServerRoot(), wc); err != nil {
		return errors.WrapIf(err, "environment/windows: failed to write worker configuration")
	}

	e.applyFirewall()

	if err := e.applyPermissions(); err != nil {
		// Not fatal. A daemon without the rights to set ACLs can still run
		// servers; they are simply not isolated from one another, which is the
		// same position as shared isolation and is already warned about at boot.
		e.log().WithField("error", err).
			Warn("could not apply per-server permissions; this server's files are not " +
				"isolated from other servers on this node")
	}

	return nil
}

// applyPermissions restricts the server's tree so its own account can write only
// its data directory.
//
// The layout puts worker.json and the console log alongside the server's files
// rather than inside them, but that only separates them if the ACL says so. A
// server able to write worker.json could rewrite the command its worker
// executes, which is arbitrary code execution as its own account.
func (e *Environment) applyPermissions() error {
	username, _, err := e.account()
	if err != nil {
		return err
	}
	if username == "" {
		// Shared isolation: there is no distinct account to grant, and the
		// operator has already been told servers are not separated.
		return nil
	}

	// Daemon-owned first, so the grants below cannot widen it.
	if err := winacl.DenyAll(e.ServerRoot()); err != nil {
		return err
	}
	if err := winacl.GrantExclusiveWrite(e.workingDirectory(), username); err != nil {
		return err
	}
	// TEMP sits outside the sandbox so that scratch files are not charged
	// against the user's quota or copied into backups, which means it needs a
	// grant of its own -- DenyAll above has just taken it away.
	return winacl.GrantExclusiveWrite(e.tempDirectory(), username)
}

// applyFirewall opens this server's allocated ports.
//
// Failure is logged rather than returned. A firewall rule the daemon could not
// write leaves the server unreachable, which is bad; refusing to create or start
// the server at all would be worse, and an operator managing rules by hand or
// through group policy is a supported configuration.
func (e *Environment) applyFirewall() {
	if !config.Get().System.Firewall.Manage {
		return
	}

	allocations := e.Config().Allocations()
	bindings := winfw.Binding(allocations.Bindings())
	if err := winfw.Apply(e.Id, "", bindings); err != nil {
		e.log().WithField("error", err).
			Warn("could not open this server's ports in the Windows Firewall; the server " +
				"will start but may be unreachable")
		return
	}
	if len(bindings) == 0 {
		// Not an error -- a server can legitimately have none -- but it is the
		// explanation for a server that starts, reports healthy, and that nobody
		// can connect to, so it is worth saying once per start.
		e.log().Warn("this server has no allocations, so no ports were opened in the " +
			"Windows Firewall; it will start but nothing outside this host can reach it")
		return
	}
	e.log().WithField("allocations", len(bindings)).
		Debug("opened this server's allocated ports in the Windows Firewall")
}

// Destroy stops the server and removes its worker state.
func (e *Environment) Destroy() error {
	// Stops the reconnection watchdog before the worker is shut down, so a
	// deliberate teardown is not mistaken for a worker that died.
	e.cancel()

	e.mu.RLock()
	c := e.client
	e.mu.RUnlock()

	if c != nil {
		_ = c.Shutdown()
		time.Sleep(250 * time.Millisecond)
		_ = c.Close()
	}

	e.mu.Lock()
	e.client = nil
	e.mu.Unlock()

	e.SetState(environment.ProcessOfflineState)
	netstat.Release(e.Id)

	// One tree per server, so removal is a single call. The server's files, its
	// worker configuration, its console log and any private runtime all go
	// together.
	if err := removeTree(e.ServerRoot()); err != nil {
		return err
	}

	// The account outlives the files unless it is removed explicitly, and a node
	// that has churned through servers would otherwise accumulate one dormant
	// local account per server it has ever hosted.
	//
	// Deliberately after the files: an account still holding open handles is a
	// reason to keep it, and a failure here should not leave the tree behind.
	if err := accounts.Release(e.Id); err != nil {
		return errors.Wrap(err, "environment/windows: failed to remove the server's account")
	}

	// Firewall rules outlive the daemon, so a deleted server whose rules were
	// left behind holds its ports open until somebody notices. Not fatal: the
	// server is gone either way, and Prune at boot catches what this misses.
	if config.Get().System.Firewall.Manage {
		if err := winfw.Remove(e.Id); err != nil {
			e.log().WithField("error", err).
				Warn("could not close this server's ports in the Windows Firewall")
		}
	}
	return nil
}

// removeTree deletes a server's directory, retrying while something still holds
// a handle on it.
//
// Windows refuses to remove a directory that any process has open, and a
// deletion arriving moments after a server was stopped races several things that
// are on their way out: the worker process, whose working directory is the
// server's own data directory; whatever the server itself spawned; and the
// virus scanner that woke up when they exited. All of them clear in well under a
// second, so a short retry converts a spurious failure into a slight delay.
//
// It does not retry forever. A handle that is still held after this is held by
// something that is not leaving, and the operator needs to be told rather than
// have the request hang.
func removeTree(path string) error {
	const attempts = 10

	var err error
	for i := 0; i < attempts; i++ {
		if err = os.RemoveAll(path); err == nil || os.IsNotExist(err) {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 100 * time.Millisecond)
	}

	return errors.Wrapf(err,
		"environment/windows: could not remove %s after %d attempts. Something still has a "+
			"handle open on it -- most often this server's own worker process, or a virus "+
			"scanner. `handle64.exe %s` or Resource Monitor's Associated Handles search will "+
			"name it; note that the daemon itself will not appear in Explorer as holding the "+
			"directory even when it is",
		path, attempts, path)
}

// IsRunning reports whether the supervised process is alive.
func (e *Environment) IsRunning(ctx context.Context) (bool, error) {
	c, err := e.connect(ctx)
	if err != nil {
		// No worker means nothing is running, which is not an error condition.
		return false, nil
	}
	st := c.Hello().State
	return st == wire.StateRunning || st == wire.StateStarting || st == wire.StateStopping, nil
}

// ExitState returns the last exit code and whether the memory limit was hit.
//
// The second value stands in for Docker's OOMKilled flag. A Job Object memory
// cap does not reap anything — allocations simply start failing — so this
// reports that the cap was reached before the process ended, which is the
// closest signal available.
func (e *Environment) ExitState() (uint32, bool, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.exitCode, e.memoryLimitHit, nil
}

// Uptime returns how long the current process has been running, in ms.
func (e *Environment) Uptime(ctx context.Context) (int64, error) {
	e.mu.RLock()
	startedAt := e.startedAt
	e.mu.RUnlock()

	if startedAt.IsZero() {
		return 0, nil
	}
	return time.Since(startedAt).Milliseconds(), nil
}

// InSituUpdate applies changed resource limits to a running server.
func (e *Environment) InSituUpdate() error {
	e.mu.RLock()
	c := e.client
	e.mu.RUnlock()

	if c == nil {
		return nil
	}
	if err := c.UpdateLimits(e.Config().Limits().AsJobLimits()); err != nil {
		// A server that is not running has no job to update; that is not a failure.
		e.log().WithField("error", err).Debug("in-situ update skipped")
	}
	return nil
}

// Readlog returns the last n lines of the server's console log.
func (e *Environment) Readlog(lines int) ([]string, error) {
	path := config.Get().System.ServerConsoleLog(e.Id)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Read a bounded tail rather than the whole file: a busy server's log can be
	// megabytes and only the last few lines are ever wanted.
	const maxTail = 256 * 1024
	size := st.Size()
	offset := int64(0)
	if size > maxTail {
		offset = size - maxTail
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}

	buf := make([]byte, size-offset)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return []string{}, nil
	}

	split := strings.Split(strings.ReplaceAll(string(buf[:n]), "\r\n", "\n"), "\n")
	if len(split) > lines {
		split = split[len(split)-lines:]
	}
	return split, nil
}

// SendCommand writes a command to the server's standard input.
func (e *Environment) SendCommand(c string) error {
	e.mu.RLock()
	cl := e.client
	e.mu.RUnlock()

	if cl == nil {
		return errors.New("environment/windows: server is not running")
	}

	// Refuse to relay the configured stop command through the generic command
	// path. Upstream did the same: the Panel needs to see a stop go through the
	// power flow so crash detection is not triggered by an intentional shutdown.
	e.mu.RLock()
	stop := e.meta.Stop
	e.mu.RUnlock()
	if stop.Type == remote.ProcessStopCommand && c == stop.Value {
		e.SetState(environment.ProcessStoppingState)
	}

	return cl.Stdin([]byte(c + "\r\n"))
}
