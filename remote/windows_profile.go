package remote

import (
	"context"
	"fmt"
	"net/http"
)

// WindowsProfile is the per-egg information a Windows node needs that a stock
// Pterodactyl egg does not carry.
//
// It is served by the win-wings Blueprint plugin on the Panel rather than by the
// Panel itself, which is what lets this fork run against an unmodified
// Pterodactyl installation. See docs/PANEL-API.md for the full contract.
type WindowsProfile struct {
	// Runtime names what must be present on the host for this egg — "jdk-21",
	// "dotnet-8", or empty for a self-contained binary. It replaces the egg's
	// container image, which means nothing without containers.
	//
	// The daemon does not currently install runtimes; this is passed to the
	// installation script as INSTALL_RUNTIME so the script can verify or fetch
	// what it needs.
	Runtime string `json:"runtime"`

	// Startup overrides the egg's startup command for Windows.
	//
	// Almost every Linux startup line needs changing: paths use backslashes,
	// binaries have different names, and shell operators (&&, |, >) are not
	// interpreted because the command is executed directly rather than through a
	// shell. Empty means use the Panel's standard startup value unchanged.
	Startup string `json:"startup"`

	// WorkingDir is the directory the server process is started in, relative to
	// the server's data directory. Empty -- the ordinary case -- starts it in the
	// data directory itself.
	//
	// This is for the games that do not ask where they are, they assume. A server
	// that writes its logs to "..\Logs" is computing a path from the directory it
	// was started in; started from the data directory that resolves to the
	// server's root, which the server's account cannot write and must not be able
	// to, because worker.json lives there. Naming the subdirectory the game was
	// installed into moves the whole computation back inside the sandbox.
	//
	// The startup command's own relative paths are resolved against this too, so
	// an egg setting it should name its binary relative to it: with a WorkingDir
	// of "ServerFile", the startup command is "MyServer.exe", not
	// "ServerFile\MyServer.exe".
	//
	// Supports {{VAR}} and ${VAR}. Anything resolving outside the server's
	// directory is refused.
	WorkingDir string `json:"working_dir"`

	// Stop overrides how the server is asked to shut down.
	//
	// Eggs using a signal-based stop must set this: Windows has no signals. A
	// command written to stdin is the only mechanism most game servers actually
	// implement.
	Stop *ProcessStopConfiguration `json:"stop"`

	// PseudoConsole allocates a ConPTY instead of plain pipes for this egg.
	//
	// Needed only by processes that detect a non-console stdout and change
	// behaviour — steamcmd is the usual case, and will mangle or drop progress
	// output when handed a pipe. It costs a VT escape stream instead of clean
	// lines, so leave it off unless an egg needs it.
	PseudoConsole bool `json:"pseudo_console"`

	// Console redirects where the server's output is read from and where the
	// commands typed into the Panel are written to.
	//
	// Empty is the ordinary arrangement -- the process's own stdout and stdin --
	// and is what all but a handful of eggs want. See ConsoleProfile.
	Console ConsoleProfile `json:"console"`

	// PreStartScript is PowerShell run to completion every time the server
	// starts, before the startup command, in the server's directory and under its
	// own account.
	//
	// This is the counterpart to the install script and not a replacement for the
	// startup command: it prepares, it does not run the game. A server whose
	// config file has to be rewritten from its egg variables on every boot is what
	// it is for. Empty means none.
	PreStartScript string `json:"pre_start_script"`
}

// ConsoleProfile describes a server whose console is not its stdio.
//
// A few games write their log only to a file and accept commands only on a TCP
// port they open themselves. Their Linux eggs bolt the two onto the container's
// stdio with a shell pipeline -- `tail -F` into stdout, a telnet client on stdin
// -- which cannot be reproduced here and should not be: it puts a shell between
// the daemon and the process it supervises, and the exit code the Panel then
// sees belongs to the pipeline rather than to the game.
//
// So it is declared instead, and the worker does both jobs itself against the
// game process it already holds.
type ConsoleProfile struct {
	Source   ConsoleSource   `json:"source"`
	Commands ConsoleCommands `json:"commands"`
}

// ConsoleSource says where a server's console output is read from.
type ConsoleSource struct {
	// Type is "file" to follow a log file, or empty for the process's own output.
	Type string `json:"type"`

	// Path is the log file, relative to the server's data directory. Supports
	// {{VAR}} and ${VAR}, because a log path is frequently built out of the egg's
	// own variables. Anything resolving outside the server's directory is refused.
	Path string `json:"path"`

	// Encoding is "utf-8" (the default), "utf-16le" or "utf-16be".
	Encoding string `json:"encoding"`
}

// ConsoleCommands says where the commands typed into the Panel are written.
type ConsoleCommands struct {
	// Type is "telnet" for a TCP console, or empty for the process's stdin.
	Type string `json:"type"`

	// Host to connect to; empty means 127.0.0.1. A console like this authenticates
	// weakly or not at all, so it should never be reachable off the host, and this
	// exists for the odd server that binds a specific interface rather than as an
	// invitation to point it elsewhere.
	Host string `json:"host"`

	// Port is a string rather than a number so it can carry {{SERVER_PORT}} or
	// whichever variable the egg uses. It is expanded and parsed by the daemon.
	Port string `json:"port"`

	// Password is sent as the first line after connecting, when set. Supports
	// variable substitution, since it is usually an egg variable.
	Password string `json:"password"`

	// ConnectTimeoutSeconds bounds how long the worker waits for the port to open
	// after the server starts. Zero uses the worker's default, which is generous:
	// this is a world-loading time, not a network timeout.
	ConnectTimeoutSeconds int `json:"connect_timeout_seconds"`
}

// GetWindowsProfile fetches the Windows profile for a server's egg.
//
// A 404 means the plugin has no profile for this egg, which is reported as
// ErrNoWindowsProfile so the caller can refuse to run the server rather than
// guessing at Linux defaults that will not work.
func (c *client) GetWindowsProfile(ctx context.Context, uuid string) (WindowsProfile, error) {
	res, err := c.Get(ctx, fmt.Sprintf("/windows/servers/%s/profile", uuid), nil)
	if err != nil {
		return WindowsProfile{}, err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		return WindowsProfile{}, ErrNoWindowsProfile
	}
	if res.HasError() {
		return WindowsProfile{}, res.Error()
	}

	var p WindowsProfile
	err = res.BindJSON(&p)
	return p, err
}

// CheckWindowsProfileSupport reports whether the Panel is serving the Windows
// profile API at all.
//
// Called once at boot. If the plugin is missing the daemon can refuse to start
// rather than discovering it server by server, which is the fail-closed
// behaviour the design calls for.
func (c *client) CheckWindowsProfileSupport(ctx context.Context) error {
	res, err := c.Get(ctx, "/windows/ping", nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		return ErrNoWindowsProfileAPI
	}
	if res.HasError() {
		return res.Error()
	}
	return nil
}
