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
