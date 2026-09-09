//go:build windows

package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/jobobject"
	"github.com/pterodactyl/wings/internal/winacl"
	"github.com/pterodactyl/wings/internal/winenv"
	"github.com/pterodactyl/wings/internal/winfw"
	"github.com/pterodactyl/wings/internal/winpriv"
	"github.com/pterodactyl/wings/internal/winproc"
	"github.com/pterodactyl/wings/internal/winsta"
	"github.com/pterodactyl/wings/internal/winuser"
)

// The self-test exists because almost everything that can go wrong on a Windows
// node goes wrong in a way that first shows up as a server failing to start,
// with an error from three layers down. Account rights, NTFS inheritance, Job
// Object limits and long path support are all host state the daemon depends on
// and does not control.
//
// So it does the real thing rather than inspecting configuration: it creates two
// accounts, gives them directories, launches processes as them, and checks that
// each can reach its own files and not the other's. If that passes, the node
// works.
//
// It never stops at the first failure. A host with three problems should produce
// one report listing three problems, not three runs.

type checkStatus string

const (
	statusPass checkStatus = "PASS"
	statusFail checkStatus = "FAIL"
	statusWarn checkStatus = "WARN"
	statusSkip checkStatus = "SKIP"
)

type checkResult struct {
	name   string
	status checkStatus
	detail string
}

// suite accumulates results so that the run can continue past a failure and
// still report accurately at the end.
//
// A check that could not run because an earlier one failed is recorded as
// skipped rather than failed: "no account was created" is not a second problem
// with the host, and reporting it as one buries the first.
type suite struct {
	results []checkResult
	cleanup []func()
}

func newSuite() *suite { return &suite{} }

// run executes one check, converting a panic into a failure.
//
// A hand-written Win32 binding that gets a struct layout wrong tends to panic
// rather than return an error, and taking the whole report down with it would
// hide every check after it.
func (s *suite) run(name string, fn func() (string, error)) bool {
	detail, err := func() (detail string, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return fn()
	}()

	if err != nil {
		s.results = append(s.results, checkResult{name, statusFail, err.Error()})
		s.report(name, statusFail, err.Error())
		return false
	}
	s.results = append(s.results, checkResult{name, statusPass, detail})
	s.report(name, statusPass, detail)
	return true
}

func (s *suite) warn(name, detail string) {
	s.results = append(s.results, checkResult{name, statusWarn, detail})
	s.report(name, statusWarn, detail)
}

func (s *suite) skip(name, detail string) {
	s.results = append(s.results, checkResult{name, statusSkip, detail})
	s.report(name, statusSkip, detail)
}

// report prints a check as it completes, so a run that hangs still shows how far
// it got.
func (s *suite) report(name string, status checkStatus, detail string) {
	fmt.Printf("  %-6s %-44s", status, name)
	if detail == "" {
		fmt.Println()
		return
	}
	lines := strings.Split(detail, "\n")
	fmt.Println(lines[0])
	for _, l := range lines[1:] {
		fmt.Printf("  %-6s %-44s%s\n", "", "", l)
	}
}

func (s *suite) defer_(fn func()) { s.cleanup = append(s.cleanup, fn) }

func (s *suite) runCleanup() {
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}
}

func (s *suite) counts() (pass, fail, warn, skip int) {
	for _, r := range s.results {
		switch r.status {
		case statusPass:
			pass++
		case statusFail:
			fail++
		case statusWarn:
			warn++
		case statusSkip:
			skip++
		}
	}
	return
}

