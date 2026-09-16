package windows

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/apex/log"

	"github.com/pterodactyl/wings/internal/accounts"
	"github.com/pterodactyl/wings/internal/winacl"
	"github.com/pterodactyl/wings/internal/winproc"
	"github.com/pterodactyl/wings/internal/wire"
)

// steamcmdExe is the executable's name within the server's steamcmd directory.
const steamcmdExe = "steamcmd.exe"

// resolvePreStart returns the commands to run to completion, in order, before
// the server starts. Nil when there are none.
//
// Two things can land here, and the order between them is not arbitrary. The
// steamcmd update goes first because it rewrites the server's files; an egg's
// own pre-start script goes second because what it almost always does is patch
// one of those files with the server's variables, and a script that ran before
// the update would have its work overwritten by it.
func (e *Environment) resolvePreStart(envVars []string) []wire.PreStartCommand {
	var cmds []wire.PreStartCommand

	if argv := e.resolveSteamUpdate(envVars); argv != nil {
		cmds = append(cmds, wire.PreStartCommand{Argv: argv, Label: "the steamcmd update"})
	}
	if argv := e.resolveEggPreStart(); argv != nil {
		cmds = append(cmds, wire.PreStartCommand{Argv: argv, Label: "this egg's pre-start script"})
	}
	return cmds
}

// resolveSteamUpdate returns the steamcmd update to run before the server, or
// nil when none is due.
//
// Under Docker this was the steamcmd image's entrypoint: before handing over
// to the startup command it ran an app_update whenever AUTO_UPDATE was set.
// There is no entrypoint here, so the daemon works the same command out from
// the server's variables and the worker runs it ahead of the server, in the
// same job and account, with its output on the console.
//
// Whether a server is a Steam game at all is decided by the presence of
// steamcmd in the server's steamcmd directory, which the install script is
// told about as STEAMCMD_DIR. A server without one is not touched.
func (e *Environment) resolveSteamUpdate(envVars []string) []string {
	dir := e.workingDirectory()
	exe := filepath.Join(e.steamcmdDirectory(), steamcmdExe)
	if _, err := os.Stat(exe); err != nil {
		e.warnAboutMisplacedSteamcmd()
		return nil
	}

	argv, reason := steamcmdUpdate(exe, dir, envLookup(envVars))
	if argv == nil {
		// Said out loud, as the entrypoint's "not updating" line was, because
		// an operator wondering why their server is stale looks here first.
		e.log().WithField("reason", reason).Info(
			"steamcmd is present but no update will run before the server starts")
		return nil
	}
	e.log().WithField("command", strings.Join(redactSteamSecrets(argv), " ")).
		Info("running a steamcmd update before the server starts")
	return argv
}

// preStartScriptName is the staged script's filename, beside install.ps1.
const preStartScriptName = "prestart.ps1"

// resolveEggPreStart stages the egg's pre-start script and returns the command
// that runs it, or nil when the egg has none.
//
// A failure here is reported and skipped rather than failing the boot. The
// script is preparation, not the server: a config file that did not get rewritten
// leaves a server running with its previous settings, which the operator can see
// and fix, whereas refusing to start leaves them with a stopped server and a line
// in a log they have to go looking for.
func (e *Environment) resolveEggPreStart() []string {
	e.mu.RLock()
	script := e.meta.PreStartScript
	e.mu.RUnlock()

	if strings.TrimSpace(script) == "" {
		return nil
	}

	path, err := e.stageScript(preStartScriptName, script)
	if err != nil {
		e.log().WithField("error", err).Error(
			"could not stage this egg's pre-start script; the server is starting without it")
		return nil
	}

	powershell, err := winproc.PowerShellPath()
	if err != nil {
		e.log().WithField("error", err).Error(
			"no PowerShell interpreter to run this egg's pre-start script; " +
				"the server is starting without it")
		return nil
	}

	e.log().WithField("script", path).Info("running this egg's pre-start script before the server starts")
	return winproc.PowerShellArgv(powershell, path)
}

// stageScript writes an egg script where the server can read it but not write
// it, and returns its path.
//
// The reasoning is the installer's, and it matters more here: these scripts run
// on every single boot and stop, under the server's own account. Staged in the
// server root -- which the daemon owns and the server has no access to -- with
// read and execute granted on this one file. A script inside the server's data
// directory would be a server rewriting what its next boot executes.
func (e *Environment) stageScript(name, script string) (string, error) {
	path := filepath.Join(e.ServerRoot(), name)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}

	// PowerShell is content with either line ending, but normalising to CRLF
	// avoids surprises in here-strings within scripts authored on Windows.
	body := strings.ReplaceAll(script, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, strings.NewReader(body)); err != nil {
		_ = f.Close()
		return "", err
	}
	// Closed before the ACL is rewritten, for the same reason the installer does:
	// leaving it open past the point the server account needs to read it invites
	// a sharing violation.
	if err := f.Close(); err != nil {
		return "", err
	}

	username, _, err := accounts.For(e.Id)
	if err != nil {
		return "", err
	}
	if err := winacl.GrantReadFile(path, username); err != nil {
		return "", err
	}
	return path, nil
}

