//go:build windows

package windows

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/internal/wire"
	"github.com/pterodactyl/wings/remote"
)

// Resolving an egg's console redirection into what the worker is told to do.
//
// The daemon does the interpretation -- variable substitution, path confinement,
// parsing a port out of a template -- and hands the worker a settled answer, for
// the same reason it resolves the startup command rather than sending the egg's
// template: the worker is the privileged half, and the fewer decisions it makes
// about operator-authored text the better.
//
// Everything here degrades rather than refuses. A profile with a nonsense port
// leaves a server that runs and cannot be sent commands, which an operator can
// see and fix; refusing the boot leaves them with a stopped server and a reason
// buried in a log.

// resolveConsole turns the egg's console profile into the worker's console
// configuration, substituting the server's variables into it.
func (e *Environment) resolveConsole(envVars []string) wire.ConsoleConfig {
	e.mu.RLock()
	profile := e.meta.Console
	e.mu.RUnlock()

	lookup := envLookup(envVars)

	return wire.ConsoleConfig{
		Source:   e.resolveConsoleSource(profile.Source, lookup),
		Commands: e.resolveConsoleCommands(profile.Commands, lookup),
	}
}

// resolveConsoleSource works out which log file to follow, if any.
func (e *Environment) resolveConsoleSource(src remote.ConsoleSource, lookup map[string]string) wire.LogSource {
	if !strings.EqualFold(strings.TrimSpace(src.Type), string(wire.SourceFile)) {
		return wire.LogSource{}
	}

	path := expandVars(src.Path, lookup)
	if strings.TrimSpace(path) == "" {
		e.log().Warn("this egg's console is configured to follow a log file but names no path; " +
			"the server's own output is all that will appear on the console")
		return wire.LogSource{}
	}

	// Checked here as well as in the worker. This one produces a message an
	// operator will actually see, next to the profile they just edited; the
	// worker's is the one that matters, because the worker is what opens the file.
	if err := checkContained(path); err != nil {
		e.log().WithFields(log.Fields{"path": path, "error": err}).Error(
			"refusing to follow this egg's log file; the server's own output is all that " +
				"will appear on the console")
		return wire.LogSource{}
	}

	e.log().WithFields(log.Fields{
		"path":     path,
		"encoding": src.Encoding,
	}).Debug("this server's console will follow a log file")

	return wire.LogSource{
		Type:     wire.SourceFile,
		Path:     path,
		Encoding: src.Encoding,
	}
}

// resolveConsoleCommands works out where console input goes.
func (e *Environment) resolveConsoleCommands(cmd remote.ConsoleCommands, lookup map[string]string) wire.CommandChannel {
	if !strings.EqualFold(strings.TrimSpace(cmd.Type), string(wire.ChannelTelnet)) {
		return wire.CommandChannel{}
	}

	raw := expandVars(cmd.Port, lookup)
	port, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || port < 1 || port > 65535 {
		// The single most likely thing to be wrong in one of these profiles: the
		// port is usually written as an egg variable, and a variable that does not
		// exist expands to nothing. Worth naming both the template and what it
		// came out as, because "{{TELNET_PORT}}" and "" are very different bugs.
		e.log().WithFields(log.Fields{
			"template": cmd.Port,
			"resolved": raw,
		}).Error("this egg's command console has no usable port, so the server cannot be sent " +
			"commands and its stop command has nowhere to go. Check the port in the egg's " +
			"windows profile")
		return wire.CommandChannel{}
	}

	host := strings.TrimSpace(expandVars(cmd.Host, lookup))
	if host == "" {
		host = "127.0.0.1"
	}

	e.log().WithFields(log.Fields{
		"host": host,
		"port": port,
	}).Debug("this server takes console commands over a port of its own")

	return wire.CommandChannel{
		Type:                  wire.ChannelTelnet,
		Host:                  host,
		Port:                  port,
		Password:              expandVars(cmd.Password, lookup),
		ConnectTimeoutSeconds: cmd.ConnectTimeoutSeconds,
	}
}

// expandVars substitutes the server's environment variables into a profile
// field, accepting both the {{VAR}} form eggs use and ${VAR}.
//
// The same normalisation resolveStartup does, and deliberately the same set of
// variables: somebody writing a log path into a profile has the egg's variable
// list in front of them and no reason to expect a different vocabulary here.
func expandVars(s string, lookup map[string]string) string {
	if s == "" {
		return ""
	}
	expanded := strings.NewReplacer("{{", "${", "}}", "}").Replace(s)
	return os.Expand(expanded, func(k string) string { return lookup[k] })
}

var (
	errPathNotRelative = errors.New("the path must be relative to the server's directory")
	errPathEscapes     = errors.New("the path climbs out of the server's directory")
)

// checkContained rejects a log path that is not a plain relative path inside the
// server's directory.
//
// Only the shape is checked here; the worker resolves it against the real
// directory and checks again. Splitting it this way means an operator gets the
// obvious mistakes -- an absolute path pasted from the game's own documentation,
// a ..\ climbing out -- reported against the profile rather than only on the
// next boot.
func checkContained(rel string) error {
	// A leading separator is caught explicitly because filepath.IsAbs does not:
	// `\Logs` is drive-relative rather than absolute on Windows, and Join would
	// quietly rewrite it into a contained path. Harmless, but it is not what was
	// typed, and the Panel plugin refuses the same shape -- a value the node
	// silently reinterprets is worse than one it rejects.
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) ||
		strings.Contains(rel, ":") {
		return errPathNotRelative
	}
	for _, part := range strings.FieldsFunc(filepath.ToSlash(rel), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return errPathEscapes
		}
	}
	return nil
}
