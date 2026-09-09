//go:build windows

// Package winfw manages the Windows Firewall rules that let a server be reached.
//
// Under Docker this was implicit: publishing a container port opened the path to
// it, and an unpublished port was simply unreachable. Windows has no such
// coupling. A server binds whatever the Panel allocated it, the Windows Firewall
// blocks inbound connections to it by default, and nothing joins the two — which
// presents as a server that starts cleanly, reports healthy, and that nobody can
// connect to.
//
// So the daemon opens exactly the allocations the Panel assigned, and closes
// them when the server is deleted. This is only possible because managed
// isolation already requires administrator rights; under pool or shared
// isolation the calls fail and are reported as warnings, leaving the operator
// where they were before.
//
// Implemented over netsh rather than the INetFwPolicy2 COM interface. The COM
// API is the supported one, but reaching it from Go means either a new
// dependency or several hundred lines of hand-bound vtables, and netsh is
// present on every supported Windows version, needs nothing, and is called here
// with fully quoted arguments rather than a constructed command line.
package winfw

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// rulePrefix identifies every rule this daemon creates.
//
// Also how they are found again: a rule is named deterministically from the
// server's UUID, so removing one needs no stored state and survives the daemon
// being restarted, reinstalled, or replaced.
const rulePrefix = "win-wings-"

// commandTimeout bounds a netsh invocation. Adding a rule is near-instant; the
// timeout exists so that a wedged firewall service cannot stall a server start.
const commandTimeout = 30 * time.Second

// protocols are opened together because a Pterodactyl allocation does not say
// which one it is.
//
// The Panel models an allocation as an IP and a port, with no protocol, and eggs
// rely on that: a Source engine server wants UDP for game traffic and TCP for
// RCON on the same number, and a Minecraft server wants TCP for the game and UDP
// for query. Opening one and guessing wrong produces a server that half works,
// which is worse to diagnose than one that does not work at all.
var protocols = []string{"TCP", "UDP"}

// RuleName returns the firewall rule this daemon uses for a server's ports on
// one protocol.
func RuleName(uuid, protocol string) string {
	return rulePrefix + uuid + "-" + strings.ToLower(protocol)
}

// Binding is one server's assigned ports, keyed by the address they are on.
type Binding map[string][]int

// Apply opens a server's allocations, replacing any rules it already has.
//
// Replacing rather than amending is deliberate: allocations change on the Panel
// without the server being recreated, and a rule left behind from a previous
// allocation would keep a port open after it had been handed to another server.
func Apply(uuid, serverName string, b Binding) error {
	if err := Remove(uuid); err != nil {
		return err
	}

	ips, ports := flatten(b)
	if len(ports) == 0 {
		return nil
	}

	for _, protocol := range protocols {
		args := []string{
			"advfirewall", "firewall", "add", "rule",
			"name=" + RuleName(uuid, protocol),
			"dir=in",
			"action=allow",
			"protocol=" + protocol,
			"localport=" + joinInts(ports),
			"profile=any",
			"enable=yes",
			"description=" + description(uuid, serverName),
		}
		// An allocation on 0.0.0.0 means any address, which is netsh's default
		// and cannot be written as a literal.
		if len(ips) > 0 {
			args = append(args, "localip="+strings.Join(ips, ","))
		}

		if out, err := netsh(args...); err != nil {
			return fmt.Errorf("winfw: could not open %s ports for server %s: %w: %s",
				protocol, uuid, err, out)
		}
	}

	return nil
}

// Remove closes a server's ports.
//
// Deleting a rule that does not exist is not an error: netsh reports it, and a
// server that never had rules must still be removable.
func Remove(uuid string) error {
	for _, protocol := range protocols {
		out, err := netsh("advfirewall", "firewall", "delete", "rule",
			"name="+RuleName(uuid, protocol))
		if err != nil && !isNoRulesMatched(out) {
			return fmt.Errorf("winfw: could not close ports for server %s: %w: %s",
				uuid, err, out)
		}
	}
	return nil
}

