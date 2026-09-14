//go:build windows

package worker

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pterodactyl/wings/internal/wire"
)

// --- Path confinement --------------------------------------------------------

func TestResolveLogPathConfinesToTheServerDirectory(t *testing.T) {
	dir := t.TempDir()

	ok, err := resolveLogPath(dir, `Logs\server\server.log`)
	if err != nil {
		t.Fatalf("a plain relative path was refused: %v", err)
	}
	if want := filepath.Join(dir, "Logs", "server", "server.log"); ok != want {
		t.Errorf("resolved to %q, want %q", ok, want)
	}

	// Every one of these is a real thing somebody pastes into a profile field:
	// the path from the game's own documentation, a Linux-shaped one, a climb out
	// of the sandbox, and an alternate data stream.
	for _, bad := range []string{
		"",
		`C:\Windows\System32\config\SAM`,
		`\\host\share\file.log`,
		`..\..\wings.log`,
		`logs\..\..\other-server\console.log`,
		`server.log:hidden`,
	} {
		if got, err := resolveLogPath(dir, bad); err == nil {
			t.Errorf("resolveLogPath(%q) = %q, want a refusal", bad, got)
		}
	}
}

// --- Encoding ----------------------------------------------------------------

func TestDecodersHandleSplitCharacters(t *testing.T) {
	// A pound sign split across two reads must not become two replacement
	// characters, which is what a naive pass-through produces and what an
	// operator then reports as "the console is corrupted".
	dec, err := decoderFor("utf-8")
	if err != nil {
		t.Fatal(err)
	}
	full := []byte("cost: £5\n")
	out, carry := dec(full[:7])
	if string(out) != "cost: " || len(carry) != 1 {
		t.Fatalf("first half decoded to %q with carry %v", out, carry)
	}
	out2, carry2 := dec(append(carry, full[7:]...))
	if got := string(out) + string(out2); got != "cost: £5\n" {
		t.Errorf("decoded %q, want the whole line", got)
	}
	if len(carry2) != 0 {
		t.Errorf("carry left over: %v", carry2)
	}
}

func TestUTF16DecoderStripsTheByteOrderMark(t *testing.T) {
	dec, err := decoderFor("utf-16le")
	if err != nil {
		t.Fatal(err)
	}

	// What a .NET StreamWriter left on its defaults writes.
	raw := []byte{0xFF, 0xFE}
	for _, r := range "hi\n" {
		raw = append(raw, byte(r), byte(r>>8))
	}

	out, carry := dec(raw)
	if string(out) != "hi\n" {
		t.Errorf("decoded %q, want %q", out, "hi\n")
	}
	if len(carry) != 0 {
		t.Errorf("carry left over: %v", carry)
	}

	// An odd trailing byte is half a character and must wait for its other half
	// rather than being printed.
	out, carry = dec([]byte{0x61})
	if len(out) != 0 || len(carry) != 1 {
		t.Errorf("a lone byte decoded to %q with carry %v; want nothing and one carried byte", out, carry)
	}
}

func TestDecoderForRejectsAnUnknownEncoding(t *testing.T) {
	if _, err := decoderFor("shift-jis"); err == nil {
		t.Error("an unsupported encoding was accepted")
	}
}

// --- Telnet negotiation ------------------------------------------------------

func TestTelnetFilterRefusesEveryOption(t *testing.T) {
	var f telnetFilter

	const echoOpt = 1
	text, replies := f.filter([]byte{'h', 'i', iac, do, echoOpt, '!', iac, will, echoOpt})

	if string(text) != "hi!" {
		t.Errorf("text = %q, want the negotiation stripped out of it", text)
	}
	want := []byte{iac, wont, echoOpt, iac, dont, echoOpt}
	if string(replies) != string(want) {
		t.Errorf("replies = %v, want %v -- a demand declined and an offer refused", replies, want)
	}
}

func TestTelnetFilterHandlesSequencesSplitAcrossReads(t *testing.T) {
	var f telnetFilter

	// The one that breaks a stateless filter: a TCP read boundary in the middle
	// of a three-byte command. Without carrying the partial sequence, the option
	// byte is printed to the console and the reply is never sent, so the server
	// asks again a second later, forever.
	text, replies := f.filter([]byte{'a', iac, do})
	if string(text) != "a" || len(replies) != 0 {
		t.Fatalf("first half: text = %q, replies = %v; want no reply yet", text, replies)
	}

	text, replies = f.filter([]byte{31, 'b'})
	if string(text) != "b" {
		t.Errorf("second half: text = %q, want the option byte not printed", text)
	}
	if want := []byte{iac, wont, 31}; string(replies) != string(want) {
		t.Errorf("second half: replies = %v, want %v", replies, want)
	}
}

