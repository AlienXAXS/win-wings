//go:build windows

package winfw

import (
	"reflect"
	"strings"
	"testing"
)

func TestRuleNameIsDerivedFromTheUUID(t *testing.T) {
	// Removal finds a rule by recomputing its name, so the name must depend on
	// nothing but the UUID and the protocol. A name that varied with, say, the
	// server's display name would leave rules behind whenever it was renamed.
	const uuid = "5e1f7e51-0000-4000-8000-000000000001"

	tcp := RuleName(uuid, "TCP")
	if tcp != RuleName(uuid, "tcp") {
		t.Error("RuleName is sensitive to the case of the protocol")
	}
	if tcp == RuleName(uuid, "UDP") {
		t.Error("TCP and UDP produced the same rule name")
	}
	if !strings.HasPrefix(tcp, rulePrefix) {
		t.Errorf("%q does not carry the prefix that identifies this daemon's rules", tcp)
	}
	if !strings.Contains(tcp, uuid) {
		t.Errorf("%q does not name the server it belongs to", tcp)
	}
}

func TestFlattenDeduplicatesAndSorts(t *testing.T) {
	ips, ports := flatten(Binding{
		"10.0.0.5": {25565, 25575, 25565},
		"10.0.0.4": {25565, 8080},
	})

	if !reflect.DeepEqual(ips, []string{"10.0.0.4", "10.0.0.5"}) {
		t.Errorf("ips = %v, want them deduplicated and sorted", ips)
	}
	// Sorted so a rule's contents do not churn between restarts purely because
	// Go randomises map iteration, which would rewrite every rule on every boot.
	if !reflect.DeepEqual(ports, []int{8080, 25565, 25575}) {
		t.Errorf("ports = %v, want them deduplicated and sorted", ports)
	}
}

// TestFlattenTreatsWildcardsAsEveryAddress covers the case that would silently
// close a port rather than open it.
//
// netsh expresses "any address" as the absence of localip. A wildcard allocation
// recorded as the literal 0.0.0.0, or left alongside specific addresses, narrows
// the rule to those addresses — so the allocation the Panel meant to be reachable
// everywhere becomes reachable nowhere.
func TestFlattenTreatsWildcardsAsEveryAddress(t *testing.T) {
	for name, b := range map[string]Binding{
		"only a wildcard":       {"0.0.0.0": {25565}},
		"ipv6 wildcard":         {"::": {25565}},
		"empty address":         {"": {25565}},
		"wildcard and specific": {"0.0.0.0": {25565}, "10.0.0.4": {25566}},
	} {
		ips, ports := flatten(b)
		if len(ips) != 0 {
			t.Errorf("%s: ips = %v, want none so that the rule applies to every address",
				name, ips)
		}
		if len(ports) == 0 {
			t.Errorf("%s: no ports were returned", name)
		}
	}
}

func TestFlattenRejectsImpossiblePorts(t *testing.T) {
	_, ports := flatten(Binding{"10.0.0.4": {0, -1, 70000, 25565}})
	if !reflect.DeepEqual(ports, []int{25565}) {
		t.Errorf("ports = %v, want only the valid one; netsh rejects the whole rule "+
			"if any port in the list is out of range", ports)
	}
}

func TestFlattenOnEmptyBinding(t *testing.T) {
	ips, ports := flatten(Binding{})
	if len(ips) != 0 || len(ports) != 0 {
		t.Errorf("an empty binding produced ips=%v ports=%v", ips, ports)
	}
}

func TestJoinInts(t *testing.T) {
	if got := joinInts([]int{25565, 25575}); got != "25565,25575" {
		t.Errorf("joinInts = %q, want a comma-separated list for netsh localport", got)
	}
}

// TestIsNoRulesMatchedErrsTowardsReporting checks the direction of the
// ambiguity.
//
// netsh returns a non-zero exit code both when a rule did not exist and when the
// call was refused, and its messages are localised. Treating an unrecognised
// message as "nothing to delete" would silently swallow an access-denied
// failure, leaving ports open with nothing logged, so anything unrecognised must
// be reported.
func TestIsNoRulesMatchedErrsTowardsReporting(t *testing.T) {
	if !isNoRulesMatched("No rules match the specified criteria.") {
		t.Error("the ordinary nothing-to-delete message was not recognised")
	}
	if !isNoRulesMatched("no rules match the specified criteria.") {
		t.Error("the check should not depend on capitalisation")
	}
	for _, out := range []string{
		"The requested operation requires elevation (Run as administrator).",
		"Access is denied.",
		"",
		"Es wurden keine Regeln gefunden.",
	} {
		if isNoRulesMatched(out) {
			t.Errorf("%q was treated as nothing-to-delete; a real failure would be hidden", out)
		}
	}
}

func TestDescriptionNamesTheServer(t *testing.T) {
	const uuid = "5e1f7e51-0000-4000-8000-000000000001"

	// Somebody looking at an unexplained open port in wf.msc should be able to
	// tell which server owns it without consulting the Panel.
	if d := description(uuid, "Bob's Minecraft"); !strings.Contains(d, uuid) ||
		!strings.Contains(d, "Bob's Minecraft") {
		t.Errorf("description = %q, want it to name both the server and its UUID", d)
	}
	if d := description(uuid, ""); !strings.Contains(d, uuid) {
		t.Errorf("description = %q, want it to name the server even with no display name", d)
	}
}

func TestDescriptionHasNoQuotes(t *testing.T) {
	// netsh re-parses the command line itself, so a double quote anywhere in an
	// argument value derails its tokenizer and the call is rejected with an
	// error about IP addresses. Server names come from the Panel.
	for _, name := range []string{
		`win-wings self test`,
		`Bob's "Best" Server`,
		"line\nbreak",
		"tab\there",
	} {
		got := description("5e1f7e5f-0000-4000-8000-00000000fw01", name)
		if strings.ContainsRune(got, '"') {
			t.Errorf("description(%q) contains a double quote: %q", name, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("description(%q) contains a control character %#U: %q", name, r, got)
			}
		}
	}
}

func TestDescriptionWithoutAName(t *testing.T) {
	const uuid = "5e1f7e5f-0000-4000-8000-00000000fw01"
	// A name that sanitises away to nothing must fall back rather than produce
	// "allocations for  (uuid)".
	if got, want := description(uuid, "\x00\x01"), "win-wings: allocations for server "+uuid; got != want {
		t.Errorf("description with an empty name = %q, want %q", got, want)
	}
}

func TestSanitiseIsBounded(t *testing.T) {
	if got := sanitise(strings.Repeat("a", 500)); len(got) > maxDescriptionName {
		t.Errorf("sanitise returned %d bytes, want at most %d", len(got), maxDescriptionName)
	}
}