// Prune removes rules belonging to servers this node no longer has.
//
// Rules outlive the daemon, so a server deleted while it was stopped leaves its
// ports open indefinitely. Called at boot, once the server list is known.
//
// Best effort by design. It enumerates through PowerShell, because netsh can
// only list every rule on the host in a localised text format that is not worth
// parsing; if that is unavailable, nothing is pruned and the daemon still runs.
func Prune(known []string) ([]string, error) {
	keep := make(map[string]bool, len(known)*2)
	for _, uuid := range known {
		for _, protocol := range protocols {
			keep[strings.ToLower(RuleName(uuid, protocol))] = true
		}
	}

	names, err := listRuleNames()
	if err != nil {
		return nil, err
	}

	var removed []string
	for _, name := range names {
		if !strings.HasPrefix(strings.ToLower(name), rulePrefix) {
			continue
		}
		if keep[strings.ToLower(name)] {
			continue
		}
		if out, err := netsh("advfirewall", "firewall", "delete", "rule", "name="+name); err != nil {
			if !isNoRulesMatched(out) {
				return removed, fmt.Errorf("winfw: could not remove the orphaned rule %q: %w: %s",
					name, err, out)
			}
			continue
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// Available reports whether firewall rules can be managed at all, and why not.
//
// Checked once at boot so that a host with the firewall service disabled, or a
// daemon without the rights to change it, says so plainly instead of logging a
// failure for every server it starts.
//
// Probes with a *write* rather than a query, because the two do not agree:
// `netsh advfirewall show` succeeds for any account, while every rule change
// requires elevation. A read-only probe would report the firewall as manageable
// on exactly the unprivileged hosts where it is not.
//
// The write chosen is the deletion of a rule that cannot exist, which changes
// nothing on a host where it succeeds. Its failure is the signal: "no rules
// match" means the call was permitted and found nothing, anything else means it
// was not permitted.
func Available() error {
	out, err := netsh("advfirewall", "firewall", "delete", "rule",
		"name="+rulePrefix+"availability-probe")
	if err == nil || isNoRulesMatched(out) {
		return nil
	}
	return fmt.Errorf("winfw: firewall rules cannot be changed: %w: %s", err, out)
}

// flatten reduces a binding to the distinct addresses and ports to open.
//
// One rule per protocol carrying every address and every port, rather than a
// rule per address-port pair. netsh treats the lists as a cross product, so a
// server with allocations on two different addresses ends up with each of its
// ports open on both. That is wider than the Panel assigned, and accepted: the
// alternative is a rule per pair, which multiplies the rules on a busy node and
// makes removal depend on knowing what the allocations were rather than only the
// server's UUID. Servers with allocations spread across several host addresses
// are rare; servers whose ports are silently left open after deletion would not
// be.
func flatten(b Binding) (ips []string, ports []int) {
	seenIP := make(map[string]bool, len(b))
	seenPort := make(map[int]bool)
	wildcard := false

	for ip, list := range b {
		// 0.0.0.0 and :: mean "every address". netsh spells that as the absence
		// of localip, so recording them would narrow the rule to a literal
		// address that nothing is bound to.
		if ip == "" || ip == "0.0.0.0" || ip == "::" {
			wildcard = true
		} else if !seenIP[ip] {
			seenIP[ip] = true
			ips = append(ips, ip)
		}
		for _, p := range list {
			if p < 1 || p > 65535 || seenPort[p] {
				continue
			}
			seenPort[p] = true
			ports = append(ports, p)
		}
	}

	// Sorted so that a rule's contents do not churn between daemon restarts
	// purely because Go randomises map iteration.
	sort.Strings(ips)
	sort.Ints(ports)

	// A single wildcard allocation makes every specific address redundant. Left
	// in the list, those addresses would instead *narrow* the rule and quietly
	// close the port the wildcard allocation was meant to open.
	if wildcard {
		ips = nil
	}
	return ips, ports
}

func joinInts(ports []int) string {
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = strconv.Itoa(p)
	}
	return strings.Join(out, ",")
}

// description is what an operator sees in wf.msc next to the rule.
//
// Worth spending characters on: somebody looking at an unexplained open port
// should be able to tell which server owns it without consulting the Panel.
func description(uuid, serverName string) string {
	if serverName == "" {
		return "win-wings: allocations for server " + uuid
	}
	return fmt.Sprintf("win-wings: allocations for %q (%s)", serverName, uuid)
}

// isNoRulesMatched reports whether netsh failed only because there was nothing
// to delete.
//
// netsh returns a non-zero exit code for this, and its message is localised, so
// the English text is checked and the code is treated as authoritative only
// where the text does not match. Getting this wrong in the permissive direction
// would hide real failures, so it errs the other way: an unrecognised message is
// an error.
func isNoRulesMatched(out string) bool {
	return strings.Contains(strings.ToLower(out), "no rules match")
}

func netsh(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	// Absolute path: netsh is being run by a service, and resolving it through
	// PATH would let a writable directory earlier in PATH decide what runs as
	// an administrator.
	cmd := exec.CommandContext(ctx, filepath.Join(systemRoot(), "System32", "netsh.exe"), args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// listRuleNames returns the display names of every firewall rule.
func listRuleNames() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	powershell := filepath.Join(systemRoot(), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.CommandContext(ctx, powershell,
		"-NoProfile", "-NonInteractive", "-Command",
		// Filtered in the query rather than afterwards: a host can have
		// thousands of rules and only this daemon's are of interest.
		"Get-NetFirewallRule -DisplayName '"+rulePrefix+"*' -ErrorAction SilentlyContinue | "+
			"Select-Object -ExpandProperty DisplayName")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("winfw: could not list firewall rules: %w", err)
	}

	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// systemRoot locates the Windows directory, falling back rather than returning
// an empty path that would resolve netsh through PATH.
func systemRoot() string {
	if v := os.Getenv("SystemRoot"); v != "" {
		return v
	}
	return `C:\Windows`
}

// Exists reports whether a server's rules are present.
//
// Used to confirm a write actually landed, through a different mechanism than
// the one that wrote it. netsh reporting success while no rule appears is not
// hypothetical: a group policy that forbids local firewall rules lets the rule
// be accepted and then discards it.
func Exists(uuid string) (bool, error) {
	names, err := listRuleNames()
	if err != nil {
		return false, err
	}
	want := strings.ToLower(RuleName(uuid, "TCP"))
	for _, n := range names {
		if strings.ToLower(n) == want {
			return true, nil
		}
	}
	return false, nil
}