func TestTelnetFilterPassesEscapedBytesAndSkipsSubnegotiation(t *testing.T) {
	var f telnetFilter

	text, _ := f.filter([]byte{iac, iac})
	if len(text) != 1 || text[0] != 255 {
		t.Errorf("IAC IAC decoded to %v, want a single literal 255", text)
	}

	text, _ = f.filter([]byte{'a', iac, sb, 24, 'x', 'y', iac, se, 'b'})
	if string(text) != "ab" {
		t.Errorf("text = %q, want the subnegotiation payload dropped", text)
	}
}

// --- The log tail, against a real worker -------------------------------------

func TestConsoleFollowsALogFile(t *testing.T) {
	c, col, instanceDir := startWorker(t, "console-logfile")
	workingDir := filepath.Join(filepath.Dir(instanceDir), "volume")

	// A stale log from a previous run. It must not be replayed: its content
	// belongs to a server that is not this one.
	logRel := filepath.Join("Logs", "server.log")
	if err := os.MkdirAll(filepath.Join(workingDir, "Logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, logRel), []byte("STALE-FROM-LAST-RUN\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The game: says nothing on stdout, writes to its log, then deletes and
	// recreates it -- which is how most of them start a fresh log -- and writes
	// some more. Following the name rather than the handle is the whole point.
	script := strings.Join([]string{
		`echo FIRST>>Logs\server.log`,
		`ping -n 3 127.0.0.1 >nul`,
		`del Logs\server.log`,
		`echo AFTER-ROTATION>>Logs\server.log`,
		`ping -n 4 127.0.0.1 >nul`,
	}, " & ")

	if err := c.Start(wire.Start{
		Argv:   []string{comspec(), "/c", script},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
		Console: wire.ConsoleConfig{
			Source: wire.LogSource{Type: wire.SourceFile, Path: `Logs\server.log`},
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 30*time.Second, "the rotated log's output", func() bool {
		return strings.Contains(col.consoleText(), "AFTER-ROTATION")
	})

	out := col.consoleText()
	if !strings.Contains(out, "FIRST") {
		t.Errorf("console = %q; want the log's first line", out)
	}
	if strings.Contains(out, "STALE-FROM-LAST-RUN") {
		t.Errorf("console = %q; want the previous run's log not replayed", out)
	}
}

func TestABadLogPathDoesNotStopTheServer(t *testing.T) {
	c, col, _ := startWorker(t, "console-badlog")

	if err := c.Start(wire.Start{
		Argv:   []string{comspec(), "/c", "echo SERVER-RAN"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
		Console: wire.ConsoleConfig{
			Source: wire.LogSource{Type: wire.SourceFile, Path: `..\..\escape.log`},
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 20*time.Second, "the server's own output", func() bool {
		return strings.Contains(col.consoleText(), "SERVER-RAN")
	})

	if !strings.Contains(col.logText(), "not following") {
		t.Errorf("worker log = %q; want it to say why the log is not being followed", col.logText())
	}
}

// --- The command channel, against a real worker ------------------------------

// fakeConsole is a game's TCP command console: it negotiates like a telnet
// server, then answers each line it is sent.
type fakeConsole struct {
	ln net.Listener

	mu       sync.Mutex
	received []string
}

func newFakeConsole(t *testing.T) *fakeConsole {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeConsole{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeConsole) serve(conn net.Conn) {
	defer conn.Close()

	// The negotiation a real one opens with. None of it should reach the console.
	_, _ = conn.Write([]byte{iac, do, 1, iac, will, 3})
	_, _ = conn.Write([]byte("Console ready\r\n"))

	// A real console parses the negotiation out of what it is sent rather than
	// treating it as command text, so this one does too -- otherwise the worker's
	// perfectly correct refusal lands at the front of the first command.
	var filter telnetFilter
	var pending []byte
	buf := make([]byte, 4096)

	for {
		n, err := conn.Read(buf)
		if n > 0 {
			text, _ := filter.filter(buf[:n])
			pending = append(pending, text...)

			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := strings.TrimRight(string(pending[:i]), "\r")
				pending = pending[i+1:]

				f.mu.Lock()
				f.received = append(f.received, line)
				f.mu.Unlock()

				if _, werr := conn.Write([]byte("ACK " + line + "\r\n")); werr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *fakeConsole) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeConsole) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func TestCommandsGoToTheServersOwnConsole(t *testing.T) {
	fake := newFakeConsole(t)
	c, col, _ := startWorker(t, "console-channel")

	// A server that does not read stdin at all: if the command went there it
	// would be swallowed, which is exactly the failure this is guarding.
	if err := c.Start(wire.Start{
		Argv:   []string{comspec(), "/c", "ping", "-n", "30", "127.0.0.1"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
		Console: wire.ConsoleConfig{
			Commands: wire.CommandChannel{
				Type:                  wire.ChannelTelnet,
				Port:                  fake.port(),
				ConnectTimeoutSeconds: 20,
			},
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 20*time.Second, "the command console to connect", func() bool {
		return strings.Contains(col.consoleText(), "Console ready")
	})

	if err := c.Stdin([]byte("say hello\r\n")); err != nil {
		t.Fatalf("Stdin: %v", err)
	}

	waitFor(t, 10*time.Second, "the console's answer", func() bool {
		return strings.Contains(col.consoleText(), "ACK say hello")
	})

	if got := fake.lines(); len(got) == 0 || got[0] != "say hello" {
		t.Errorf("the console received %v, want the command as a single line", got)
	}

	// The negotiation must have been consumed rather than printed.
	if out := col.consoleText(); strings.ContainsRune(out, 0xFFFD) || strings.Contains(out, "\xff") {
		t.Errorf("console = %q; want the telnet negotiation not rendered as text", out)
	}
}

func TestTheStopCommandGoesOverTheChannel(t *testing.T) {
	fake := newFakeConsole(t)
	c, col, _ := startWorker(t, "console-channel-stop")

	if err := c.Start(wire.Start{
		Argv:   []string{comspec(), "/c", "ping", "-n", "60", "127.0.0.1"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
		Console: wire.ConsoleConfig{
			Commands: wire.CommandChannel{
				Type:                  wire.ChannelTelnet,
				Port:                  fake.port(),
				ConnectTimeoutSeconds: 20,
			},
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 20*time.Second, "the command console to connect", func() bool {
		return strings.Contains(col.consoleText(), "Console ready")
	})

	// This fake never acts on it, so the stop escalates and kills the process.
	// That is fine and is not what is being checked: what matters is that the
	// command was delivered to the socket rather than to a stdin nobody reads.
	if err := c.Stop(wire.Stop{Mode: wire.StopCommand, Value: "saveandexit", TimeoutSeconds: 2}); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitFor(t, 30*time.Second, "the stop command to reach the console", func() bool {
		for _, l := range fake.lines() {
			if l == "saveandexit" {
				return true
			}
		}
		return false
	})

	waitFor(t, 30*time.Second, "exit event", func() bool {
		return col.exitCount() > 0
	})
}

func TestAnUnreachableConsolePortDoesNotStopTheServer(t *testing.T) {
	c, col, _ := startWorker(t, "console-channel-dead")

	// A port nothing is listening on. The server must still run: it is playable,
	// it just cannot be sent commands, and refusing to boot it would be worse.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if err := c.Start(wire.Start{
		Argv:   []string{comspec(), "/c", "echo SERVER-RAN & ping -n 4 127.0.0.1 >nul"},
		Env:    os.Environ(),
		Limits: wire.Limits{ProcessLimit: 32, MemoryBytes: 512 << 20},
		Console: wire.ConsoleConfig{
			Commands: wire.CommandChannel{
				Type:                  wire.ChannelTelnet,
				Port:                  dead,
				ConnectTimeoutSeconds: 2,
			},
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 20*time.Second, "the server's own output", func() bool {
		return strings.Contains(col.consoleText(), "SERVER-RAN")
	})

	waitFor(t, 20*time.Second, "the worker to give up on the console", func() bool {
		return strings.Contains(col.logText(), "could not reach the server's command console")
	})

	// And a command sent to a channel that never connected must say so where the
	// person who typed it will see it. Console input carries no request id and is
	// not waited on, so an error reply would reach nobody at all.
	if err := c.Stdin([]byte("noop\r\n")); err != nil {
		t.Fatalf("Stdin: %v", err)
	}
	waitFor(t, 15*time.Second, "the console to report the undelivered command", func() bool {
		return strings.Contains(col.consoleText(), "the command was not delivered")
	})
}
