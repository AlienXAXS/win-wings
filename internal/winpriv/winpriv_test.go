//go:build windows

package winpriv

import "testing"

// TestCurrentReportsSomething exercises the token inspection against whatever
// account the tests run as, and prints it. The values differ between a developer
// workstation and a service host, so this asserts internal consistency rather
// than specific results.
func TestCurrent(t *testing.T) {
	s, err := Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}

	t.Logf("%s", s.Describe())

	if s.Account == "" {
		t.Error("account name is empty")
	}
	if s.CanLaunchAsUser && len(s.Missing) != 0 {
		t.Errorf("CanLaunchAsUser is true but %v are missing", s.Missing)
	}
	if !s.CanLaunchAsUser && len(s.Missing) == 0 {
		t.Error("CanLaunchAsUser is false but nothing is reported missing")
	}

	// LocalSystem holds both privileges by definition; if we ever report
	// otherwise the privilege enumeration is broken.
	if s.IsSystem && !s.CanLaunchAsUser {
		t.Errorf("running as LocalSystem but missing %v — privilege enumeration is wrong", s.Missing)
	}
}

// TestPrivilegeEnumerationFindsKnownPrivileges checks the LUID-to-name path
// against a privilege every token has.
func TestPrivilegeEnumerationFindsKnownPrivileges(t *testing.T) {
	held, err := heldPrivileges(currentToken())
	if err != nil {
		t.Fatalf("heldPrivileges: %v", err)
	}
	if len(held) == 0 {
		t.Fatal("no privileges enumerated; the LUID-to-name lookup is failing")
	}

	names := make([]string, 0, len(held))
	for n := range held {
		names = append(names, n)
	}
	t.Logf("%d privileges held: %v", len(held), names)

	// Every token holds SeChangeNotifyPrivilege (bypass traverse checking).
	if !held["SeChangeNotifyPrivilege"] {
		t.Error("SeChangeNotifyPrivilege is absent, which no real token should be")
	}
}