var selfTestCommand = &cobra.Command{
	Use:     "selftest",
	Aliases: []string{"test"},
	Short:   "Check that this host can actually run servers.",
	// A failing check is a result, not a misuse of the command, so cobra should
	// not answer it with a usage screen.
	SilenceUsage: true,
	Long: `Exercises everything the daemon depends on that lives outside it.

Creates two temporary local accounts, gives each a directory, launches a process
as each one, and checks that a server can reach its own files and cannot reach
another server's. Also checks Job Object limits, long path support, PowerShell
and the worker binary.

Every check runs regardless of whether earlier ones failed, and the accounts and
directories are removed afterwards. Run it from an elevated prompt; without
administrator rights the account checks are skipped rather than failed.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		keep, _ := cmd.Flags().GetBool("keep")
		return runSelfTest(keep)
	},
}

func init() {
	selfTestCommand.Flags().Bool("keep", false,
		"leave the test accounts and directories in place for inspection")
	rootCommand.AddCommand(selfTestCommand)
}

// selfTestUUIDs are the two fake servers the suite provisions. The second exists
// only so the first can be shown to be unable to read it.
//
// They must differ within their first 16 hexadecimal characters, because that is
// all of a UUID that survives truncation into a 20-character account name. Two
// that differ only at the end map onto one account, and the daemon then refuses
// to reuse it — correctly, but the suite cannot run. A real v4 UUID is random
// throughout, so this is a constraint on test data rather than on servers.
var selfTestUUIDs = [2]string{
	"5e1f7e51-0000-4000-8000-000000000001",
	"5e1f7e52-0000-4000-8000-000000000002",
}

func runSelfTest(keep bool) error {
	fmt.Println("win-wings self test")
	fmt.Println()

	s := newSuite()
	if !keep {
		defer s.runCleanup()
	}

	// -- Host and configuration ------------------------------------------
	fmt.Println("Host")

	var state winpriv.State
	s.run("daemon security context", func() (string, error) {
		st, err := winpriv.Current()
		if err != nil {
			return "", err
		}
		state = st
		return st.Describe(), nil
	})

	loaded := s.run("configuration loads", func() (string, error) {
		if err := config.FromFile(configPath); err != nil {
			return "", fmt.Errorf("%s: %w", configPath, err)
		}
		if config.Get().AuthenticationToken == "" {
			return "", fmt.Errorf("%s has no panel token; run `wings configure` first "+
				"or paste the node's configuration from the Panel", configPath)
		}
		c := config.Get()
		return fmt.Sprintf("%s (isolation=%s data=%s)",
			configPath, c.System.Account.Isolation, c.System.Data), nil
	})

	s.run("window station", func() (string, error) {
		// The most useful single fact when a process dies with 0xC0000142.
		// Service-0x0-3e7$ means the daemon is running as a service and every
		// process it launches under another account needs an explicit grant on
		// that station; WinSta0 means it is running in a console, where the
		// grant usually already exists and this check therefore proves less
		// than it appears to.
		name, err := winsta.Current()
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(strings.ToLower(name), "winsta0") {
			return name + " (a console session; the service runs on Service-0x0-3e7$ " +
				"instead, which is stricter)", nil
		}
		return name, nil
	})

	s.run("worker binary present", func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		p := filepath.Join(filepath.Dir(exe), config.WorkerExecutable)
		st, err := os.Stat(p)
		if err != nil {
			return "", fmt.Errorf("%s is missing; build it with "+
				"go build -o %s ./cmd/winwings-worker", p, config.WorkerExecutable)
		}
		return fmt.Sprintf("%s (%d KiB)", p, st.Size()/1024), nil
	})

	s.run("PowerShell available", func() (string, error) {
		for _, p := range []string{
			filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7", "pwsh.exe"),
			filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
		} {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
		return "", fmt.Errorf("no PowerShell interpreter found; every egg install will fail")
	})

	s.run("long paths enabled", func() (string, error) {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE,
			`SYSTEM\CurrentControlSet\Control\FileSystem`, registry.QUERY_VALUE)
		if err != nil {
			return "", err
		}
		defer k.Close()
		v, _, err := k.GetIntegerValue("LongPathsEnabled")
		if err != nil {
			return "", fmt.Errorf("LongPathsEnabled is not set; paths over 260 characters "+
				"will fail: %w", err)
		}
		if v != 1 {
			return "", fmt.Errorf("LongPathsEnabled is %d, want 1", v)
		}
		return "LongPathsEnabled=1", nil
	})

	// -- Storage -----------------------------------------------------------
	fmt.Println()
	fmt.Println("Storage")

	var root string
	if !loaded {
		s.skip("data directory writable", "the configuration did not load")
	} else {
		s.run("data directory writable", func() (string, error) {
			root = config.Get().System.Data
			if err := os.MkdirAll(root, 0o700); err != nil {
				return "", err
			}
			probe := filepath.Join(root, ".winwings-selftest")
			if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
				return "", err
			}
			_ = os.Remove(probe)
			return root, nil
		})
	}

	if root == "" {
		s.skip("long path write", "no data directory")
	} else {
		s.run("long path write", func() (string, error) {
			// Well past MAX_PATH. Server files routinely go deeper than people
			// expect — a modpack's nested config tree will find this.
			deep := filepath.Join(root, ".winwings-selftest-long")
			for i := 0; i < 12; i++ {
				deep = filepath.Join(deep, strings.Repeat("d", 30))
			}
			defer os.RemoveAll(filepath.Join(root, ".winwings-selftest-long"))

			if err := os.MkdirAll(deep, 0o700); err != nil {
				return "", fmt.Errorf("could not create a %d-character path: %w", len(deep), err)
			}
			f := filepath.Join(deep, "file.txt")
			if err := os.WriteFile(f, []byte("ok"), 0o600); err != nil {
				return "", fmt.Errorf("could not write a %d-character path: %w", len(f), err)
			}
			return fmt.Sprintf("wrote a %d-character path", len(f)), nil
		})
	}

	// -- Accounts and isolation -------------------------------------------
	fmt.Println()
	fmt.Println("Accounts and isolation")

	if !state.IsAdmin && !state.IsSystem {
		s.skip("account lifecycle", "not running as an administrator; run this from an elevated prompt")
		s.skip("logon as a server account", "no account was created")
		s.skip("run a process as a server account", "no account was created")
		s.skip("server can write its own files", "no account was created")
		s.skip("server cannot read another server", "no account was created")
		s.skip("server cannot write its worker config", "no account was created")
		s.skip("job object process limit", "no account was created")
		s.skip("job object memory limit", "no account was created")
	} else if root == "" {
		s.skip("account lifecycle", "no data directory")
	} else {
		if iso := config.Get().System.Account.Isolation; iso != "managed" {
			detail := fmt.Sprintf("configured as %q, so the checks below exercise managed "+
				"isolation, which is not what the daemon will use", iso)
			// Worth being specific rather than merely noting the mismatch: this
			// combination does not start. The privilege rules differ per mode,
			// and an administrative daemon is refused under pool and shared.
			if state.IsAdmin || state.IsElevated || state.IsSystem {
				detail += fmt.Sprintf("\nthe daemon will REFUSE TO START: %q isolation does "+
					"not permit running as %s.", iso, state.Account) +
					"\nset system.account.isolation to \"managed\" in the config."
			}
			s.warn("isolation mode", detail)
		}
		runIsolationChecks(s, root, keep)
	}

	// -- Firewall ----------------------------------------------------------
	fmt.Println()
	fmt.Println("Firewall")
	runFirewallChecks(s, state)

	// -- Summary -----------------------------------------------------------
	pass, fail, warn, skip := s.counts()
	fmt.Println()
	fmt.Printf("%d passed, %d failed, %d warnings, %d skipped\n", pass, fail, warn, skip)

	if fail > 0 {
		fmt.Println()
		fmt.Println("Failed checks:")
		for _, r := range s.results {
			if r.status == statusFail {
				fmt.Printf("  - %s: %s\n", r.name, strings.Split(r.detail, "\n")[0])
			}
		}
		// A non-zero exit so this is usable from a provisioning script, without
		// cobra printing usage for what is not a usage error.
		return &silentError{fmt.Errorf("%d of %d checks failed", fail, pass+fail)}
	}
	if keep {
		fmt.Println()
		fmt.Println("--keep was given: the test accounts and directories were left in place.")
	}
	return nil
}

// silentError suppresses cobra's usage output for a failure that is not a
// usage error.
type silentError struct{ error }

func (e *silentError) Unwrap() error { return e.error }

// runFirewallChecks proves the daemon can open and close a server's ports.
//
// Worth exercising for real rather than inferring from the daemon's privileges:
// the rules are written by netsh, which fails differently for an unprivileged
// account, a disabled firewall service and a group policy that forbids local
// rules -- and only the first of those is visible from a token.
func runFirewallChecks(s *suite, state winpriv.State) {
	if !config.Get().System.Firewall.Manage {
		s.warn("firewall management",
			"system.firewall.manage is false; servers' ports must be opened by hand")
		return
	}

	if !s.run("firewall is manageable", func() (string, error) {
		if err := winfw.Available(); err != nil {
			return "", err
		}
		return "rules can be created and removed", nil
	}) {
		s.skip("open and close a server's ports", "the firewall cannot be managed")
		return
	}

	s.run("open and close a server's ports", func() (string, error) {
		const uuid = "5e1f7e5f-0000-4000-8000-00000000fw01"
		binding := winfw.Binding{"0.0.0.0": {28960, 28961}}

		if err := winfw.Apply(uuid, "win-wings self test", binding); err != nil {
			return "", err
		}
		defer winfw.Remove(uuid)

		// Confirm through a different mechanism than the one that wrote them.
		// netsh reporting success and the rule not existing is exactly the kind
		// of thing this suite is for.
		found, err := winfw.Exists(uuid)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("netsh reported success but no rule named %s exists",
				winfw.RuleName(uuid, "TCP"))
		}

		if err := winfw.Remove(uuid); err != nil {
			return "", err
		}
		if found, err := winfw.Exists(uuid); err != nil {
			return "", err
		} else if found {
			return "", fmt.Errorf("the rule %s still exists after removal; a deleted "+
				"server would leave its ports open", winfw.RuleName(uuid, "TCP"))
		}

		return "created TCP and UDP rules for two ports, then removed them", nil
	})
}

// runIsolationChecks provisions two throwaway servers and proves they are
// separated.
func runIsolationChecks(s *suite, root string, keep bool) {
	m := winuser.New("wwt-")

	type fake struct {
		uuid       string
		user       string
		password   string
		serverRoot string
		dataDir    string
		token      windows.Token
	}
	var servers [2]fake

	for i, uuid := range selfTestUUIDs {
		servers[i].uuid = uuid
		servers[i].serverRoot = filepath.Join(root, uuid)
		servers[i].dataDir = filepath.Join(servers[i].serverRoot, "data")
	}

	if !keep {
		s.defer_(func() {
			for i := range servers {
				if servers[i].token != 0 {
					_ = servers[i].token.Close()
				}
				_ = m.Remove(servers[i].uuid)
				_ = os.RemoveAll(servers[i].serverRoot)
			}
		})
	}

	created := s.run("account lifecycle", func() (string, error) {
		var names []string
		for i := range servers {
			user, password, err := m.Ensure(servers[i].uuid)
			if err != nil {
				return "", err
			}
			servers[i].user, servers[i].password = user, password
			names = append(names, user)

			info, err := winuser.Lookup(user)
			if err != nil {
				return "", err
			}
			if info == nil {
				return "", fmt.Errorf("the account %q was created but cannot be read back", user)
			}
			admin, err := winuser.IsAdministrator(user)
			if err != nil {
				return "", err
			}
			if admin {
				return "", fmt.Errorf("the account %q is an administrator, which would defeat "+
					"the NTFS separation between servers", user)
			}
		}
		return "created " + strings.Join(names, ", ") + " (unprivileged, batch logon only)", nil
	})

	if !created {
		s.skip("logon as a server account", "no account was created")
		s.skip("run a process as a server account", "no account was created")
		s.skip("server can write its own files", "no account was created")
		s.skip("server cannot read another server", "no account was created")
		s.skip("server cannot write its worker config", "no account was created")
		s.skip("job object process limit", "no account was created")
		s.skip("job object memory limit", "no account was created")
		return
	}

	// Directories, with the same permissions a real server gets.
	perms := s.run("NTFS permissions applied", func() (string, error) {
		for i := range servers {
			if err := os.MkdirAll(servers[i].dataDir, 0o700); err != nil {
				return "", err
			}
			// A file only the daemon should be able to write, standing in for
			// worker.json.
			if err := os.WriteFile(filepath.Join(servers[i].serverRoot, "worker.json"),
				[]byte("{}"), 0o600); err != nil {
				return "", err
			}
			if err := winacl.DenyAll(servers[i].serverRoot); err != nil {
				return "", err
			}
			if err := winacl.GrantExclusiveWrite(servers[i].dataDir, servers[i].user); err != nil {
				return "", err
			}
			// Something for the other server to fail to read.
			if err := os.WriteFile(filepath.Join(servers[i].dataDir, "secret.txt"),
				[]byte("server "+servers[i].uuid), 0o600); err != nil {
				return "", err
			}
		}
		return "each server's data directory admits only its own account", nil
	})

	// The logon is what proves the batch logon right was actually granted.
	loggedOn := s.run("logon as a server account", func() (string, error) {
		for i := range servers {
			token, err := winproc.LogonUser(servers[i].user, servers[i].password)
			if err != nil {
				return "", fmt.Errorf("could not log on as %s. This usually means the "+
					"\"Log on as a batch job\" right was not applied: %w", servers[i].user, err)
			}
			servers[i].token = token
		}
		return "both accounts obtained a batch logon token", nil
	})

	if !loggedOn {
		s.skip("run a process as a server account", "no logon token")
		s.skip("server can write its own files", "no logon token")
		s.skip("server cannot read another server", "no logon token")
		s.skip("server cannot write its worker config", "no logon token")
		s.skip("job object process limit", "no logon token")
		s.skip("job object memory limit", "no logon token")
		return
	}

	a, b := &servers[0], &servers[1]

	s.run("run a process as a server account", func() (string, error) {
		// whoami reads the process token rather than the environment, which is
		// the only answer worth having here. Asking cmd.exe to echo %USERNAME%
		// would either report whatever the daemon put in the environment block —
		// making the check circular — or, since the block it is given is
		// deliberately minimal, echo the variable name back unexpanded.
		whoami := filepath.Join(os.Getenv("SystemRoot"), "System32", "whoami.exe")
		code, out, err := runAs(a.token, a.dataDir, []string{whoami},
			jobobject.Limits{ProcessLimit: 16})
		if err != nil {
			return "", err
		}
		got := strings.TrimSpace(out)
		if code != 0 {
			return "", fmt.Errorf("exit code %d, output %q", code, got)
		}
		// whoami reports HOST\account.
		if _, after, ok := strings.Cut(got, `\`); ok {
			got = after
		}
		if !strings.EqualFold(got, a.user) {
			return "", fmt.Errorf("the process reported its account as %q, want %q", got, a.user)
		}
		return fmt.Sprintf("a process launched as %s reported itself as %s", a.user, got), nil
	})

	s.run("server can write its own files", func() (string, error) {
		code, out, err := runAs(a.token, a.dataDir,
			[]string{comspec(), "/c", `echo written> selftest.txt && type selftest.txt`},
			jobobject.Limits{ProcessLimit: 16})
		if err != nil {
			return "", err
		}
		if code != 0 || !strings.Contains(out, "written") {
			return "", fmt.Errorf("the server could not write into its own data directory "+
				"(exit %d): %s", code, strings.TrimSpace(out))
		}
		return "wrote and read back a file in its data directory", nil
	})

	if !perms {
		s.skip("server cannot read another server", "permissions were not applied")
		s.skip("server cannot write its worker config", "permissions were not applied")
	} else {
		s.run("server cannot read another server", func() (string, error) {
			target := filepath.Join(b.dataDir, "secret.txt")
			code, out, err := runAs(a.token, a.dataDir,
				[]string{comspec(), "/c", "type " + quoteForCmd(target)},
				jobobject.Limits{ProcessLimit: 16})
			if err != nil {
				return "", err
			}
			if code == 0 || strings.Contains(out, "server "+b.uuid) {
				return "", fmt.Errorf("SERVERS ARE NOT ISOLATED: %s read %s. Check that the "+
					"data volume is NTFS and that no inherited rule grants a broad group "+
					"access to %s", a.user, target, root)
			}
			return fmt.Sprintf("%s was denied %s", a.user, target), nil
		})

		s.run("server cannot write its worker config", func() (string, error) {
			target := filepath.Join(a.serverRoot, "worker.json")
			code, _, err := runAs(a.token, a.dataDir,
				[]string{comspec(), "/c", "echo tampered> " + quoteForCmd(target)},
				jobobject.Limits{ProcessLimit: 16})
			if err != nil {
				return "", err
			}
			if code == 0 {
				return "", fmt.Errorf("a server could rewrite %s, which is the command its "+
					"worker executes -- this is arbitrary code execution as the server's "+
					"account", target)
			}
			return "the server was denied write access to its own worker.json", nil
		})
	}

	s.run("job object process limit", func() (string, error) {
		// One process is the cmd.exe itself, so a limit of 1 must stop it
		// spawning a child.
		code, _, err := runAs(a.token, a.dataDir,
			[]string{comspec(), "/c", comspec() + " /c exit 0"},
			jobobject.Limits{ProcessLimit: 1})
		if err != nil {
			return "", err
		}
		if code == 0 {
			return "", fmt.Errorf("a process limit of 1 did not stop a second process from " +
				"starting; server process limits are not being enforced")
		}
		return "an active process limit stopped a second process from starting", nil
	})

	// PowerShell rather than cmd.exe, deliberately. Every install script is run
	// by PowerShell, and it is far more demanding of its environment: it loads
	// the .NET runtime, which fails outright without SystemRoot. A host where
	// cmd.exe runs and PowerShell does not is a host where every server appears
	// to work until the first install.
	psPath := powershellPath()
	if psPath == "" {
		s.skip("run PowerShell as a server account", "no PowerShell interpreter")
	} else {
		s.run("run PowerShell as a server account", func() (string, error) {
			code, out, err := runAs(a.token, a.dataDir,
				[]string{psPath, "-NoProfile", "-NonInteractive", "-Command", "Write-Host ran-ok"},
				jobobject.Limits{ProcessLimit: 16})
			if err != nil {
				return "", err
			}
			if code != 0 || !strings.Contains(out, "ran-ok") {
				return "", fmt.Errorf("PowerShell did not run as %s: exit %s, output %q. "+
					"Every egg install runs this way, so no server can be installed on this "+
					"host until it does", a.user, winproc.ExplainExitCode(code), strings.TrimSpace(out))
			}
			return "PowerShell started, ran and exited cleanly", nil
		})

		// A logon token is not a logon session as far as the registry is
		// concerned. Unless the account's profile has been loaded there is no
		// HKEY_CURRENT_USER, and the first thing that notices is .NET resolving
		// its default web proxy from the WinINET settings that live there --
		// which fails as "Error creating the Web Proxy specified in the
		// 'system.net/defaultProxy' configuration section", a message that names
		// neither the registry nor the profile.
		//
		// Every egg that downloads anything hits this on its first request, so it
		// is worth proving rather than discovering during an install.
		s.run("registry and web proxy as a server account", func() (string, error) {
			const probe = `$null = Get-Item 'HKCU:\Software' -ErrorAction Stop; ` +
				`$null = [System.Net.WebRequest]::DefaultWebProxy; Write-Host hkcu-ok`
			code, out, err := runAs(a.token, a.dataDir,
				[]string{psPath, "-NoProfile", "-NonInteractive", "-Command", probe},
				jobobject.Limits{ProcessLimit: 16})
			if err != nil {
				return "", err
			}
			if code != 0 || !strings.Contains(out, "hkcu-ok") {
				return "", fmt.Errorf("%s could not read HKEY_CURRENT_USER or resolve a web "+
					"proxy: exit %s, output %q. The account's profile was not loaded, so "+
					"anything reading per-user registry settings will fail -- egg installs "+
					"that download files, and Steam's client library lookup, among them",
					a.user, winproc.ExplainExitCode(code), strings.TrimSpace(out))
			}
			return "HKCU resolves and the default web proxy can be constructed", nil
		})
	}

	s.run("job object memory limit", func() (string, error) {
		// Allocate well past the cap and expect it to fail. cmd.exe cannot do
		// this, so the allocation is done by PowerShell.
		if psPath == "" {
			return "", fmt.Errorf("no PowerShell interpreter to allocate with")
		}
		code, out, err := runAs(a.token, a.dataDir,
			[]string{psPath, "-NoProfile", "-NonInteractive", "-Command",
				"$b = New-Object byte[] 268435456; $b[0] = 1; exit 0"},
			jobobject.Limits{MemoryBytes: 64 << 20, ProcessLimit: 16})
		if err != nil {
			return "", err
		}
		if code == 0 {
			return "", fmt.Errorf("a 256MB allocation succeeded inside a 64MB job; memory "+
				"limits are not being enforced (output: %s)", strings.TrimSpace(out))
		}
		// A process that died in the loader also exits non-zero, and accepting
		// that would report a working memory limit on a host where nothing runs
		// at all. This check passed for exactly that reason once already.
		if winproc.IsLoaderFailure(code) {
			return "", fmt.Errorf("PowerShell failed to start rather than failing to "+
				"allocate: %s. The memory limit was not exercised",
				winproc.ExplainExitCode(code))
		}
		return "a 256MB allocation was refused inside a 64MB job", nil
	})
}

// runAs launches a command as another account inside a Job Object and returns
// its exit code and combined output.
func runAs(token windows.Token, dir string, argv []string, limits jobobject.Limits) (uint32, string, error) {
	job, err := jobobject.Create()
	if err != nil {
		return 0, "", fmt.Errorf("could not create a job object: %w", err)
	}
	defer job.Close()

	if err := job.SetLimits(limits); err != nil {
		return 0, "", fmt.Errorf("could not apply job limits: %w", err)
	}

	proc, err := winproc.Start(winproc.Config{
		Argv:  argv,
		Dir:   dir,
		Env:   minimalEnvironment(dir),
		Token: token,
	}, job)
	if err != nil {
		return 0, "", fmt.Errorf("could not start the process: %w", err)
	}
	defer proc.Close()

	// Read concurrently with the wait: a process that fills the pipe buffer
	// while nobody is draining it blocks forever, and the wait would then never
	// return.
	type read struct {
		out []byte
		err error
	}
	done := make(chan read, 1)
	go func() {
		b, err := io.ReadAll(proc.Output())
		done <- read{b, err}
	}()

	code, err := proc.Wait()
	if err != nil {
		return 0, "", fmt.Errorf("waiting for the process failed: %w", err)
	}

	select {
	case r := <-done:
		return code, string(r.out), nil
	case <-time.After(5 * time.Second):
		return code, "", nil
	}
}

// minimalEnvironment is the environment a real server gets.
//
// Built by the same code path rather than a copy of it, so that a mistake in
// winenv.Base shows up here as a failed check instead of being reproduced
// faithfully by a second implementation.
func minimalEnvironment(dir string) []string {
	return winenv.Base(winenv.Paths{Data: dir, Temp: dir})
}

func comspec() string {
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
}

func powershellPath() string {
	for _, p := range []string{
		filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7", "pwsh.exe"),
		filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// quoteForCmd wraps a path for cmd.exe, which splits on spaces and does not
// understand the argv quoting rules the rest of the daemon uses.
func quoteForCmd(p string) string {
	return `"` + p + `"`
}
