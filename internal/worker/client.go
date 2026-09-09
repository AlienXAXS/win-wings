//go:build windows

package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/internal/wire"
)

// Handlers receive unsolicited messages pushed by a worker.
//
// Each is invoked on the client's read goroutine, so an implementation that
// blocks stalls every other message from that worker. Hand work off if it might
// be slow.
type Handlers struct {
	Console func(wire.Console)
	State   func(wire.State)
	Stats   func(wire.Stats)
	Exit    func(wire.Exit)
	// Disconnected is called once when the connection drops for any reason.
	Disconnected func(error)
}

// Client is the daemon's connection to one worker.
type Client struct {
	conn net.Conn
	w    *bufio.Writer

	hello wire.Hello

	mu       sync.Mutex
	nextID   atomic.Uint64
	pending  map[uint64]chan wire.Envelope
	handlers Handlers

	writeMu sync.Mutex
	closed  atomic.Bool
}

// SpawnWorker launches a detached worker for a server and returns once its
// control pipe is accepting connections.
//
// The worker is deliberately not a child in any meaningful sense: it is given no
// inherited handles and its lifetime is independent of the daemon's, so that
// restarting win-wings does not disturb running servers.
func SpawnWorker(ctx context.Context, exePath, instanceDir, uuid string, timeout time.Duration) error {
	cmd := exec.Command(exePath, instanceDir)
	cmd.Dir = instanceDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// DETACHED_PROCESS keeps the worker off the daemon's console, and
		// CREATE_BREAKAWAY_FROM_JOB stops it being swept up if the daemon itself
		// is ever placed in a job (which service hosts and CI runners do).
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_BREAKAWAY_FROM_JOB,
		HideWindow:    true,
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("worker: spawn %s: %w", exePath, err)
	}
	// Release the process handle; the worker is not ours to wait on.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("worker: release spawned process: %w", err)
	}

	return WaitForPipe(ctx, uuid, timeout)
}

