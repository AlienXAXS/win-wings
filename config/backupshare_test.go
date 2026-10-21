package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupShareCredentialsInline(t *testing.T) {
	c := BackupShareConfiguration{Username: "nasuser", Password: "pw"}
	u, p, err := c.Credentials()
	if err != nil || u != "nasuser" || p != "pw" {
		t.Fatalf("Credentials() = %q, %q, %v", u, p, err)
	}
}

func TestBackupShareCredentialsNoneUsesDaemonAccount(t *testing.T) {
	u, p, err := BackupShareConfiguration{}.Credentials()
	if err != nil || u != "" || p != "" {
		t.Fatalf("Credentials() = %q, %q, %v; want empty so the daemon's own account is used", u, p, err)
	}
}

func TestBackupShareCredentialsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "backup.cred")
	if err := os.WriteFile(f, []byte("username: NAS\\nasuser\npassword: 'p: w#d'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u, p, err := BackupShareConfiguration{CredentialsFile: f}.Credentials()
	if err != nil || u != `NAS\nasuser` || p != "p: w#d" {
		t.Fatalf("Credentials() = %q, %q, %v", u, p, err)
	}
}

func TestBackupShareCredentialsFileErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := map[string]struct {
		cfg  BackupShareConfiguration
		want string
	}{
		"both file and inline": {
			BackupShareConfiguration{CredentialsFile: write("a", "username: x\n"), Username: "y"},
			"one or the other",
		},
		"missing file": {
			BackupShareConfiguration{CredentialsFile: filepath.Join(dir, "nope")},
			"credentials",
		},
		"no username": {
			BackupShareConfiguration{CredentialsFile: write("b", "password: x\n")},
			"no username",
		},
		// A typo such as "user:" would otherwise connect with no username,
		// which silently means the daemon's own account.
		"unknown key": {
			BackupShareConfiguration{CredentialsFile: write("c", "user: x\npassword: y\n")},
			"user",
		},
	}
	for name, c := range cases {
		if _, _, err := c.cfg.Credentials(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v; want one mentioning %q", name, err, c.want)
		}
	}
}

func TestEnsureBackupDirectoryLocal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	sc := &SystemConfiguration{
		BackupDirectory: dir,
		// Must be ignored for a local path, not rejected.
		BackupShare: BackupShareConfiguration{CredentialsFile: filepath.Join(dir, "missing")},
	}
	if sc.BackupDirectoryIsShare() {
		t.Fatalf("%s reported as a share", dir)
	}
	if err := sc.EnsureBackupDirectory(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("backup directory was not created: %v", err)
	}
}
