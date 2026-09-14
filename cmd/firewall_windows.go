//go:build windows

package cmd

import (
	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/winfw"
	"github.com/pterodactyl/wings/server"
)

// pruneFirewallRules removes firewall rules for servers this node no longer has,
// and reports once whether rules can be managed at all.
//
// Both belong at boot rather than per server. A rule outlives the daemon, so a
// server deleted while this node was stopped leaves its ports open with nothing
// left to notice; and a host where the firewall cannot be reached should say so
// once, not once per server start.
func pruneFirewallRules(servers []*server.Server) {
	if !config.Get().System.Firewall.Manage {
		log.Debug("firewall management is disabled; servers' ports must be opened by hand")
		return
	}

	if err := winfw.Available(); err != nil {
		log.WithField("error", err).
			Warn("the Windows Firewall cannot be managed, so servers' allocated ports will " +
				"not be opened automatically. Open them by hand, or set " +
				"system.firewall.manage to false to stop this being attempted")
		return
	}

	if !config.Get().System.Firewall.Prune {
		return
	}

	known := make([]string, 0, len(servers)+1)
	for _, s := range servers {
		known = append(known, s.ID())
	}
	// The stats agent's rule is written under a fixed pseudo ID. Kept while
	// the agent is enabled; pruned like any orphan once it is turned off.
	if sa := config.Get().StatsAgent; sa.Enabled && sa.OpenFirewall {
		known = append(known, config.StatsAgentFirewallRuleID)
	}

	removed, err := winfw.Prune(known)
	if err != nil {
		// Best effort. Failing to tidy up is not a reason to refuse to boot, and
		// the rules that matter are written per server regardless.
		log.WithField("error", err).Debug("could not prune orphaned firewall rules")
		return
	}
	if len(removed) > 0 {
		log.WithField("rules", removed).
			Info("removed firewall rules belonging to servers this node no longer has")
	}
}

// openStatsAgentPort writes the inbound rule for the stats agent's listener.
//
// Runs after pruning so that the rule survives a boot where it was already
// present, and is written fresh when the port has changed. Failure is a
// warning: the Panel's poll then fails visibly on its Node Agent Status page,
// which is a better place to notice than a daemon that refused to start.
func openStatsAgentPort() {
	c := config.Get()
	sa := c.StatsAgent
	if !sa.Enabled || !sa.OpenFirewall {
		return
	}
	if !c.System.Firewall.Manage {
		log.WithField("port", sa.Port).Info("stats_agent.open_firewall is set but firewall management " +
			"is disabled; open the stats agent port by hand")
		return
	}
	if err := winfw.Available(); err != nil {
		log.WithFields(log.Fields{"error": err, "port": sa.Port}).
			Warn("the stats agent port could not be opened in the Windows Firewall; open it by hand")
		return
	}
	if err := winfw.OpenListener(config.StatsAgentFirewallRuleID, "stats agent (Free Servers node load)", sa.Host, sa.Port); err != nil {
		log.WithFields(log.Fields{"error": err, "port": sa.Port}).
			Warn("the stats agent port could not be opened in the Windows Firewall; open it by hand")
		return
	}
	log.WithFields(log.Fields{"port": sa.Port, "rule": winfw.RuleName(config.StatsAgentFirewallRuleID, "TCP")}).
		Info("opened the stats agent port in the Windows Firewall")
}
