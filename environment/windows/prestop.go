package windows

import (
	"strings"

	"github.com/pterodactyl/wings/internal/winproc"
	"github.com/pterodactyl/wings/internal/wire"
)

// preStopScriptName is the staged script's filename, beside prestart.ps1.
const preStopScriptName = "prestop.ps1"

// resolveEggPreStop stages the egg's pre-stop script and returns the command
// that runs it, or nil when the egg has none.
//
// The environment is rebuilt for it rather than remembered from the boot, so
// that a variable changed in the Panel while the server ran -- an RCON password,
// say -- is the one the script sees. The server itself still has the old value,
// of course, but that is the operator's to reason about; the script at least
// reads what the Panel shows.
//
// A failure here is reported and skipped rather than failing the stop, which
// would leave a server that cannot be stopped at all. The generic stop
// mechanism follows regardless; the script is a better first attempt, not the
// only one.
func (e *Environment) resolveEggPreStop(envVars []string, pty bool) *wire.PreStopCommand {
	e.mu.RLock()
	script := e.meta.PreStopScript
	e.mu.RUnlock()

	if strings.TrimSpace(script) == "" {
		return nil
	}

	path, err := e.stageScript(preStopScriptName, script)
	if err != nil {
		e.log().WithField("error", err).Error(
			"could not stage this egg's pre-stop script; the server is being stopped without it")
		return nil
	}

	powershell, err := winproc.PowerShellPath()
	if err != nil {
		e.log().WithField("error", err).Error(
			"no PowerShell interpreter to run this egg's pre-stop script; " +
				"the server is being stopped without it")
		return nil
	}

	username, password, err := e.account()
	if err != nil {
		e.log().WithField("error", err).Error(
			"could not resolve the server's account for this egg's pre-stop script; " +
				"the server is being stopped without it")
		return nil
	}

	e.log().WithField("script", path).Info("running this egg's pre-stop script before asking the server to stop")
	return &wire.PreStopCommand{
		Argv:          winproc.PowerShellArgv(powershell, path),
		Label:         "this egg's pre-stop script",
		Env:           envVars,
		PseudoConsole: pty,
		Username:      username,
		Password:      password,
	}
}
