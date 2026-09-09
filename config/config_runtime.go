package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RuntimeConfiguration defines how the daemon runs server processes on the
// host. It replaces upstream wings' DockerConfiguration.
//
// Most of the Docker configuration had no Windows analogue and was dropped:
// bridge networking and port publishing (Windows binds nothing on the server's
// behalf), image registries, tmpfs, user namespace remapping, and the CFS
// tunables (cpu_period, cpu_burst, cpu_shares) which are cgroup concepts with
// no Job Object equivalent.
//
// What survived maps onto Job Object limits, which are the closest thing
// Windows has to cgroups.
type RuntimeConfiguration struct {
	// ProcessLimit caps the number of concurrently active processes in a
	// server's Job Object, via JOB_OBJECT_LIMIT_ACTIVE_PROCESS. This is the
	// direct replacement for Docker's container_pid_limit and exists for the
	// same reason: on a shared node a malicious server can otherwise exhaust
	// the host's process table.
	ProcessLimit uint32 `default:"512" json:"process_limit" yaml:"process_limit"`

	// CpuHardCap selects how a server's CPU limit is enforced.
	//
	// When true (the default) the Job Object is given
	// JOB_OBJECT_CPU_RATE_CONTROL_HARD_CAP, so a server can never exceed its
	// allocation even on an idle host — this matches the behaviour operators
	// expect from Docker's CFS quota.
	//
	// When false the limit becomes a relative weight, which only takes effect
	// once the host is saturated. Servers may burst above their allocation on
	// an idle node.
	CpuHardCap bool `default:"true" json:"cpu_hard_cap" yaml:"cpu_hard_cap"`

	// InstallerLimits bounds the resources an installation process may consume,
	// so a runaway install script cannot destabilise the node. Applied to the
	// installer's own Job Object. Whichever is higher between this and the
	// server's own limits takes precedence.
	InstallerLimits struct {
		Memory int64 `default:"1024" json:"memory" yaml:"memory"`
		Cpu    int64 `default:"100" json:"cpu" yaml:"cpu"`
	} `json:"installer_limits" yaml:"installer_limits"`

	// Overhead controls the memory headroom added on top of a server's assigned
	// memory before the Job Object's hard limit is set. Carried over from
	// upstream unchanged, and it matters more here than it did under Docker:
	// JOB_OBJECT_LIMIT_JOB_MEMORY causes allocations to fail rather than
	// invoking an OOM killer, so a JVM sitting fractionally over its limit dies
	// with an allocation failure instead of being reaped.
	Overhead Overhead `json:"overhead" yaml:"overhead"`

	// BindAddress is the address servers are told to bind to when an egg's
	// configuration template references the node's interface. Under Docker this
	// was the pterodactyl0 bridge gateway; on Windows there is no bridge and
	// processes bind host addresses directly.
	BindAddress string `default:"0.0.0.0" json:"bind_address" yaml:"bind_address"`

	// Console configures capture of server stdout/stderr by the worker process.
	Console ConsoleConfiguration `json:"console" yaml:"console"`

	// Runtimes maps the runtime names eggs ask for onto directories on this host.
	//
	// An egg's Windows profile names what it needs — "jdk-21", "dotnet-8" — in
	// the same way a Linux egg names a container image. Unlike an image, a name
	// alone is not enough here: several Java versions coexist on one host and a
	// bare `java` on PATH resolves to whichever installer ran last.
	//
	// Each entry's `bin` directory is prepended to the PATH of that server's
	// process and its install script, and exported as RUNTIME_PATH. An egg can
	// then invoke `java` and get the right one, or use {{RUNTIME_PATH}}\java.exe
	// explicitly.
	//
	// A server whose runtime is not listed here still starts; it simply inherits
	// the host PATH, which is correct for eggs that need no runtime at all.
	Runtimes map[string]string `json:"runtimes" yaml:"runtimes"`

	// RequireWindowsProfile refuses to run any server whose egg has no Windows
	// profile published by the Panel's win-wings plugin.
	//
	// This is the fail-closed setting and should be on in production. A Linux
	// egg's install script and startup command cannot work here, so running one
	// produces a server that appears to install and then fails in a way nobody
	// can diagnose from the Panel.
	//
	// It defaults to off so a node can be brought up and tested against an
	// unmodified Panel before the plugin exists, falling back to the egg's
	// standard fields. Turn it on once the plugin is deployed.
	RequireWindowsProfile bool `default:"false" json:"require_windows_profile" yaml:"require_windows_profile"`
}