// WaitForPipe blocks until a worker's control pipe exists or the timeout passes.
func WaitForPipe(ctx context.Context, uuid string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pipe := wire.PipeName(uuid)

	for {
		if _, err := os.Stat(pipe); err == nil {
			return nil
		}
		// os.Stat on a pipe path is unreliable; a dial attempt is definitive.
		c, err := winio.DialPipeContext(ctx, pipe)
		if err == nil {
			_ = c.Close()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("worker: timed out waiting for %s", pipe)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Dial connects to a running worker and completes the handshake.
//
// since is the last console sequence number the caller already has; the worker
// replays anything newer, so a reconnecting daemon does not lose output that
// arrived while it was away.
func Dial(ctx context.Context, uuid, token string, since uint64, h Handlers) (*Client, error) {
	conn, err := winio.DialPipeContext(ctx, wire.PipeName(uuid))
	if err != nil {
		return nil, fmt.Errorf("worker: dial %s: %w", wire.PipeName(uuid), err)
	}

	c := &Client{
		conn:     conn,
		w:        bufio.NewWriterSize(conn, 32*1024),
		pending:  make(map[uint64]chan wire.Envelope),
		handlers: h,
	}

	reply, err := c.request(wire.TypeHandshake, wire.Handshake{
		Version: wire.ProtocolVersion,
		Token:   token,
		Since:   since,
	}, func() { go c.readLoop() })
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if reply.Type == wire.TypeError {
		var e wire.Error
		_ = wire.Unmarshal(reply, &e)
		_ = conn.Close()
		return nil, fmt.Errorf("worker: handshake rejected: %s", e.Message)
	}
	if err := wire.Unmarshal(reply, &c.hello); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if c.hello.Version != wire.ProtocolVersion {
		_ = conn.Close()
		return nil, fmt.Errorf(
			"worker: protocol version mismatch: daemon speaks %d, worker speaks %d",
			wire.ProtocolVersion, c.hello.Version)
	}

	return c, nil
}

// Hello returns the worker's state as reported at connection time.
func (c *Client) Hello() wire.Hello { return c.hello }

func (c *Client) readLoop() {
	r := bufio.NewReaderSize(c.conn, 64*1024)
	var err error

	defer func() {
		c.closed.Store(true)
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if c.handlers.Disconnected != nil {
			c.handlers.Disconnected(err)
		}
	}()

	for {
		var line []byte
		line, err = r.ReadBytes('\n')
		if err != nil {
			return
		}

		var env wire.Envelope
		if e := json.Unmarshal(line, &env); e != nil {
			// A malformed frame is not worth dropping the connection over.
			continue
		}

		if env.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[env.ID]
			delete(c.pending, env.ID)
			c.mu.Unlock()
			if ok {
				ch <- env
				close(ch)
				continue
			}
		}

		c.handle(env)
	}
}

func (c *Client) handle(env wire.Envelope) {
	switch env.Type {
	case wire.TypeConsole:
		if c.handlers.Console != nil {
			var p wire.Console
			if wire.Unmarshal(env, &p) == nil {
				c.handlers.Console(p)
			}
		}
	case wire.TypeState:
		if c.handlers.State != nil {
			var p wire.State
			if wire.Unmarshal(env, &p) == nil {
				c.handlers.State(p)
			}
		}
	case wire.TypeStats:
		if c.handlers.Stats != nil {
			var p wire.Stats
			if wire.Unmarshal(env, &p) == nil {
				c.handlers.Stats(p)
			}
		}
	case wire.TypeExit:
		if c.handlers.Exit != nil {
			var p wire.Exit
			if wire.Unmarshal(env, &p) == nil {
				c.handlers.Exit(p)
			}
		}
	}
}

// send writes a message without waiting for a reply.
func (c *Client) send(t wire.MessageType, id uint64, payload any) error {
	if c.closed.Load() {
		return fmt.Errorf("worker: connection is closed")
	}
	b, err := wire.Marshal(t, id, payload)
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.w.Write(b); err != nil {
		return fmt.Errorf("worker: write: %w", err)
	}
	return c.w.Flush()
}

// request sends a message and waits for the correlated reply.
//
// afterRegister runs once the reply channel is registered but before the message
// is sent, so the handshake can start its read loop without racing its own
// response.
func (c *Client) request(t wire.MessageType, payload any, afterRegister func()) (wire.Envelope, error) {
	id := c.nextID.Add(1)
	ch := make(chan wire.Envelope, 1)

	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if afterRegister != nil {
		afterRegister()
	}

	if err := c.send(t, id, payload); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return wire.Envelope{}, err
	}

	select {
	case env, ok := <-ch:
		if !ok {
			return wire.Envelope{}, fmt.Errorf("worker: connection closed awaiting %s reply", t)
		}
		return env, nil
	case <-time.After(30 * time.Second):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return wire.Envelope{}, fmt.Errorf("worker: timed out awaiting %s reply", t)
	}
}

// call sends a message and turns an error reply into a Go error.
func (c *Client) call(t wire.MessageType, payload any) error {
	env, err := c.request(t, payload, nil)
	if err != nil {
		return err
	}
	if env.Type == wire.TypeError {
		var e wire.Error
		_ = wire.Unmarshal(env, &e)
		return fmt.Errorf("worker: %s", e.Message)
	}
	return nil
}

// Start launches the server process.
func (c *Client) Start(p wire.Start) error { return c.call(wire.TypeStart, p) }

// Stdin writes to the process's standard input.
func (c *Client) Stdin(data []byte) error {
	return c.send(wire.TypeStdin, 0, wire.Stdin{Data: data})
}

// Stop requests a graceful shutdown. It returns as soon as the request is
// accepted; completion is observed through the State and Exit handlers.
func (c *Client) Stop(p wire.Stop) error { return c.send(wire.TypeStop, 0, p) }

// Terminate kills the process tree immediately.
func (c *Client) Terminate() error { return c.send(wire.TypeTerminate, 0, struct{}{}) }

// UpdateLimits adjusts resource limits on a running process.
func (c *Client) UpdateLimits(l wire.Limits) error {
	return c.call(wire.TypeUpdateLimits, wire.UpdateLimits{Limits: l})
}

// Stats requests an immediate resource sample.
func (c *Client) Stats() (wire.Stats, error) {
	env, err := c.request(wire.TypeStatsRequest, struct{}{}, nil)
	if err != nil {
		return wire.Stats{}, err
	}
	if env.Type == wire.TypeError {
		var e wire.Error
		_ = wire.Unmarshal(env, &e)
		return wire.Stats{}, fmt.Errorf("worker: %s", e.Message)
	}
	var s wire.Stats
	return s, wire.Unmarshal(env, &s)
}

// Shutdown asks the worker to stop the process and exit.
func (c *Client) Shutdown() error { return c.send(wire.TypeShutdown, 0, struct{}{}) }

// Close drops the connection without affecting the worker or its server.
func (c *Client) Close() error {
	c.closed.Store(true)
	return c.conn.Close()
}

// WriteConfig writes a worker configuration into an instance directory.
func WriteConfig(instanceDir string, cfg Config) error {
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		return fmt.Errorf("worker: create instance directory: %w", err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("worker: marshal config: %w", err)
	}
	path := filepath.Join(instanceDir, "worker.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("worker: write config: %w", err)
	}
	return nil
}
