//go:build windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pterodactyl/wings/internal/winpriv"
	"github.com/pterodactyl/wings/internal/winuser"
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
	if err := validatePrivileges(c.System.Account); err != nil {
		return err
	}
	if err := validateAccounts(c.System.Account); err != nil {
		return err
	}
	enableLongPaths()
	warnIfPowerShellMissing()

	return nil
}

// validatePrivileges checks the daemon is running with an appropriate amount of
// authority for the isolation mode in force.
//
// There is a real tension here, and which way it resolves depends on who is
// expected to create the accounts.
//
// Launching a process as another local account — the mechanism that isolates
// servers — requires SeAssignPrimaryTokenPrivilege and SeIncreaseQuotaPrivilege,
// which an ordinary user does not hold. *Creating* those accounts, and setting
// NTFS ownership on their directories, requires administrator rights on top of
// that.
//
// Under "managed" isolation the daemon does both, so it is an administrator and
// this function checks that it is. The authority is real and worth stating
// plainly: a compromise of the daemon is a compromise of the host. What it buys
// is that a compromise of a *server* — the far likelier event, since servers run
// third-party code that faces the internet — reaches nothing but that server's
// own files, because every server has its own unprivileged account and an ACL
// that admits only it.
//
// Under "pool" or "shared" the operator creates the accounts, the daemon needs
// only the two privileges, and running as an administrator is then unnecessary
// authority that this function refuses.
func validatePrivileges(a AccountConfiguration) error {
	state, err := winpriv.Current()
	if err != nil {
		// Not fatal: an inability to read our own token should not stop a node
		// from running, but the operator should know the check did not happen.
		log.WithField("error", err).Warn("could not determine the daemon's privileges")
		return nil
	}

	log.WithField("privileges", state.Describe()).Info("daemon security context")

	if a.Isolation == "managed" {
		return validateManagedPrivileges(a, state)
	}

	if (state.IsSystem || state.IsAdmin || state.IsElevated) && !a.AllowElevated {
		return errors.Errorf(
			"config: refusing to run as %s. This daemon executes egg install scripts and "+
				"supervises game servers, both third-party code, so a compromise of either "+
				"should not yield the host.\n\n"+
				"Create a dedicated account and grant it only SeAssignPrimaryTokenPrivilege "+
				"and SeIncreaseQuotaPrivilege (secpol.msc -> Local Policies -> User Rights "+
				"Assignment -> \"Replace a process level token\" and \"Adjust memory quotas "+
				"for a process\"), then reinstall the service with:\n"+
				"    wings.exe service install --account <domain>\\<account> --password <password>\n\n"+
				"To override this deliberately, set system.account.allow_elevated to true.",
			state.Account)
	}

	// Pool isolation without the privileges to use it would fail at the first
	// server start, so say so now.
	if a.Isolation == "pool" && !state.CanLaunchAsUser {
		return errors.Errorf(
			"config: system.account.isolation is \"pool\", but this account (%s) is missing "+
				"%s. Without them the daemon cannot launch a process as another account, so "+
				"servers cannot be isolated.\n\n"+
				"Grant them via secpol.msc -> Local Policies -> User Rights Assignment:\n"+
				"    SeAssignPrimaryTokenPrivilege = \"Replace a process level token\"\n"+
				"    SeIncreaseQuotaPrivilege      = \"Adjust memory quotas for a process\"\n\n"+
				"Or set system.account.isolation to \"shared\", accepting that every server "+
				"runs as this account and none are isolated from one another.",
			state.Account, strings.Join(state.Missing, " and "))
	}

	if state.CanLaunchAsUser && !state.IsSystem {
		log.Info("running unprivileged with the token-assignment rights needed to isolate servers")
	}

	return nil
}

// validateManagedPrivileges checks the daemon can do what managed isolation
// requires of it: create local accounts, and launch processes as them.
func validateManagedPrivileges(a AccountConfiguration, state winpriv.State) error {
	if state.IsSystem && !a.AllowElevated {
		return errors.New(
			"config: refusing to run as LocalSystem. Managed isolation needs administrator " +
				"rights, but not the unrestricted authority of the SYSTEM account, and a " +
				"dedicated account can be audited and revoked where SYSTEM cannot.\n\n" +
				"Reinstall the service with an account of its own — the daemon will create " +
				"and configure it for you:\n" +
				"    wings.exe service install\n\n" +
				"To override this deliberately, set system.account.allow_elevated to true.")
	}

	if !state.IsAdmin && !state.IsSystem {
		return errors.Errorf(
			"config: system.account.isolation is \"managed\", which means this daemon creates "+
				"a local account per server and sets the NTFS permissions that keep servers "+
				"apart. Both need administrator rights, and this account (%s) does not have "+
				"them.\n\n"+
				"Either reinstall the service so it runs as an account the daemon manages:\n"+
				"    wings.exe service install\n\n"+
				"or set system.account.isolation to \"pool\" and create the server accounts "+
				"yourself, which keeps this daemon unprivileged. See docs/DEPLOYMENT.md.",
			state.Account)
	}

	// Administrators hold SeIncreaseQuotaPrivilege by default but not
	// SeAssignPrimaryTokenPrivilege, so an administrative account is not
	// automatically able to launch a process as somebody else. It can grant
	// itself the right, but LSA evaluates account rights at logon, so it does
	// not take effect until the service restarts.
	if !state.CanLaunchAsUser {
		if err := winuser.GrantSelfRights(); err != nil {
			return errors.Errorf(
				"config: this account (%s) is missing %s, which are needed to launch a server "+
					"under its own account, and they could not be granted automatically: %v",
				state.Account, strings.Join(state.Missing, " and "), err)
		}
		return errors.Errorf(
			"config: this account (%s) was missing %s. They have now been granted, but Windows "+
				"only applies account rights at logon, so the daemon has to be restarted before "+
				"it can use them.\n\n"+
				"    sc stop winwings && sc start winwings",
			state.Account, strings.Join(state.Missing, " and "))
	}

	log.Info("managed isolation active: server accounts are created, secured and removed by the daemon")
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
	case "managed":
		prefix := strings.TrimSpace(a.Prefix)
		if prefix == "" {
			return errors.New(
				"config: system.account.prefix must not be empty. It is what distinguishes " +
					"the accounts this daemon creates from every other account on the host, " +
					"and the daemon will not touch an account that does not carry it")
		}
		// A local account name cannot exceed 20 characters, and the rest of the
		// name is the server's UUID. Leaving fewer than 12 hex characters of it
		// makes collisions between two servers plausible rather than negligible.
		if len(prefix) > 8 {
			return errors.Errorf(
				"config: system.account.prefix %q is too long. A Windows local account name "+
					"is capped at 20 characters and the remainder identifies the server, so "+
					"the prefix must be 8 characters or fewer", prefix)
		}
		log.WithField("prefix", prefix).
			Info("servers are isolated using local accounts created and removed by the daemon")

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
			"config: system.account.isolation must be \"managed\", \"pool\" or \"shared\", got %q",
			a.Isolation)
	}

	return nil
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
	return fmt.Sprintf("isolation=%s accounts=%d servers=%s runtimes=%d",
		c.System.Account.Isolation,
		len(c.System.Account.Accounts),
		c.System.Data,
		len(c.Runtime.Runtimes))
}