// ConsoleConfiguration controls how a server's console output is captured and
// retained. Under Docker this was the container log driver; here the worker
// owns it.
type ConsoleConfiguration struct {
	// PseudoConsole forces ConPTY allocation for every server, rather than
	// honouring the per-egg setting in the Windows profile.
	//
	// Leave this off. ConPTY is required for processes that check whether their
	// stdout is a character device (steamcmd is the common case, and will mangle
	// or drop output when handed a plain pipe), but it returns a VT stream with
	// escape sequences and cursor movement rather than clean lines. Servers that
	// write line-oriented output — anything JVM-based, most notably — produce
	// better logs on plain pipes.
	PseudoConsole bool `default:"false" json:"pseudo_console" yaml:"pseudo_console"`

	// InstallPseudoConsole allocates a ConPTY for installation scripts.
	//
	// On by default, which is the opposite of the setting above, because an
	// install and a running server want opposite things. A server's console is
	// read for a long time by software, and clean lines are worth more than
	// liveness. An install is watched by a person for a couple of minutes and
	// then thrown away, and liveness is the entire point.
	//
	// Handed a plain pipe, the C runtime that steamcmd and most other installers
	// are built on switches stdout from line buffering to full buffering. The
	// output is not lost, it just arrives in 4KB blocks — which for a download
	// that takes twenty minutes means an empty console and then everything at
	// once, exactly when somebody is watching to see whether it is progressing.
	//
	// Turn it off if an egg's installer produces unreadable output through a
	// ConPTY; allocation failing is handled without it, by falling back to pipes.
	InstallPseudoConsole bool `default:"true" json:"install_pseudo_console" yaml:"install_pseudo_console"`

	// Columns and Rows size the pseudo console when one is allocated. Some
	// processes wrap or truncate output to the reported width.
	Columns uint16 `default:"200" json:"columns" yaml:"columns"`
	Rows    uint16 `default:"50" json:"rows" yaml:"rows"`

	// MaxSize is the maximum size in megabytes of a server's console log before
	// it is rotated.
	MaxSize int64 `default:"5" json:"max_size" yaml:"max_size"`

	// MaxFiles is the number of rotated console logs retained per server.
	MaxFiles int `default:"1" json:"max_files" yaml:"max_files"`
}

// FirewallConfiguration controls whether the daemon opens a server's allocated
// ports in the Windows Firewall.
//
// Docker coupled these: publishing a container port opened the path to it.
// Windows does not, so without this a server starts cleanly, reports healthy,
// and cannot be connected to.
type FirewallConfiguration struct {
	// Manage lets the daemon create an inbound allow rule per server covering
	// exactly the allocations the Panel assigned, and remove it when the server
	// is deleted.
	//
	// Turn this off if the host's firewall rules are managed centrally — by
	// group policy, or by a configuration management tool that would fight the
	// daemon over them. Servers are then unreachable until something else opens
	// their ports.
	//
	// Requires administrator rights, which managed isolation already implies.
	// Under pool or shared isolation the daemon is unprivileged by design, so
	// this is reported as unavailable at boot rather than failing per server.
	Manage bool `default:"true" json:"manage" yaml:"manage"`

	// Prune removes rules belonging to servers this node no longer has, at boot.
	//
	// Firewall rules outlive the daemon, so a server deleted while its node was
	// stopped leaves its ports open indefinitely. Only rules this daemon created
	// are considered; anything not carrying its prefix is left alone.
	Prune bool `default:"true" json:"prune" yaml:"prune"`
}

