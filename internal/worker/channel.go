//go:build windows

package worker

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pterodactyl/wings/internal/wire"
)

// The command channel: a TCP console for servers that do not read stdin.
//
// Several games -- Empyrion is the one this was written for -- take console
// commands only over a port they open themselves, and their Linux eggs reach it
// by running a telnet client with the container's stdin on one end. The client
// is doing nothing a socket cannot: write the line, print whatever comes back.
// So there is no client here, just the socket, which keeps the game process the
// process the worker supervises rather than putting a shell pipeline in the way.
//
// Everything the server sends is put on the console, because the answer to a
// command belongs next to the command. That includes the console's own banner
// and prompt, which is noise, but it is the noise an operator expects to see and
// it is how you tell a connected channel from a silent one.

const (
	// channelDialTimeout bounds one connection attempt. The port is on this host,
	// so a slow attempt means a firewall dropping rather than refusing, and there
	// is no point waiting out a TCP timeout when another attempt follows.
	channelDialTimeout = 5 * time.Second

	// channelRetryInterval is how often the port is retried while the server is
	// still opening it.
	channelRetryInterval = time.Second

	// channelDefaultConnectTimeout is how long the port is waited for when the
	// profile does not say. Generous because it is a world-loading time, not a
	// network timeout: a large save can take minutes before the console opens.
	channelDefaultConnectTimeout = 5 * time.Minute

	// channelWriteWait is how long a command waits for a channel that is not
	// connected yet. Short: the caller is either an operator who has just typed
	// something, or a stop that has its own timeout to spend.
	channelWriteWait = 10 * time.Second
)

// commandChannel is one run's connection to a server's TCP console.
type commandChannel struct {
	cfg  wire.CommandChannel
	w    *Worker
	addr string

	mu   sync.Mutex
	conn net.Conn

	// ready is closed on the first successful connection. Commands wait on it,
	// so one typed while the world is still loading is delivered when the console
	// opens rather than failing.
	ready     chan struct{}
	readyOnce sync.Once

	// gone is closed when the channel is shut down, so a command waiting on ready
	// is released rather than waiting out its full timeout after the run ends.
	gone     chan struct{}
	goneOnce sync.Once
}

// newCommandChannel starts a channel for a run. It returns immediately; the
// connection is made in the background, because the port does not exist until
// the server has finished starting.
func newCommandChannel(w *Worker, cfg wire.CommandChannel, done <-chan struct{}) *commandChannel {
	host := strings.TrimSpace(cfg.Host)
	if host == "" {
		host = "127.0.0.1"
	}

	c := &commandChannel{
		cfg:   cfg,
		w:     w,
		addr:  net.JoinHostPort(host, strconv.Itoa(cfg.Port)),
		ready: make(chan struct{}),
		gone:  make(chan struct{}),
	}

	go c.run(done)
	return c
}

// run connects, streams, and reconnects for as long as the run lasts.
func (c *commandChannel) run(done <-chan struct{}) {
	defer c.w.guard("running the server's command channel", nil)
	defer c.shutdown()

	timeout := time.Duration(c.cfg.ConnectTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = channelDefaultConnectTimeout
	}

	c.w.Log(wire.LogInfo, "waiting for the server's command console",
		"address", c.addr, "timeout", timeout.String())

	first := true
	for {
		conn, err := c.dial(timeout, done)
		if err != nil {
			// Said as a warning rather than an error: the server is running and
			// perfectly playable, it just cannot be sent commands. Both wordings
			// end the same way, and neither may be silent -- a channel that gives
			// up without saying so leaves an operator typing into a console that
			// answers nothing.
			if first {
				// Nearly always the port in the profile not being the one the egg's
				// console actually listens on.
				c.w.Log(wire.LogWarn, "could not reach the server's command console; commands "+
					"and the stop command have nowhere to go. Check the port in this egg's "+
					"windows profile", "address", c.addr, "error", err.Error())
			} else {
				c.w.Log(wire.LogWarn, "the server's command console did not come back; this "+
					"server can no longer be sent commands, and its stop will become a kill",
					"address", c.addr, "error", err.Error())
			}
			return
		}
		first = false

		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()

		c.w.Log(wire.LogInfo, "connected to the server's command console", "address", c.addr)
		c.readyOnce.Do(func() { close(c.ready) })

		if c.cfg.Password != "" {
			// Not logged, and sent before anything is read: these consoles prompt
			// for the password and discard whatever arrives before it.
			if err := c.write([]byte(c.cfg.Password + "\r\n")); err != nil {
				c.w.Log(wire.LogWarn, "could not send the command console password",
					"address", c.addr, "error", err.Error())
			}
		}

		err = c.pump(conn)

		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		_ = conn.Close()

		select {
		case <-done:
			return
		case <-c.gone:
			return
		default:
		}

		// The run is still going, so the console dropping is not the end of it.
		// Empyrion's closes on some commands, and a channel that stayed down after
		// the first one would leave a running server permanently uncommandable.
		c.w.Log(wire.LogInfo, "the server's command console disconnected; reconnecting",
			"address", c.addr, "reason", reasonOf(err))

		select {
		case <-time.After(channelRetryInterval):
		case <-done:
			return
		case <-c.gone:
			return
		}
	}
}

