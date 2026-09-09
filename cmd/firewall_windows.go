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

	known := make([]string, 0, len(servers))
	for _, s := range servers {
		known = append(known, s.ID())
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