// AccountConfiguration describes the Windows account(s) that server processes
// run under.
//
// Running every server as the account the daemon itself uses is the easy path
// and the wrong one: a single compromised game server would then be able to read
// every other server's files and the daemon's own configuration, which holds the
// Panel node token — a credential that controls every server on the node.
type AccountConfiguration struct {
	// Isolation selects how server processes are separated from each other.
	//
	//   "managed" — the daemon creates one local account per server, named after
	//               the server's UUID, with a random password it keeps only in
	//               memory, and deletes the account when the server is deleted.
	//               Requires the daemon to run as an administrator. This is the
	//               default and what an operator should use.
	//   "pool"    — each server is assigned a distinct pre-created local account
	//               from Accounts, whose passwords live in this file. For hosts
	//               where the daemon must not hold administrator rights and the
	//               operator is willing to maintain the accounts by hand.
	//   "shared"  — every server runs as the account named by Shared. Simple,
	//               and acceptable only for single-tenant nodes where every
	//               server is already trusted equally.
	Isolation string `default:"managed" json:"isolation" yaml:"isolation"`

	// Prefix is prepended to the account names the daemon creates under
	// "managed" isolation. A Windows local account name is capped at 20
	// characters and the rest is the server's UUID, so a longer prefix means
	// fewer UUID characters and a higher chance of two servers colliding on one
	// name. Three characters is the intended size.
	Prefix string `default:"ww-" json:"prefix" yaml:"prefix"`

	// Accounts is the pool of local accounts available to servers when Isolation
	// is "pool". Under "managed" isolation this is unused: the daemon creates
	// the accounts itself.
	//
	// A node can run at most len(Accounts) servers concurrently.
	Accounts []PoolAccount `json:"accounts" yaml:"accounts"`

	// Shared names the account used when Isolation is "shared".
	Shared string `default:"" json:"shared" yaml:"shared"`

	// AllowElevated permits the daemon to run as an administrator under "pool"
	// or "shared" isolation, where it has no need to.
	//
	// Under "managed" isolation the daemon must be an administrator — creating
	// accounts and rewriting NTFS ownership are privileged operations — so this
	// setting does not apply and the check is inverted: the daemon refuses to
	// start if it is *not* an administrator.
	//
	// It never permits running as LocalSystem. See docs/DEPLOYMENT.md.
	AllowElevated bool `default:"false" json:"allow_elevated" yaml:"allow_elevated"`
}

// For returns the local account credentials a given server should run under,
// for the isolation modes whose accounts are configured rather than created.
//
// "managed" is deliberately absent: its accounts are created on demand and its
// passwords exist only in memory, so it cannot be answered from configuration.
// Call internal/accounts.For instead, which handles every mode.
//
// Pool assignment is by hash of the server UUID rather than by allocation order,
// so it is stable across daemon restarts without persisting a mapping. Two
// servers can therefore collide onto one account, which weakens isolation
// between exactly those two but never grants access outside the pool. Size the
// pool comfortably above the server count.
//
// Returns empty strings when no isolation is configured, which runs the server
// as the account the daemon itself uses.
func (a AccountConfiguration) For(uuid string) (username, password string) {
	switch a.Isolation {
	case "shared":
		return a.Shared, ""
	case "pool":
		if len(a.Accounts) == 0 {
			return "", ""
		}
		// FNV-1a over the UUID.
		var h uint32 = 2166136261
		for i := 0; i < len(uuid); i++ {
			h ^= uint32(uuid[i])
			h *= 16777619
		}
		acct := a.Accounts[int(h%uint32(len(a.Accounts)))]
		return acct.Username, acct.Password
	default:
		return "", ""
	}
}

