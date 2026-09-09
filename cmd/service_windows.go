//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/pterodactyl/wings/internal/winuser"
)

// ServiceName is the Windows service the daemon registers as.
const ServiceName = "winwings"

// ServiceDisplayName is what appears in services.msc.
const ServiceDisplayName = "win-wings Game Server Daemon"

// runningAsService reports whether this process was started by the service
// control manager rather than from a console.
//
// The same binary serves both, so this decides whether to hand control to the
// SCM dispatcher or just run in the foreground.
func runningAsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return isService
}

// wingsService adapts the daemon to the service control manager.
type wingsService struct {
	run  func()
	stop func()
}

func (s *wingsService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.run()
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case <-done:
			// The daemon exited on its own.
			changes <- svc.Status{State: svc.StopPending}
			return false, 0

		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus

			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				if s.stop != nil {
					s.stop()
				}
				// Running servers are deliberately left alone. Each is supervised
				// by its own worker process, so stopping the daemon does not stop
				// the games; the daemon reconnects to them when it comes back.
				return false, 0
			}
		}
	}
}

// RunAsService hands control to the service control manager.
func RunAsService(run func(), stop func()) error {
	return svc.Run(ServiceName, &wingsService{run: run, stop: stop})
}

var serviceCommand = &cobra.Command{
	Use:   "service",
	Short: "Manage the win-wings Windows service.",
}

// DefaultServiceAccount is the local account the daemon creates for itself when
// none is named.
const DefaultServiceAccount = "winwings"

// serviceAccountComment marks the account as the daemon's own, so that a later
// reinstall can tell it apart from an account of the operator's that happens to
// share the name.
const serviceAccountComment = "win-wings daemon service account"

