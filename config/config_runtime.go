package config

import (
	"sort"
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
	//   "pool"   — each server is assigned a distinct pre-created local account
	//              from Accounts. Strongest isolation available without admin
	//              rights at runtime, since NTFS ACLs then separate servers.
	//   "shared" — every server runs as the account named by Shared. Simple, and
	//              acceptable only for single-tenant nodes where every server is
	//              already trusted equally.
	Isolation string `default:"pool" json:"isolation" yaml:"isolation"`

	// Accounts is the pool of local accounts available to servers when Isolation
	// is "pool". Create these once at install time; the daemon does not create
	// accounts itself, as that requires administrator rights it should not hold
	// while running.
	//
	// A node can run at most len(Accounts) servers concurrently.
	Accounts []PoolAccount `json:"accounts" yaml:"accounts"`

	// Shared names the account used when Isolation is "shared".
	Shared string `default:"" json:"shared" yaml:"shared"`
}

// For returns the local account credentials a given server should run under.
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
