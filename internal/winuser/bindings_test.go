//go:build windows

package winuser

import (
	"os/user"
	"strings"
	"testing"
)

// TestProbeReadOnlyBindings exercises the netapi32 and advapi32 bindings that do
// not need privilege, against the account the test is running as. It is here to
// catch a wrong struct layout or a mistyped parameter, which are the failure
// modes these hand-written bindings actually have.
func TestProbeReadOnlyBindings(t *testing.T) {
	// The account the tests run as may be a domain account, which the network
	// management API does not answer for. Fall back to the built-in accounts
	// that exist on every Windows installation.
	name := ""
	candidates := []string{}
	if u, err := user.Current(); err == nil {
		n := u.Username
		if _, after, ok := strings.Cut(n, `\`); ok {
			n = after
		}
		candidates = append(candidates, n)
	}
	candidates = append(candidates, "Administrator", "DefaultAccount", "Guest", "WDAGUtilityAccount")

	var info *Info
	for _, c := range candidates {
		got, err := Lookup(c)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", c, err)
		}
		if got != nil {
			name, info = c, got
			break
		}
	}
	if info == nil {
		t.Skipf("no local account among %v", candidates)
	}
	t.Logf("Lookup: name=%q comment=%q flags=%#x", info.Name, info.Comment, info.Flags)
	if !strings.EqualFold(info.Name, name) {
		t.Errorf("USER_INFO_1 decoded the name as %q, want %q -- struct layout is wrong", info.Name, name)
	}

	groups, err := LocalGroups(name)
	if err != nil {
		t.Fatalf("LocalGroups(%q): %v", name, err)
	}
	t.Logf("LocalGroups: %v", groups)

	admin, err := IsAdministrator(name)
	if err != nil {
		t.Fatalf("IsAdministrator(%q): %v", name, err)
	}
	t.Logf("IsAdministrator: %v", admin)

	sid, err := SID(name)
	if err != nil {
		t.Fatalf("SID(%q): %v", name, err)
	}
	t.Logf("SID: %s", sid)
}

func TestLookupMissingAccountIsNotAnError(t *testing.T) {
	info, err := Lookup("winwings-no-such-account")
	if err != nil {
		t.Fatalf("a missing account should not be an error, got %v", err)
	}
	if info != nil {
		t.Fatalf("a missing account returned %+v", info)
	}
}
