package environment

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Allocations defines the addresses and ports assigned to a given server.
type Allocations struct {
	// ForceOutgoingIP is accepted from the Panel and ignored.
	//
	// Under Docker this created a dedicated bridge network that SNAT'd outgoing
	// traffic to the server's own IP, which matters to games that check their
	// public address. There is no equivalent without per-server network
	// namespaces, which Windows does not provide outside of containers.
	ForceOutgoingIP bool `json:"force_outgoing_ip"`
	// Defines the default allocation that should be used for this server. This is
	// what will be used for {SERVER_IP} and {SERVER_PORT} when modifying configuration
	// files or the startup arguments for a server.
	DefaultMapping struct {
		Ip   string `json:"ip"`
		Port int    `json:"port"`
	} `json:"default"`

	// Mappings contains all the ports that should be assigned to a given server
	// attached to the IP they correspond to.
	Mappings PortMappings `json:"mappings"`
}

// PortMappings is a set of ports keyed by the address they are assigned on.
//
// It exists only to survive how the Panel serialises an empty one. The Panel is
// PHP, and PHP does not distinguish an empty map from an empty list: json_encode
// renders both as [], so a server with no allocations arrives as
//
//	"mappings": []
//
// which is not an object and will not decode into a map. The failure lands
// during server creation, as
//
//	json: cannot unmarshal array into Go struct field ... of type map[string][]int
//
// and leaves the server uncreatable rather than merely unallocated.
type PortMappings map[string][]int

// UnmarshalJSON accepts either an object or the empty array PHP produces for an
// empty map.
//
// A non-empty array is still an error. That would mean the Panel sent a shape
// nothing here understands, and quietly treating it as "no allocations" would
// turn a protocol mismatch into a server that starts and is unreachable, which
// is materially harder to diagnose than a decode failure.
func (m *PortMappings) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if bytes.Equal(trimmed, []byte("null")) {
		*m = nil
		return nil
	}
	if bytes.Equal(trimmed, []byte("[]")) {
		*m = PortMappings{}
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return fmt.Errorf("environment: allocations.mappings arrived as a non-empty JSON "+
			"array (%.64s); it must be an object keyed by address, or [] when there are "+
			"none", trimmed)
	}

	// Decoded into the underlying map type rather than into *m, so this is an
	// ordinary map decode and not a recursive call back into this function.
	var plain map[string][]int
	if err := json.Unmarshal(trimmed, &plain); err != nil {
		return err
	}
	*m = plain
	return nil
}

// Bindings returns every port assigned to this server, keyed by the IP it is
// assigned on.
//
// Unlike the Docker environment, nothing here publishes or reserves these ports.
// Windows binds nothing on a server's behalf: the assigned address and port are
// passed to the process through {SERVER_IP} and {SERVER_PORT} and it is up to
// the server to honour them.
//
// That is a real reduction in what the daemon can guarantee. A misconfigured or
// malicious server can bind any free port on the host, and nothing here stops
// it.
//
// What the daemon does do is open these ports, and only these, in the Windows
// Firewall -- see internal/winfw. That makes the allocation reachable and leaves
// anything else the server binds blocked from outside, which recovers most of
// what publishing a container port used to provide. It is not the same
// guarantee: a server can still bind a port another server was allocated, and
// win the race.
func (a *Allocations) Bindings() map[string][]int {
	out := make(map[string][]int, len(a.Mappings))
	for ip, ports := range a.Mappings {
		valid := make([]int, 0, len(ports))
		for _, port := range ports {
			if port < 1 || port > 65535 {
				continue
			}
			valid = append(valid, port)
		}
		if len(valid) > 0 {
			out[ip] = valid
		}
	}
	return out
}
