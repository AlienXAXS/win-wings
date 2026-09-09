//go:build windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
)

// WorkerExecutable is the supervisor binary the daemon spawns per server.
const WorkerExecutable = "winwings-worker.exe"

// ValidateWindowsHost checks that the host can actually run servers before the
// daemon starts accepting work.
//
// The Docker daemon used to fail loudly and early if it was misconfigured. There
// is no such backstop here, so these checks exist to turn what would otherwise be
// a confusing per-server start failure into one clear message at boot.
func ValidateWindowsHost() error {
	c := Get()

	if err := validateWorkerBinary(); err != nil {
		return err
	}
	if err := validateAccounts(c.System.Account); err != nil {
		return err
	}
	validateInstanceDirectorySeparation(c)
	warnIfPowerShellMissing()

	return nil
}

// validateWorkerBinary confirms the supervisor is deployed alongside the daemon.
func validateWorkerBinary() error {
	exe, err := os.Executable()
	if err != nil {
		return errors.Wrap(err, "config: could not determine the daemon's own path")
	}

	path := filepath.Join(filepath.Dir(exe), WorkerExecutable)
	if _, err := os.Stat(path); err != nil {
		return errors.Errorf(
			"config: %s was not found next to the daemon at %s. "+
				"Every server is supervised by this binary; the daemon cannot start any "+
				"server without it. Build it with: go build -o %s ./cmd/winwings-worker",
			WorkerExecutable, filepath.Dir(exe), WorkerExecutable)
	}
	return nil
}

// validateAccounts checks the server isolation configuration is coherent.
func validateAccounts(a AccountConfiguration) error {
	switch a.Isolation {
	case "pool":
		if len(a.Accounts) == 0 {
			return errors.New(
				"config: system.account.isolation is \"pool\" but no accounts are configured. " +
					"Create local accounts on this host, grant them the \"Log on as a batch job\" " +
					"right, and list them under system.account.accounts. To run every server as " +
					"the daemon's own account instead, set isolation to \"shared\" with an empty " +
					"shared name — but understand that one compromised server can then read every " +
					"other server's files and this daemon's Panel token")
		}
		for i, acct := range a.Accounts {
			if strings.TrimSpace(acct.Username) == "" {
				return errors.Errorf("config: system.account.accounts[%d] has no username", i)
			}
			if acct.Password == "" {
				return errors.Errorf(
					"config: system.account.accounts[%d] (%s) has no password. "+
						"The daemon needs it to obtain a logon token for that account. "+
						"The value supports the file:// prefix so it can be read from a file "+
						"rather than stored inline", i, acct.Username)
			}
		}
		log.WithField("accounts", len(a.Accounts)).
			Info("server isolation enabled using a pool of local accounts")

	case "shared":
		if strings.TrimSpace(a.Shared) == "" {
			log.Warn("system.account.isolation is \"shared\" with no account named: every server " +
				"will run as the account this daemon uses. A compromised server can read every " +
				"other server's files and this daemon's configuration, which holds the Panel " +
				"token. Acceptable only where every server on this node is equally trusted")
		} else {
			log.WithField("account", a.Shared).
				Warn("every server will run as a single shared account; servers are not isolated " +
					"from one another")
		}

	default:
		return errors.Errorf(
			"config: system.account.isolation must be \"pool\" or \"shared\", got %q", a.Isolation)
	}

	return nil
}

// validateInstanceDirectorySeparation warns if worker state is reachable from a
// server's own files.
//
// The instance directory holds each server's resolved startup command. A server
// able to write there could rewrite what its worker executes, which is arbitrary
// code execution as whatever account it runs under.
func validateInstanceDirectorySeparation(c *Configuration) {
	data, err1 := filepath.Abs(c.System.Data)
	inst, err2 := filepath.Abs(c.System.InstanceDirectory)
	if err1 != nil || err2 != nil {
		return
	}

	rel, err := filepath.Rel(data, inst)
	if err != nil {
		return
	}
	if !strings.HasPrefix(rel, "..") && rel != "." {
		log.WithFields(log.Fields{
			"data":      data,
			"instances": inst,
		}).Error("system.instance_directory is inside system.data, which servers can write to. " +
			"A server could rewrite its own startup command and execute arbitrary code. " +
			"Move it outside the server data tree")
	}
}

// warnIfPowerShellMissing reports up front rather than at first install.
func warnIfPowerShellMissing() {
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7", "pwsh.exe"),
		filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			log.WithField("interpreter", c).Debug("located PowerShell for installation scripts")
			return
		}
	}
	log.Warn("no PowerShell interpreter was found; server installations will fail")
}

// DescribeHost returns a short human-readable summary of the runtime
// configuration, for the boot log.
func DescribeHost() string {
	c := Get()
	return fmt.Sprintf("isolation=%s accounts=%d data=%s instances=%s",
		c.System.Account.Isolation,
		len(c.System.Account.Accounts),
		c.System.Data,
		c.System.InstanceDirectory)
}