// steamcmdUpdate builds the update command from the variables the standard
// steamcmd eggs define, mirroring what the yolks entrypoint ran. It returns nil
// and the reason when no update should run.
//
// The install directory is forced to the server's own directory rather than
// left to the variables: the Linux eggs hardcode /home/container there, which
// means nothing on this host, and letting a variable choose it would let a
// server write outside its sandbox.
func steamcmdUpdate(exe, dir string, vars map[string]string) ([]string, string) {
	if !isPositive(vars["AUTO_UPDATE"]) {
		return nil, "AUTO_UPDATE is not enabled"
	}
	appID := strings.TrimSpace(vars["SRCDS_APPID"])
	if appID == "" {
		appID = strings.TrimSpace(vars["STEAM_APPID"])
	}
	if appID == "" {
		return nil, "neither SRCDS_APPID nor STEAM_APPID names an app to update"
	}

	argv := []string{exe, "+force_install_dir", dir}

	user := strings.TrimSpace(vars["STEAM_USER"])
	if user == "" || strings.EqualFold(user, "anonymous") {
		argv = append(argv, "+login", "anonymous")
	} else {
		argv = append(argv, "+login", user)
		if pass := vars["STEAM_PASS"]; pass != "" {
			argv = append(argv, pass)
			if auth := strings.TrimSpace(vars["STEAM_AUTH"]); auth != "" {
				argv = append(argv, auth)
			}
		}
	}

	argv = append(argv, "+app_update", appID)
	if beta := strings.TrimSpace(vars["SRCDS_BETAID"]); beta != "" {
		argv = append(argv, "-beta", beta)
		if pass := vars["SRCDS_BETAPASS"]; pass != "" {
			argv = append(argv, "-betapassword", pass)
		}
	}
	if flags := strings.TrimSpace(vars["INSTALL_FLAGS"]); flags != "" {
		extra, err := winproc.ParseCommandLine(flags)
		if err != nil {
			return nil, "INSTALL_FLAGS could not be parsed: " + err.Error()
		}
		argv = append(argv, extra...)
	}
	if isPositive(vars["VALIDATE"]) {
		argv = append(argv, "validate")
	}
	return append(argv, "+quit"), ""
}

// warnAboutMisplacedSteamcmd says why a steamcmd inside the server directory
// is being ignored.
//
// That is where the Linux eggs put it, so it is the natural first attempt at a
// port. It cannot work: steamcmd refuses to install into its own folder or
// any folder above it, prints "Please set the game install path to something
// other than the Steam install folder", ignores the directive, and installs
// into its own folder instead. Better to say so than to leave a server that
// quietly never updates.
func (e *Environment) warnAboutMisplacedSteamcmd() {
	misplaced := filepath.Join(e.workingDirectory(), "steamcmd", steamcmdExe)
	if _, err := os.Stat(misplaced); err != nil {
		return
	}
	e.log().WithFields(log.Fields{
		"found":    misplaced,
		"expected": filepath.Join(e.steamcmdDirectory(), steamcmdExe),
	}).Warn("steamcmd was found inside the server directory and cannot update it from " +
		"there; steamcmd refuses to install into a folder above itself. Have the egg's " +
		"install script put it in STEAMCMD_DIR instead")
}

// isPositive reports whether an egg variable is switched on. Eggs are not
// consistent about how they spell that, so every common spelling counts.
func isPositive(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

// envLookup indexes "KEY=VALUE" strings by key.
func envLookup(envVars []string) map[string]string {
	lookup := make(map[string]string, len(envVars))
	for _, v := range envVars {
		if k, val, ok := strings.Cut(v, "="); ok {
			lookup[k] = val
		}
	}
	return lookup
}

// redactSteamSecrets returns a copy of a steamcmd command line fit for a log:
// the account password and any beta password are masked.
func redactSteamSecrets(argv []string) []string {
	out := append([]string(nil), argv...)
	for i := 0; i < len(out); i++ {
		switch out[i] {
		case "+login":
			// "+login user password [guard]": the password is the second
			// argument, when there is one and it is not the next directive.
			if i+2 < len(out) && !strings.EqualFold(out[i+1], "anonymous") && !strings.HasPrefix(out[i+2], "+") {
				out[i+2] = "****"
			}
		case "-betapassword":
			if i+1 < len(out) {
				out[i+1] = "****"
			}
		}
	}
	return out
}
