package windows

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/apex/log"

	"github.com/pterodactyl/wings/internal/winproc"
)

// steamcmdExe is the executable's name within the server's steamcmd directory.
const steamcmdExe = "steamcmd.exe"

// resolvePreStart returns the command to run to completion before the server
// starts, or nil when there is none.
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
func (e *Environment) resolvePreStart(envVars []string) []string {
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
