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

var serviceInstallCommand = &cobra.Command{
	Use:   "install",
	Short: "Register win-wings as a Windows service.",
	Long: "Registers this executable as a Windows service set to start automatically.\n\n" +
		"Must be run from an elevated prompt. The service account needs the\n" +
		"SeAssignPrimaryTokenPrivilege and SeIncreaseQuotaPrivilege rights if\n" +
		"per-server account isolation is used.",
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

		// An empty ServiceStartName means LocalSystem. The daemon refuses to run
		// that way by default, so installing it that way would produce a service
		// that fails at startup with a message the operator only sees in the log.
		// Refuse here instead, where they are already at a prompt.
		if account == "" && !allowSystem {
			return errors.New(strings.Join([]string{
				"refusing to install as LocalSystem.",
				"",
				"This daemon runs egg install scripts and game servers, both third-party",
				"code, so it should not hold SYSTEM authority. Create a dedicated account,",
				`grant it "Replace a process level token" and "Adjust memory quotas for a`,
				`process" in secpol.msc, then:`,
				"",
				`    wings.exe service install --account .\winwings --password <password>`,
				"",
				"Those two rights are the minimum needed to launch servers under their own",
				"accounts, which is what isolates servers from one another.",
				"",
				"Pass --allow-system to override, and set system.account.allow_elevated",
				"in the config to match.",
			}, "\n"))
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

var serviceUninstallCommand = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the win-wings Windows service.",
	RunE: func(*cobra.Command, []string) error {
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

		if err := s.Delete(); err != nil {
			return fmt.Errorf("could not remove the service: %w", err)
		}
		_ = eventlog.Remove(ServiceName)

		fmt.Printf("Service %q removed.\n", ServiceName)
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
		"password for --account; omit for a managed or virtual account")
	serviceInstallCommand.Flags().Bool("allow-system", false,
		"permit installing as LocalSystem, which is strongly discouraged")

	serviceCommand.AddCommand(serviceInstallCommand)
	serviceCommand.AddCommand(serviceUninstallCommand)
	serviceCommand.AddCommand(serviceStatusCommand)
	rootCommand.AddCommand(serviceCommand)
}

// serviceStartupGrace bounds how long the SCM waits for the daemon to report
// running. Loading many servers from the Panel can take a moment.
const serviceStartupGrace = 30 * time.Second