// PoolAccount is a single pre-created local account available for running server
// processes.
type PoolAccount struct {
	// Username is the local account name, without a domain component.
	Username string `json:"username" yaml:"username"`

	// Password authenticates the account when the daemon creates a process as it.
	//
	// Supports the `file://` prefix handled by Expand, so the value can be read
	// from a file rather than stored inline. Prefer that: this file is readable
	// by the daemon's own account and nothing else should hold these secrets.
	Password string `json:"-" yaml:"password"`
}

// Overhead controls the memory overhead given to all servers to work around
// software such as the JVM not reliably staying below its configured maximum.
type Overhead struct {
	// Override controls if the overhead limits should be overridden by the values in the config file.
	Override bool `default:"false" json:"override" yaml:"override"`

	// DefaultMultiplier sets the default multiplier for if no Multipliers are able to be applied.
	DefaultMultiplier float64 `default:"1.05" json:"default_multiplier" yaml:"default_multiplier"`

	// Multipliers allows overriding DefaultMultiplier depending on the amount of memory
	// configured for a server.
	//
	// Default values (used if Override is `false`)
	// - Less than 2048 MB of memory, multiplier of 1.15 (15%)
	// - Less than 4096 MB of memory, multiplier of 1.10 (10%)
	// - Otherwise, multiplier of 1.05 (5%) - specified in DefaultMultiplier
	//
	// If the defaults were specified in the config they would look like:
	// ```yaml
	// multipliers:
	//   2048: 1.15
	//   4096: 1.10
	// ```
	Multipliers map[int]float64 `json:"multipliers" yaml:"multipliers"`
}

func (o Overhead) GetMultiplier(memoryLimit int64) float64 {
	// Default multiplier values.
	if !o.Override {
		if memoryLimit <= 2048 {
			return 1.15
		} else if memoryLimit <= 4096 {
			return 1.10
		}
		return 1.05
	}

	// This plucks the keys of the Multipliers map, so they can be sorted from
	// smallest to largest in order to correctly apply the proper multiplier.
	i := 0
	multipliers := make([]int, len(o.Multipliers))
	for k := range o.Multipliers {
		multipliers[i] = k
		i++
	}
	sort.Ints(multipliers)

	// Loop through the memory values in order (smallest to largest)
	for _, m := range multipliers {
		// If the server's memory limit exceeds the modifier's limit, don't apply it.
		if memoryLimit > int64(m) {
			continue
		}
		return o.Multipliers[m]
	}

	return o.DefaultMultiplier
}

// RuntimePath resolves a runtime name to its directory on this host.
//
// Returns an empty string when the name is unknown or unset, which means the
// server inherits the host PATH unchanged.
func (r RuntimeConfiguration) RuntimePath(name string) string {
	if name == "" || len(r.Runtimes) == 0 {
		return ""
	}
	if p, ok := r.Runtimes[name]; ok {
		return p
	}
	// Egg authors and operators will not always agree on capitalisation.
	for k, v := range r.Runtimes {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// ApplyRuntime prepends a runtime's bin directory to the PATH entry of env and
// exports RUNTIME_PATH, returning the adjusted environment.
//
// Prepending rather than replacing matters: an install script may still need
// tools from the host PATH, and a runtime that shadows them would break it.
func (r RuntimeConfiguration) ApplyRuntime(name string, env []string) []string {
	dir := r.RuntimePath(name)
	if dir == "" {
		return env
	}

	bin := filepath.Join(dir, "bin")
	if _, err := os.Stat(bin); err != nil {
		// Some runtimes are laid out without a bin subdirectory.
		bin = dir
	}

	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, e := range env {
		// Windows environment variables are case-insensitive, and the inherited
		// block may spell it "Path".
		if k, v, ok := strings.Cut(e, "="); ok && strings.EqualFold(k, "PATH") {
			out = append(out, k+"="+bin+string(os.PathListSeparator)+v)
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced {
		out = append(out, "PATH="+bin)
	}

	return append(out, "RUNTIME_PATH="+bin)
}
