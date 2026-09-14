//go:build windows

package winproc

import (
	"fmt"
	"os"
	"path/filepath"
)

// PowerShellPath locates a PowerShell interpreter.
//
// Windows PowerShell 5.1 is present on every supported Windows install and is
// the fallback. PowerShell 7 is preferred when present, since egg authors are
// more likely to target it and it handles UTF-8 far better.
//
// This lives here rather than beside the installer because two things now run
// operator-authored PowerShell -- the install script and an egg's pre-start
// script -- and they must agree on which interpreter that is. A script written
// against pwsh and silently run by powershell.exe fails on syntax the author has
// no reason to suspect.
func PowerShellPath() (string, error) {
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7", "pwsh.exe"),
		filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("winproc: could not locate a PowerShell interpreter")
}

// PowerShellArgv is the command line for running a generated script file.
//
// -ExecutionPolicy Bypass is scoped to this process only and is required because
// the script is generated rather than signed. -NonInteractive and -NoProfile
// keep a script from stalling on a prompt or inheriting operator profile state.
func PowerShellArgv(exe, script string) []string {
	return []string{
		exe,
		"-NoProfile",
		"-NonInteractive",
		"-NoLogo",
		"-ExecutionPolicy", "Bypass",
		"-File", script,
	}
}
