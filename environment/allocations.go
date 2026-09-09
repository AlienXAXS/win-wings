package environment

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
	Mappings map[string][]int `json:"mappings"`
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
