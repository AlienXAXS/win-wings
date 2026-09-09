//go:build windows

package winsta

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestGrantToTheCurrentAccountIsSafeAndComplete exercises the whole path --
// reading the station and desktop DACLs, merging an entry, writing them back --
// against the account this process already runs as.
//
// Granting to ourselves is deliberate: the account already has access, so the
// merge is a no-op in effect while still proving that GetSecurityInfo,
// ACLFromEntries and SetSecurityInfo agree about the object type and that
// nothing is corrupted along the way. Granting to some other account would be a
// real, lasting change to the machine running the tests.
func TestGrantToTheCurrentAccountIsSafeAndComplete(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}

	name, err := Grant(user.User.Sid)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	t.Logf("station and desktop = %q", name)

	// STARTUPINFO.lpDesktop wants "station\desktop". A name missing either half
	// is accepted by CreateProcess and then resolves to nothing, which is the
	// failure this package exists to prevent.
	station, desktop, ok := strings.Cut(name, `\`)
	if !ok || station == "" || desktop == "" {
		t.Fatalf("Grant returned %q, want a station and desktop pair", name)
	}

	// A service runs on Service-0x0-3e7$; an interactive process on WinSta0.
	// Both are valid here -- the point is that a name was resolved rather than
	// guessed.
	t.Logf("station=%q desktop=%q", station, desktop)
}

func TestGrantIsCachedPerAccount(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}

	first, err := Grant(user.User.Sid)
	if err != nil {
		t.Fatal(err)
	}
	// Each grant is a read-modify-write of a DACL shared with every other
	// process on the station. Repeating it once per server start would be both
	// wasteful and a good way to lose an entry to a race.
	second, err := Grant(user.User.Sid)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("Grant returned %q then %q for the same account", first, second)
	}
}

// TestWithheldRightsStayWithheld pins the access masks.
//
// These are easy to widen by reflex when something does not work, and the
// consequence is not obvious: a game server running third-party code would gain
// the ability to read the screen, read the clipboard, or install input hooks on
// whichever station the daemon is running on -- which, when it is run in the
// foreground, is the operator's own.
func TestWithheldRightsStayWithheld(t *testing.T) {
	for name, bit := range map[string]uint32{
		"WINSTA_ACCESSCLIPBOARD": winstaAccessClipboard,
		"WINSTA_READSCREEN":      winstaReadScreen,
	} {
		if stationRights&bit != 0 {
			t.Errorf("%s is granted on the window station; it should not be", name)
		}
	}
	for name, bit := range map[string]uint32{
		"DESKTOP_HOOKCONTROL":     desktopHookControl,
		"DESKTOP_JOURNALRECORD":   desktopJournalRecord,
		"DESKTOP_JOURNALPLAYBACK": desktopJournalPlayback,
		"DESKTOP_SWITCHDESKTOP":   desktopSwitchDesktop,
	} {
		if desktopRights&bit != 0 {
			t.Errorf("%s is granted on the desktop; it should not be", name)
		}
	}

	// And the two that user32 genuinely needs during initialisation must stay.
	if stationRights&winstaAccessGlobalAtoms == 0 {
		t.Error("WINSTA_ACCESSGLOBALATOMS is required for user32 to initialise")
	}
	if stationRights&winstaExitWindows == 0 {
		t.Error("WINSTA_EXITWINDOWS is required for user32 to initialise")
	}
}