var serviceInstallCommand = &cobra.Command{
	Use:   "install",
	Short: "Register win-wings as a Windows service.",
	Long: `Registers this executable as a Windows service set to start automatically.

Must be run from an elevated prompt.

By default the daemon creates its own service account (.\winwings), gives it a
random password nobody has to know, adds it to Administrators, and grants it the
rights it needs to launch servers under their own accounts. Nothing has to be
prepared in secpol.msc or Local Users and Groups first.

Pass --account with --password to use an account you have already created, or
--account with --no-create for a managed service account or a virtual account.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		exe, err = filepath.Abs(exe)
		if err != nil {
			return err
		}

		account, _ := cmd.Flags().GetString("account")
		password, _ := cmd.Flags().GetString("password")
		allowSystem, _ := cmd.Flags().GetBool("allow-system")
		noCreate, _ := cmd.Flags().GetBool("no-create")

		switch {
		case allowSystem:
			// An empty ServiceStartName means LocalSystem. Left alone.
			account, password = "", ""

		default:
			if account == "" {
				account = `.\` + DefaultServiceAccount
			}
			// A password supplied on the command line means the operator owns
			// the account and the daemon should leave it alone; so does
			// --no-create, which is how a managed or virtual service account is
			// installed.
			if password == "" && !noCreate {
				name, err := localAccountName(account)
				if err != nil {
					return err
				}
				fmt.Printf("Creating and configuring the service account %s...\n", account)
				password, err = winuser.EnsureServiceAccount(name, serviceAccountComment)
				if err != nil {
					return err
				}
				fmt.Println("  Password:   randomly generated, held by the service control manager")
				fmt.Println("  Groups:     Administrators")
				fmt.Println("  Rights:     log on as a service, replace a process level token,")
				fmt.Println("              adjust memory quotas; interactive logon denied")
			}
		}

		m, err := mgr.Connect()
		if err != nil {
			return fmt.Errorf("could not connect to the service manager (run elevated): %w", err)
		}
		defer m.Disconnect()

		if s, err := m.OpenService(ServiceName); err == nil {
			s.Close()
			return fmt.Errorf("service %q already exists; remove it first with: %s service uninstall",
				ServiceName, filepath.Base(exe))
		}

		cfg := mgr.Config{
			DisplayName:      ServiceDisplayName,
			Description:      "Runs and supervises game servers for a Pterodactyl-compatible panel.",
			StartType:        mgr.StartAutomatic,
			ServiceStartName: account,
			Password:         password,
		}

		s, err := m.CreateService(ServiceName, exe, cfg, "--config", configPath)
		if err != nil {
			return fmt.Errorf("could not create the service: %w", err)
		}
		defer s.Close()

		// Best effort: an event log source lets service start failures be seen in
		// Event Viewer, which is where an operator will look first.
		if err := eventlog.InstallAsEventCreate(ServiceName,
			eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
			fmt.Printf("note: could not register the event log source: %v\n", err)
		}

		fmt.Printf("Service %q installed.\n", ServiceName)
		fmt.Printf("  Executable: %s\n", exe)
		fmt.Printf("  Config:     %s\n", configPath)
		if account != "" {
			fmt.Printf("  Account:    %s\n", account)
		}
		fmt.Printf("\nStart it with: sc start %s\n", ServiceName)
		return nil
	},
}

// localAccountName reduces a service account specification to the bare SAM name
// the network management API expects.
//
// `.\winwings` and `WINSRV01\winwings` are both this host's `winwings`.
// Anything else names an account this daemon has no business creating, and the
// operator is told to supply its password instead.
func localAccountName(account string) (string, error) {
	domain, name, ok := strings.Cut(account, `\`)
	if !ok {
		return account, nil
	}

	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	if domain == "." || (host != "" && strings.EqualFold(domain, host)) {
		return name, nil
	}

	return "", errors.New(strings.Join([]string{
		fmt.Sprintf("%q is not a local account, so this command cannot create it.", account),
		"",
		"Supply its password instead:",
		fmt.Sprintf(`    wings.exe service install --account %s --password <password>`, account),
		"",
		"or pass --no-create if it is a managed service account or a virtual account,",
		"which authenticate without one.",
	}, "\n"))
}

var serviceUninstallCommand = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the win-wings Windows service.",
	RunE: func(cmd *cobra.Command, _ []string) error {
		m, err := mgr.Connect()
		if err != nil {
			return fmt.Errorf("could not connect to the service manager (run elevated): %w", err)
		}
		defer m.Disconnect()

		s, err := m.OpenService(ServiceName)
		if err != nil {
			return fmt.Errorf("service %q is not installed", ServiceName)
		}
		defer s.Close()

		account := ""
		if cfg, err := s.Config(); err == nil {
			account = cfg.ServiceStartName
		}

		if err := s.Delete(); err != nil {
			return fmt.Errorf("could not remove the service: %w", err)
		}
		_ = eventlog.Remove(ServiceName)

		fmt.Printf("Service %q removed.\n", ServiceName)

		// The account is left in place by default. Removing the service is
		// usually a step in reinstalling it, and deleting the account in between
		// would strip the ACLs that name it — every server's data directory
		// would be left granting access to a SID that no longer resolves.
		if remove, _ := cmd.Flags().GetBool("remove-account"); remove {
			name, err := localAccountName(account)
			if err != nil {
				return err
			}
			info, err := winuser.Lookup(name)
			if err != nil {
				return err
			}
			switch {
			case info == nil:
				fmt.Printf("Account %s does not exist.\n", account)
			case info.Comment != serviceAccountComment:
				fmt.Printf("Account %s was not created by this daemon; leaving it alone.\n", account)
			default:
				if sid, err := winuser.SID(name); err == nil {
					_ = winuser.RemoveAllRights(sid)
				}
				if err := winuser.Delete(name); err != nil {
					return err
				}
				fmt.Printf("Account %s removed.\n", account)
			}
		} else if account != "" {
			fmt.Printf("Account %s was left in place; pass --remove-account to delete it.\n", account)
		}

		fmt.Println("Note: running servers were not stopped. Each is supervised by its own")
		fmt.Println("worker process and will keep running until stopped explicitly.")
		return nil
	},
}

var serviceStatusCommand = &cobra.Command{
	Use:   "status",
	Short: "Show the win-wings service status.",
	RunE: func(*cobra.Command, []string) error {
		// Query only. mgr.Connect asks for SC_MANAGER_ALL_ACCESS, which requires
		// elevation; reading a service's state does not, and an operator should
		// not need an admin prompt just to ask whether the daemon is running.
		h, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
		if err != nil {
			return fmt.Errorf("could not connect to the service manager: %w", err)
		}
		m := &mgr.Mgr{Handle: h}
		defer m.Disconnect()

		name, err := windows.UTF16PtrFromString(ServiceName)
		if err != nil {
			return err
		}
		sh, err := windows.OpenService(m.Handle, name,
			windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
		if err != nil {
			fmt.Printf("Service %q is not installed.\n", ServiceName)
			return nil
		}
		s := &mgr.Service{Name: ServiceName, Handle: sh}
		defer s.Close()

		status, err := s.Query()
		if err != nil {
			return err
		}

		var state string
		switch status.State {
		case svc.Stopped:
			state = "stopped"
		case svc.StartPending:
			state = "starting"
		case svc.StopPending:
			state = "stopping"
		case svc.Running:
			state = "running"
		default:
			state = fmt.Sprintf("state %d", status.State)
		}

		fmt.Printf("Service %q: %s", ServiceName, state)
		if status.ProcessId != 0 {
			fmt.Printf(" (pid %d)", status.ProcessId)
		}
		fmt.Println()

		if cfg, err := s.Config(); err == nil {
			fmt.Printf("  Executable: %s\n", cfg.BinaryPathName)
			fmt.Printf("  Account:    %s\n", cfg.ServiceStartName)
		}
		return nil
	},
}

func init() {
	serviceInstallCommand.Flags().String("account", "",
		`the account the service runs as, e.g. .\winwings`)
	serviceInstallCommand.Flags().String("password", "",
		"password for an existing --account; omit to have the daemon create and manage it")
	serviceInstallCommand.Flags().Bool("no-create", false,
		"use --account as it already exists, without creating or configuring it")
	serviceInstallCommand.Flags().Bool("allow-system", false,
		"permit installing as LocalSystem, which is strongly discouraged")
	serviceUninstallCommand.Flags().Bool("remove-account", false,
		"also delete the service account the daemon created for itself")

	serviceCommand.AddCommand(serviceInstallCommand)
	serviceCommand.AddCommand(serviceUninstallCommand)
	serviceCommand.AddCommand(serviceStatusCommand)
	rootCommand.AddCommand(serviceCommand)
}

// serviceStartupGrace bounds how long the SCM waits for the daemon to report
// running. Loading many servers from the Panel can take a moment.
const serviceStartupGrace = 30 * time.Second
