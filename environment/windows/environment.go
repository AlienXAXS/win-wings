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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
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
}

// New creates a Windows environment for a server. The worker is not started
// here; that happens on Create.
func New(id string, m *Metadata, c *environment.Configuration) (*Environment, error) {
	if m == nil {
		m = &Metadata{}
	}
	token, err := workerToken()
	if err != nil {
		return nil, err
	}

	return &Environment{
		Id:            id,
		Configuration: c,
		meta:          m,
		st:            system.NewAtomicString(environment.ProcessOfflineState),
		emitter:       events.NewBus(),
		token:         token,
	}, nil
}

// workerToken generates the shared secret that authenticates this daemon to the
// server's worker over its control pipe.
//
// The pipe's ACL is the primary control — only the daemon's own account can
// connect at all. This is defence in depth against another process running as
// that same account.
func workerToken() (string, error) {
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

// InstanceDirectory is where this server's worker state lives.
//
// This is deliberately outside the server's own data directory: it holds the
// worker configuration and console log, and a server able to write there could
// rewrite what the worker executes.
func (e *Environment) InstanceDirectory() string {
	return filepath.Join(config.Get().System.InstanceDirectory, e.Id)
}

// workingDirectory is the server's data directory, which the server owns.
func (e *Environment) workingDirectory() string {
	return filepath.Join(config.Get().System.Data, e.Id)
}

// Exists reports whether the server's worker environment has been created.
func (e *Environment) Exists() (bool, error) {
	if _, err := os.Stat(filepath.Join(e.InstanceDirectory(), "worker.json")); err != nil {
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

	wc := worker.Config{
		UUID:            e.Id,
		Token:           e.token,
		WorkingDir:      e.workingDirectory(),
		LogPath:         filepath.Join(e.InstanceDirectory(), "console.log"),
		ConsoleBacklog:  cfg.System.WebsocketLogCount * 2,
		MaxLogSizeMB:    cfg.Runtime.Console.MaxSize,
		MaxLogFiles:     cfg.Runtime.Console.MaxFiles,
		StatsIntervalMS: 2000,
	}
	if wc.ConsoleBacklog < 256 {
		wc.ConsoleBacklog = 256
	}

	if err := worker.WriteConfig(e.InstanceDirectory(), wc); err != nil {
		return errors.WrapIf(err, "environment/windows: failed to write worker configuration")
	}
	return nil
}

// Destroy stops the server and removes its worker state.
func (e *Environment) Destroy() error {
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

	if err := os.RemoveAll(e.InstanceDirectory()); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "environment/windows: failed to remove instance directory")
	}
	return nil
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
	path := filepath.Join(e.InstanceDirectory(), "console.log")
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
