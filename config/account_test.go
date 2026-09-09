package config

import "testing"

// TestAccountDefaultsAreCoherent guards the account defaults against the same
// silent-empty-tag failure that TestDefaultPathsAreWellFormed covers for paths,
// and against the two settings drifting out of agreement with the validator.
//
// An empty isolation default would be rejected at boot with "must be managed,
// pool or shared", which is a confusing thing for a fresh install to say. An
// empty prefix would be worse: the daemon would refuse to start, and if that
// check were ever relaxed it would create accounts named after the UUID alone
// and lose the only marker distinguishing its accounts from the operator's.
func TestAccountDefaultsAreCoherent(t *testing.T) {
	c, err := NewAtPath("ignored.yml")
	if err != nil {
		t.Fatalf("building a default configuration: %v", err)
	}
	a := c.System.Account

	switch a.Isolation {
	case "managed", "pool", "shared":
	default:
		t.Errorf("system.account.isolation defaults to %q, which validateAccounts rejects",
			a.Isolation)
	}

	if a.Isolation != "managed" {
		t.Errorf("system.account.isolation defaults to %q; a fresh install should get "+
			"managed isolation, which needs nothing created by hand", a.Isolation)
	}

	if a.Prefix == "" {
		t.Error("system.account.prefix has an empty default, which the validator refuses")
	}
	// A Windows local account name is capped at 20 characters and the rest is
	// the server's UUID; validateAccounts enforces 8, so the default must fit.
	if len(a.Prefix) > 8 {
		t.Errorf("system.account.prefix defaults to %q (%d characters), which the "+
			"validator rejects", a.Prefix, len(a.Prefix))
	}

	if a.AllowElevated {
		t.Error("system.account.allow_elevated defaults to true; running as LocalSystem " +
			"should have to be asked for")
	}
}

// TestForIgnoresManagedIsolation documents that AccountConfiguration.For does not
// answer for managed isolation.
//
// Its accounts are created on demand with passwords that exist only in memory,
// so there is nothing in configuration to return. A caller reaching this instead
// of internal/accounts.For would silently run every server as the daemon's own
// account — the exact failure the isolation exists to prevent — so the empty
// return is asserted rather than left to chance.
func TestForIgnoresManagedIsolation(t *testing.T) {
	a := AccountConfiguration{
		Isolation: "managed",
		Prefix:    "ww-",
		Accounts:  []PoolAccount{{Username: "should-not-be-used", Password: "x"}},
	}
	if user, pass := a.For("1e4b9e5f-6f6a-4a63-9b1e-0c9a7d2b8f41"); user != "" || pass != "" {
		t.Errorf("For returned %q/%q for managed isolation; it must return nothing so "+
			"that a caller bypassing internal/accounts fails visibly", user, pass)
	}
}

func TestForAssignsPoolAccountsStably(t *testing.T) {
	a := AccountConfiguration{
		Isolation: "pool",
		Accounts: []PoolAccount{
			{Username: "srv01", Password: "a"},
			{Username: "srv02", Password: "b"},
			{Username: "srv03", Password: "c"},
		},
	}

	const uuid = "1e4b9e5f-6f6a-4a63-9b1e-0c9a7d2b8f41"
	first, _ := a.For(uuid)
	if first == "" {
		t.Fatal("pool isolation returned no account")
	}
	// Assignment is by hash rather than by allocation order precisely so that it
	// survives a restart without anything being persisted.
	for i := 0; i < 10; i++ {
		if again, _ := a.For(uuid); again != first {
			t.Fatalf("pool assignment is not stable: got %q then %q", first, again)
		}
	}
}
