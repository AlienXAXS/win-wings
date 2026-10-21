//go:build windows

package smbconn

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestShareRoot(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		isUNC bool
	}{
		{`\\nas\backups`, `\\nas\backups`, true},
		{`\\nas\backups\node1\deep`, `\\nas\backups`, true},
		{`//nas/backups/node1`, `\\nas\backups`, true},
		{`\\192.168.1.10\backups\node1`, `\\192.168.1.10\backups`, true},
		{`\\?\UNC\nas\backups\node1`, `\\nas\backups`, true},
		{`\\?\unc\nas\backups`, `\\nas\backups`, true},

		{`C:\ProgramData\WinWings\backups`, "", false},
		{`D:/backups`, "", false},
		{`backups`, "", false},
		{`\backups`, "", false},
		{`\\?\C:\backups`, "", false},
		{`\\.\PhysicalDrive0`, "", false},
		{`\\nas`, "", false},
		{`\\nas\`, "", false},
		{`\\`, "", false},
		{``, "", false},
	}
	for _, c := range cases {
		got, ok := ShareRoot(c.in)
		if ok != c.isUNC || got != c.want {
			t.Errorf("ShareRoot(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.isUNC)
		}
	}
}

// TestConnectReadWrite runs against a real share, so it is opt-in:
//
//	$env:WINWINGS_TEST_SMB_PATH = '\\nas\backups\winwings-test'
//	$env:WINWINGS_TEST_SMB_USER = 'nasuser'
//	$env:WINWINGS_TEST_SMB_PASS = '...'
//	go test ./internal/smbconn -run ConnectReadWrite -v
//
// It does not disconnect afterwards. The connection belongs to the logon
// session, not the test, and cancelling it would also drop a share the person
// running the test had mapped themselves.
func TestConnectReadWrite(t *testing.T) {
	dir := os.Getenv("WINWINGS_TEST_SMB_PATH")
	if dir == "" {
		t.Skip("WINWINGS_TEST_SMB_PATH is not set")
	}
	share, ok := ShareRoot(dir)
	if !ok {
		t.Fatalf("WINWINGS_TEST_SMB_PATH=%q is not a UNC path", dir)
	}

	err := Connect(share, os.Getenv("WINWINGS_TEST_SMB_USER"), os.Getenv("WINWINGS_TEST_SMB_PASS"))
	if errors.Is(err, ErrCredentialConflict) {
		t.Logf("%v", err)
	} else if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}

	// Large enough to span several SMB writes, so a short write or a
	// truncating server shows up as a mismatch rather than passing by luck.
	want := make([]byte, 4<<20)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, ".winwings-smbconn-test")
	t.Cleanup(func() { _ = os.Remove(probe) })

	if err := os.WriteFile(probe, want, 0o600); err != nil {
		t.Fatalf("write %s: %v", probe, err)
	}
	got, err := os.ReadFile(probe)
	if err != nil {
		t.Fatalf("read %s: %v", probe, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read back %d bytes that differ from the %d written", len(got), len(want))
	}
	if err := os.Remove(probe); err != nil {
		t.Fatalf("remove %s: %v", probe, err)
	}
}
