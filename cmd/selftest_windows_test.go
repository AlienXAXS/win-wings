//go:build windows

package cmd

import (
	"testing"

	"github.com/pterodactyl/wings/internal/winuser"
)

// TestSelfTestUUIDsDoNotCollide guards the suite's own fixtures.
//
// A Windows account name is capped at 20 characters, so only the leading
// characters of a UUID survive into it. Two fixture UUIDs differing only at the
// end map onto one account, the daemon refuses to reuse it — correctly, since
// doing so would hand one server another's identity — and the entire isolation
// half of the self test fails with an error about a collision rather than
// telling anyone anything about the host.
//
// That is exactly what happened the first time this ran on a real machine. Real
// v4 UUIDs are random throughout and do not have this problem; hand-written
// fixtures do.
func TestSelfTestUUIDsDoNotCollide(t *testing.T) {
	m := winuser.New("wwt-")

	a, b := m.NameFor(selfTestUUIDs[0]), m.NameFor(selfTestUUIDs[1])
	if a == b {
		t.Fatalf("both self-test servers map onto the account %q; they must differ within "+
			"the first characters of their UUIDs, not just at the end", a)
	}
	t.Logf("%s -> %s", selfTestUUIDs[0], a)
	t.Logf("%s -> %s", selfTestUUIDs[1], b)
}
