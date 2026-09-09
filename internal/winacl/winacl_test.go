//go:build windows

package winacl

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// readDACL returns the DACL of a path in SDDL form, which is compact enough to
// assert against.
func readDACL(t *testing.T, path string) string {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	return sd.String()
}

func currentAccount(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	// user.Current returns DOMAIN\name; LookupSID accepts that form.
	return u.Username
}

func TestGrantExclusiveWriteAppliesAndProtects(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}

	before := readDACL(t, target)
	t.Logf("before: %s", before)

	if err := GrantExclusiveWrite(target, currentAccount(t)); err != nil {
		t.Fatalf("GrantExclusiveWrite: %v", err)
	}

	after := readDACL(t, target)
	t.Logf("after:  %s", after)

	if after == before {
		t.Error("the DACL did not change")
	}

	// "P" in the D: flags means the DACL is protected — inheritance from the
	// parent is severed, which is the whole point. Without it the servers root's
	// rules would leak back in.
	if !strings.Contains(after, "D:P") && !strings.Contains(after, "D:PAI") {
		t.Errorf("DACL is not protected from inheritance: %s", after)
	}

	// The account we granted must appear.
	sid, _, _, err := windows.LookupSID("", currentAccount(t))
	if err != nil {
		t.Fatalf("LookupSID: %v", err)
	}
	if !strings.Contains(after, sid.String()) {
		t.Errorf("granted account %s does not appear in the DACL: %s", sid, after)
	}
}

func TestGrantExclusiveWriteWithNoAccountIsNoop(t *testing.T) {
	dir := t.TempDir()
	before := readDACL(t, dir)

	// Shared isolation has no per-server account; this must not be an error.
	if err := GrantExclusiveWrite(dir, ""); err != nil {
		t.Fatalf("empty account should be a no-op, got %v", err)
	}

	if readDACL(t, dir) != before {
		t.Error("an empty account should not have changed the DACL")
	}
}

func TestDenyAllProtectsDaemonFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "server-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := DenyAll(root); err != nil {
		t.Fatalf("DenyAll: %v", err)
	}

	acl := readDACL(t, root)
	t.Logf("dacl: %s", acl)

	if !strings.Contains(acl, "D:P") {
		t.Errorf("DACL is not protected from inheritance: %s", acl)
	}

	// The daemon must retain access to its own files, or it could not write
	// worker.json for a server it just restricted.
	self, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(acl, self.String()) {
		t.Errorf("the daemon's own account is missing from the DACL: %s", acl)
	}

	// And it must still be able to write there.
	probe := filepath.Join(root, "worker.json")
	if err := os.WriteFile(probe, []byte("{}"), 0o600); err != nil {
		t.Errorf("the daemon can no longer write its own files: %v", err)
	}
}

func TestGrantExclusiveWriteRejectsUnknownAccount(t *testing.T) {
	dir := t.TempDir()
	err := GrantExclusiveWrite(dir, "winwings-definitely-not-a-real-account")
	if err == nil {
		t.Fatal("expected an error for an account that does not exist")
	}
	if !strings.Contains(err.Error(), "could not resolve account") {
		t.Errorf("error should name the unresolvable account, got: %v", err)
	}
	t.Logf("rejected as expected: %v", err)
}
