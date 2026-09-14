//go:build windows

package windows

import (
	"testing"

	"github.com/pterodactyl/wings/internal/wire"
	"github.com/pterodactyl/wings/remote"
)

// consoleEnv builds an environment carrying a console profile, with the server
// variables an egg's profile fields would be written against.
//
// Assembled directly rather than through New, which wants a loaded daemon
// configuration to mint a worker token. Resolving a console profile reads only
// the metadata, so there is nothing here for a configuration to contribute.
func consoleEnv(t *testing.T, profile remote.ConsoleProfile) (*Environment, []string) {
	t.Helper()

	e := &Environment{Id: "console-resolve-test", meta: &Metadata{Console: profile}}

	return e, []string{
		"SERVER_PORT=30000",
		"TELNET_PORT=21004",
		"TELNET_PASSWORD=hunter2",
		"WORLD=Moon",
	}
}

func TestResolveConsoleSubstitutesTheServersVariables(t *testing.T) {
	// Written the way an egg author writes it: the port is a variable, because
	// every server on the node has a different one.
	e, vars := consoleEnv(t, remote.ConsoleProfile{
		Source: remote.ConsoleSource{
			Type: "file",
			Path: `Logs\{{WORLD}}\server.log`,
		},
		Commands: remote.ConsoleCommands{
			Type:                  "telnet",
			Port:                  "{{TELNET_PORT}}",
			Password:              "${TELNET_PASSWORD}",
			ConnectTimeoutSeconds: 120,
		},
	})

	got := e.resolveConsole(vars)

	if got.Source.Type != wire.SourceFile {
		t.Errorf("source type = %q, want %q", got.Source.Type, wire.SourceFile)
	}
	if want := `Logs\Moon\server.log`; got.Source.Path != want {
		t.Errorf("source path = %q, want %q", got.Source.Path, want)
	}
	if got.Commands.Type != wire.ChannelTelnet {
		t.Errorf("channel type = %q, want %q", got.Commands.Type, wire.ChannelTelnet)
	}
	if got.Commands.Port != 21004 {
		t.Errorf("port = %d, want 21004", got.Commands.Port)
	}
	if got.Commands.Host != "127.0.0.1" {
		t.Errorf("host = %q, want the loopback default", got.Commands.Host)
	}
	if got.Commands.Password != "hunter2" {
		t.Errorf("password = %q, want the variable substituted", got.Commands.Password)
	}
	if got.Commands.ConnectTimeoutSeconds != 120 {
		t.Errorf("connect timeout = %d, want 120", got.Commands.ConnectTimeoutSeconds)
	}
}

func TestResolveConsoleIsInertWithoutAProfile(t *testing.T) {
	e, vars := consoleEnv(t, remote.ConsoleProfile{})

	got := e.resolveConsole(vars)

	if got.Source.Type != "" || got.Commands.Type != "" {
		t.Errorf("resolved %+v for an egg with no console profile; want the zero value, "+
			"which is the process's own stdio", got)
	}
}

func TestResolveConsoleDropsAPortItCannotUse(t *testing.T) {
	// The mistake this is really for: a variable name that does not exist on the
	// server expands to nothing, and the profile still looks configured.
	for _, port := range []string{"{{NO_SUCH_VARIABLE}}", "", "telnet", "0", "70000", "-1"} {
		e, vars := consoleEnv(t, remote.ConsoleProfile{
			Commands: remote.ConsoleCommands{Type: "telnet", Port: port},
		})

		if got := e.resolveConsole(vars); got.Commands.Type != "" {
			t.Errorf("port %q resolved to %+v; want the channel dropped so commands fall "+
				"back to stdin rather than going to an arbitrary port", port, got.Commands)
		}
	}
}

func TestResolveConsoleRefusesALogPathOutsideTheServer(t *testing.T) {
	for _, path := range []string{
		`C:\Windows\System32\config\SAM`,
		`..\..\wings.log`,
		`\\host\share\server.log`,
		`{{NO_SUCH_VARIABLE}}`,
	} {
		e, vars := consoleEnv(t, remote.ConsoleProfile{
			Source: remote.ConsoleSource{Type: "file", Path: path},
		})

		if got := e.resolveConsole(vars); got.Source.Type != "" {
			t.Errorf("log path %q resolved to %+v; want it refused", path, got.Source)
		}
	}
}
