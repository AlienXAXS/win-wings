package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// StatsAgentConfiguration configures the node load agent.
//
// The Panel's Free Servers extension sizes its stock by polling every node for
// host CPU, memory and disk figures. On Linux nodes that is a separate Docker
// container (ax-freeservers/node-agent) reading /proc; here there is no
// container engine, so the daemon serves the same endpoints itself, on its own
// listener, from the Windows APIs.
//
// It is a second listener rather than a route on the main API because the two
// are consumed differently. The main API is HTTPS with the node token the Panel
// holds; the agent is polled over plain HTTP with a per-node bearer token the
// operator pastes into the Free Servers admin page, and firewalled to the
// Panel's address. Keeping them apart means the load token can be rotated, or
// leaked, without touching the credential that controls every server.
type StatsAgentConfiguration struct {
	// Enabled starts the listener. Off by default: a node not used for free
	// servers has no reason to expose load figures.
	Enabled bool `default:"false" json:"enabled" yaml:"enabled"`

	// Host is the address the listener binds. The Panel polls the node's FQDN
	// unless a per-node address override is set, so the default of every
	// address is usually right.
	Host string `default:"0.0.0.0" json:"host" yaml:"host"`

	// Port must match the Panel's "Agent Port" setting, or that node's port
	// override. 8081 is what the Linux agent and the Panel both default to.
	Port int `default:"8081" json:"port" yaml:"port"`

	// Token is the shared secret the Panel sends as "Authorization: Bearer".
	// Left empty with the agent enabled, the daemon generates one at boot,
	// writes it back to this file and says so in the log; copy it into the
	// Panel's Node Agent Status table. Supports the file:// prefix handled by
	// Expand so the secret can live outside this file.
	Token string `json:"-" yaml:"token"`

	// OpenFirewall writes an inbound allow rule for Port when the daemon is
	// managing the Windows Firewall (system.firewall.manage). The rule admits
	// any source; narrow it to the Panel's address by hand if the node is on
	// the open internet, or turn this off and write your own.
	OpenFirewall bool `default:"true" json:"open_firewall" yaml:"open_firewall"`

	// resolved is Token after Expand, kept off disk.
	resolved string
}

// StatsAgentFirewallRuleID is the pseudo server ID under which the agent's firewall rule
// is written, so that it is named and pruned like any other rule this daemon
// owns: win-wings-stats-agent-tcp.
const StatsAgentFirewallRuleID = "stats-agent"

// ResolvedToken returns the bearer token requests must present. Empty until
// ResolveStatsAgentToken has run.
func (s *StatsAgentConfiguration) ResolvedToken() string {
	return s.resolved
}

// Address is the host:port the listener binds.
func (s *StatsAgentConfiguration) Address() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// ResolveStatsAgentToken expands the configured token, generating and
// persisting one when the agent is enabled without any.
//
// Generating rather than refusing to start: the Linux agent's install one-liner
// does the same (`openssl rand -hex 32`) and prints the result. Here it lands in
// config.yml, which is already the file that holds the node token, so nothing
// gets weaker. Returns whether a token was generated so the caller can tell the
// operator where to find it.
func (c *Configuration) ResolveStatsAgentToken() (generated bool, err error) {
	s := &c.StatsAgent
	if !s.Enabled {
		s.resolved = ""
		return false, nil
	}

	if strings.TrimSpace(s.Token) == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return false, fmt.Errorf("config: could not generate a stats agent token: %w", err)
		}
		s.Token = hex.EncodeToString(b)
		generated = true
	}

	v, err := Expand(s.Token)
	if err != nil {
		return generated, fmt.Errorf("config: could not resolve stats_agent.token: %w", err)
	}
	if strings.TrimSpace(v) == "" {
		return generated, fmt.Errorf("config: stats_agent.token resolved to an empty value")
	}
	s.resolved = strings.TrimSpace(v)
	return generated, nil
}
