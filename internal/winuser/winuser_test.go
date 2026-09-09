//go:build windows

package winuser

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// requireAdmin skips a test that cannot run without administrator rights.
//
// Creating an account is not something a developer machine should have to grant
// a test suite, so the account lifecycle tests are opt-in by virtue of the shell
// they run in. Everything that can be checked without privilege is checked
// unconditionally.
func requireAdmin(t *testing.T) {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	member, err := windows.GetCurrentProcessToken().IsMember(sid)
	if err != nil {
		t.Fatalf("IsMember: %v", err)
	}
	if !member {
		t.Skip("not running as an administrator; account lifecycle is not exercised")
	}
}

func TestGeneratePasswordSatisfiesComplexity(t *testing.T) {
	// The default Windows policy wants three of four character classes. All four
	// are guaranteed by construction, so a failure here means the guarantee
	// broke, not that a draw was unlucky.
	for i := 0; i < 200; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatalf("GeneratePassword: %v", err)
		}
		if len(pw) != passwordLength {
			t.Fatalf("password is %d characters, want %d", len(pw), passwordLength)
		}
		for name, class := range map[string]string{
			"upper": pwUpper, "lower": pwLower, "digit": pwDigit, "symbol": pwSymbol,
		} {
			if !strings.ContainsAny(pw, class) {
				t.Fatalf("password %q contains no %s character", pw, name)
			}
		}
	}
}

func TestGeneratePasswordDoesNotRepeat(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if seen[pw] {
			t.Fatalf("generated the same password twice: %q", pw)
		}
		seen[pw] = true
	}
}

func TestNameForFitsTheAccountLimit(t *testing.T) {
	// A name over 20 characters is rejected by NetUserAdd, which would turn into
	// "this server will not start" rather than anything diagnosable.
	cases := []struct{ prefix, uuid string }{
		{"", "1e4b9e5f-6f6a-4a63-9b1e-0c9a7d2b8f41"},
		{"ww-", "1e4b9e5f-6f6a-4a63-9b1e-0c9a7d2b8f41"},
		{"winwings", "1e4b9e5f-6f6a-4a63-9b1e-0c9a7d2b8f41"},
		{"ww-", "short"},
		{"ww-", ""},
	}
	for _, c := range cases {
		name := New(c.prefix).NameFor(c.uuid)
		if len(name) > maxAccountName {
			t.Errorf("NameFor(prefix=%q, %q) = %q, %d characters (limit %d)",
				c.prefix, c.uuid, name, len(name), maxAccountName)
		}
		if strings.ContainsAny(name, `"/\[]:;|=,+*?<>`) {
			t.Errorf("NameFor produced %q, which contains a character SAM rejects", name)
		}
	}
}

func TestNameForIsStableAndDistinct(t *testing.T) {
	m := New("")
	a := "1e4b9e5f-6f6a-4a63-9b1e-0c9a7d2b8f41"
	b := "1e4b9e5f-6f6a-4a63-9b1e-0c9a7d2b8f42"

	if m.NameFor(a) != m.NameFor(a) {
		t.Error("NameFor is not stable for one UUID")
	}
	// Distinct in the leading characters, which is what survives truncation.
	// Two UUIDs differing only in their last character would collide, which is
	// exactly why Ensure checks the marker before touching an account.
	if m.NameFor(a) == m.NameFor("2e4b9e5f-6f6a-4a63-9b1e-0c9a7d2b8f41") {
		t.Error("UUIDs differing in the first character produced the same account name")
	}
	if m.NameFor(a) != m.NameFor(b) {
		t.Log("note: these UUIDs did not collide, but the marker check is what guarantees safety")
	}
	if strings.ToLower(m.NameFor(a)) != m.NameFor(strings.ToUpper(a)) {
		t.Error("NameFor is case-sensitive; the Panel does not guarantee UUID casing")
	}
}

func TestInfoMarkerMatching(t *testing.T) {
	uuid := "1e4b9e5f-6f6a-4a63-9b1e-0c9a7d2b8f41"
	ours := Info{Comment: markerPrefix + uuid}
	if !ours.Managed() || !ours.ManagedFor(uuid) {
		t.Error("an account this daemon created was not recognised as its own")
	}
	if ours.ManagedFor("some-other-uuid") {
		t.Error("an account was claimed for the wrong server")
	}

	theirs := Info{Comment: "A user account for something else"}
	if theirs.Managed() || theirs.ManagedFor(uuid) {
		t.Error("an unrelated account was claimed as managed")
	}

	// An account with no description at all is the common case for one created
	// by hand, and must not be adopted.
	if (Info{}).Managed() {
		t.Error("an account with no description was claimed as managed")
	}
}

func TestAccountLifecycle(t *testing.T) {
	requireAdmin(t)

	const uuid = "ffffffff-0000-4000-8000-00000000test"
	m := New("wwt-")
	name := m.NameFor(uuid)
	t.Cleanup(func() { _ = Delete(name) })

	user, password, err := m.Ensure(uuid)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if user != name {
		t.Fatalf("Ensure returned %q, want %q", user, name)
	}
	if password == "" {
		t.Fatal("Ensure returned an empty password")
	}

	info, err := Lookup(name)
	if err != nil || info == nil {
		t.Fatalf("Lookup after Ensure: info=%v err=%v", info, err)
	}
	if !info.ManagedFor(uuid) {
		t.Errorf("the created account is not marked as ours: comment=%q", info.Comment)
	}

	// The account must be able to log on as a batch job, which is how a server
	// process is started under it, and must not be an administrator.
	admin, err := IsAdministrator(name)
	if err != nil {
		t.Fatalf("IsAdministrator: %v", err)
	}
	if admin {
		t.Error("a newly created server account is an administrator")
	}

	// Second call is cached and must not change the password.
	_, again, err := m.Ensure(uuid)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if again != password {
		t.Error("a second Ensure reissued the password instead of returning the cached one")
	}

	// A fresh manager stands in for a daemon restart: the account survives, the
	// password does not, and a new one is issued.
	restarted := New("wwt-")
	_, reissued, err := restarted.Ensure(uuid)
	if err != nil {
		t.Fatalf("Ensure after restart: %v", err)
	}
	if reissued == password {
		t.Error("the password was not reissued after a restart")
	}

	if err := m.Remove(uuid); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	info, err = Lookup(name)
	if err != nil {
		t.Fatalf("Lookup after Remove: %v", err)
	}
	if info != nil {
		t.Error("the account still exists after Remove")
	}
}

func TestEnsureRefusesAnAccountItDoesNotOwn(t *testing.T) {
	requireAdmin(t)

	const uuid = "eeeeeeee-0000-4000-8000-00000000test"
	m := New("wwt-")
	name := m.NameFor(uuid)
	t.Cleanup(func() { _ = Delete(name) })

	password, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for an operator's own account that happens to collide.
	if err := Create(name, password, "Not a win-wings account"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, _, err := m.Ensure(uuid); err == nil {
		t.Fatal("Ensure adopted an account it did not create")
	} else if !strings.Contains(err.Error(), "not created by this daemon") {
		t.Errorf("the error should say why it refused, got: %v", err)
	}

	if err := m.Remove(uuid); err == nil {
		t.Error("Remove deleted an account it did not create")
	}
	if info, _ := Lookup(name); info == nil {
		t.Error("the unrelated account was deleted")
	}
}