// dial retries the port until it answers or the connect timeout elapses.
func (c *commandChannel) dial(timeout time.Duration, done <-chan struct{}) (net.Conn, error) {
	deadline := time.Now().Add(timeout)

	var lastErr error
	for {
		select {
		case <-done:
			return nil, fmt.Errorf("the server stopped before its command console opened")
		case <-c.gone:
			return nil, fmt.Errorf("the command channel was shut down")
		case <-c.w.shutdown:
			return nil, fmt.Errorf("the worker is shutting down")
		default:
		}

		conn, err := net.DialTimeout("tcp", c.addr, channelDialTimeout)
		if err == nil {
			return conn, nil
		}
		lastErr = err

		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("gave up after %s: %w", timeout, lastErr)
		}

		select {
		case <-time.After(channelRetryInterval):
		case <-done:
			return nil, fmt.Errorf("the server stopped before its command console opened")
		case <-c.gone:
			return nil, fmt.Errorf("the command channel was shut down")
		}
	}
}

// pump reads the console until the connection ends, putting everything it sends
// onto the server console and answering its telnet negotiation.
func (c *commandChannel) pump(conn net.Conn) error {
	var filter telnetFilter
	buf := make([]byte, 16*1024)

	for {
		n, err := conn.Read(buf)
		if n > 0 {
			text, replies := filter.filter(buf[:n])
			if len(replies) > 0 {
				// Best effort: a console that negotiates and then ignores the answer
				// is still usable, and failing the read here would drop the channel
				// over something cosmetic.
				_, _ = conn.Write(replies)
			}
			if len(text) > 0 {
				c.w.emitConsole(text)
			}
		}
		if err != nil {
			return err
		}
	}
}

// Write sends one command, waiting for the channel to connect if the server is
// still opening its console.
func (c *commandChannel) Write(data []byte) error {
	select {
	case <-c.ready:
	case <-c.gone:
		return fmt.Errorf("worker: the command channel to %s is closed", c.addr)
	case <-time.After(channelWriteWait):
		return fmt.Errorf("worker: the server's command console at %s has not opened yet, "+
			"so there is nowhere to send this", c.addr)
	}
	return c.write(data)
}

func (c *commandChannel) write(data []byte) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("worker: the command channel to %s is not connected", c.addr)
	}
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("worker: write to the command channel at %s: %w", c.addr, err)
	}
	return nil
}

// Connected reports whether commands can currently be delivered.
func (c *commandChannel) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// shutdown ends the channel and releases anything waiting on it.
func (c *commandChannel) shutdown() {
	c.goneOnce.Do(func() { close(c.gone) })

	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
}

func reasonOf(err error) string {
	if err == nil {
		return "the console closed the connection"
	}
	return err.Error()
}

// --- Telnet option negotiation ----------------------------------------------

// The subset of RFC 854 a game console needs.
//
// These servers are not really telnet servers; they listen on a socket and some
// of them send an option negotiation at the start because the reference client
// people connect with expects one. Nothing here wants an option -- not echo, not
// binary mode, not window size -- so every offer is refused and every demand is
// declined, which leaves a plain line-oriented byte stream. That is exactly what
// `telnet -E` in the Linux eggs settles on too.
//
// Refusing rather than ignoring matters: a server that has sent DO and had no
// answer will keep sending it, and those bytes would otherwise be printed to the
// console as mojibake once per second.
const (
	iac  = 255 // interpret as command
	dont = 254
	do   = 253
	wont = 252
	will = 251
	sb   = 250 // subnegotiation begin
	se   = 240 // subnegotiation end
)

// telnetFilter strips command sequences from a stream and produces the replies
// they call for. It is stateful because a sequence can be split across reads.
type telnetFilter struct {
	// pending holds the bytes of an incomplete command sequence, IAC first.
	pending []byte
	// inSub is set while inside a subnegotiation, which runs until IAC SE.
	inSub bool
}

func (f *telnetFilter) filter(in []byte) (text, replies []byte) {
	for _, b := range in {
		switch {
		case len(f.pending) > 0:
			f.pending = append(f.pending, b)
			out, reply, done := f.consume()
			if done {
				text = append(text, out...)
				replies = append(replies, reply...)
				f.pending = nil
			}

		case f.inSub:
			if b == iac {
				f.pending = []byte{iac}
			}
			// Everything else inside a subnegotiation is the option's own payload
			// and is not console output.

		case b == iac:
			f.pending = []byte{iac}

		default:
			text = append(text, b)
		}
	}
	return text, replies
}

// consume interprets f.pending, reporting whether it is now a complete sequence.
func (f *telnetFilter) consume() (text, reply []byte, done bool) {
	p := f.pending
	if len(p) < 2 {
		return nil, nil, false
	}

	switch p[1] {
	case iac:
		if f.inSub {
			// An escaped 255 inside a subnegotiation is payload, not output.
			return nil, nil, true
		}
		// IAC IAC is a literal 255 byte in the stream.
		return []byte{iac}, nil, true

	case se:
		f.inSub = false
		return nil, nil, true

	case sb:
		f.inSub = true
		return nil, nil, true

	case will, wont, do, dont:
		if len(p) < 3 {
			return nil, nil, false
		}
		option := p[2]
		switch p[1] {
		case will, wont:
			// It offers to do something. It should not.
			return nil, []byte{iac, dont, option}, true
		default:
			// It asks us to do something. We will not.
			return nil, []byte{iac, wont, option}, true
		}

	default:
		// Every other command -- NOP, break, are-you-there and the rest -- is two
		// bytes and needs no answer.
		return nil, nil, true
	}
}
