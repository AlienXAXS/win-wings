package config

import (
	"strings"
	"testing"
)

// TestDefaultPathsAreWellFormed guards the default path values.
//
// Struct tags are unquoted with Go escape rules, so a Windows path written with
// single backslashes contains invalid escapes (\P, \W), the tag fails to parse,
// and the default silently becomes the empty string. A daemon booting with an
// empty data directory would create servers at the filesystem root.
//
// The failure is silent in both directions — too few backslashes empties the
// value, too many produce doubled separators that Windows tolerates but that
// corrupt every log line and path comparison. Hence a test.
func TestDefaultPathsAreWellFormed(t *testing.T) {
	c, err := NewAtPath("ignored.yml")
	if err != nil {
		t.Fatalf("building a default configuration: %v", err)
	}

	paths := map[string]string{
		"system.root_directory":    c.System.RootDirectory,
		"system.log_directory":     c.System.LogDirectory,
		"system.data":              c.System.Data,
		"system.archive_directory": c.System.ArchiveDirectory,
		"system.backup_directory":  c.System.BackupDirectory,
		"system.tmp_directory":     c.System.TmpDirectory,
	}

	for name, v := range paths {
		if v == "" {
			t.Errorf("%s has an empty default: its struct tag most likely uses single "+
				"backslashes, which are invalid escapes and make the tag unparseable", name)
			continue
		}
		if strings.Contains(v, `\\`) {
			t.Errorf("%s = %q contains a doubled separator; the struct tag has too many "+
				"backslashes", name, v)
		}
		if !strings.HasPrefix(v, `C:\`) {
			t.Errorf("%s = %q does not look like an absolute Windows path", name, v)
		}
	}
}

// TestServerPathLayout pins the per-server directory layout.
//
// The nesting is load-bearing: the sandbox is rooted at the data subdirectory so
// that a server cannot reach its own worker.json, which holds the startup
// command it would otherwise be able to rewrite.
func TestServerPathLayout(t *testing.T) {
	sc := SystemConfiguration{Data: `D:\servers`}
	const uuid = "abcd-1234"

	cases := map[string]struct{ got, want string }{
		"root":    {sc.ServerRoot(uuid), `D:\servers\abcd-1234`},
		"data":    {sc.ServerData(uuid), `D:\servers\abcd-1234\data`},
		"runtime": {sc.ServerRuntime(uuid), `D:\servers\abcd-1234\runtime`},
		"worker":  {sc.ServerWorkerConfig(uuid), `D:\servers\abcd-1234\worker.json`},
		"console": {sc.ServerConsoleLog(uuid), `D:\servers\abcd-1234\console.log`},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}

	// The daemon-owned files must sit outside the sandbox, or a server could
	// rewrite what its worker executes.
	data := sc.ServerData(uuid)
	for _, p := range []string{sc.ServerWorkerConfig(uuid), sc.ServerConsoleLog(uuid), sc.ServerRuntime(uuid)} {
		if strings.HasPrefix(p, data+`\`) {
			t.Errorf("%q is inside the server-writable directory %q", p, data)
		}
	}
}
