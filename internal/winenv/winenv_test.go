//go:build windows

package winenv

import (
	"strings"
	"testing"
)

func lookup(env []string, key string) (string, bool) {
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// TestBaseSuppliesWhatWindowsNeeds guards the variables whose absence produces
// failures that do not name themselves.
//
// SystemRoot is the sharp one: it is read during process startup to locate
// system DLLs, and Winsock initialisation fails without it. A downloader
// launched into such an environment reports that it cannot resolve any host,
// which sends whoever is debugging it to the network rather than the daemon.
func TestBaseSuppliesWhatWindowsNeeds(t *testing.T) {
	env := Base(Paths{Data: `D:\servers\abc\data`, Temp: `D:\servers\abc\tmp`})

	for _, key := range []string{
		"SystemRoot", "SystemDrive", "PATH", "PATHEXT", "ComSpec",
		"TEMP", "TMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
	} {
		v, ok := lookup(env, key)
		if !ok || v == "" {
			t.Errorf("%s is missing from the base environment", key)
		}
	}

	// PATH must reach System32, or nothing resolves by name -- including the
	// powershell.exe that runs every install script.
	path, _ := lookup(env, "PATH")
	if !strings.Contains(strings.ToLower(path), `system32`) {
		t.Errorf("PATH does not include System32: %q", path)
	}

	if v, _ := lookup(env, "TEMP"); v != `D:\servers\abc\tmp` {
		t.Errorf("TEMP = %q, want the server's own scratch directory", v)
	}
	// TEMP outside the sandbox is the point: inside, scratch files are charged
	// against the user's disk quota and copied into every backup.
	if strings.HasPrefix(`D:\servers\abc\tmp`, `D:\servers\abc\data`) {
		t.Error("the temp directory is inside the data directory")
	}
}

// TestBaseDoesNotLeakTheDaemonsEnvironment checks that only named variables
// cross over.
//
// The daemon's environment carries whatever the service account was configured
// with — proxy settings, credential helpers, tokens exported by an operator —
// and a game server running third-party code has no business seeing any of it.
func TestBaseDoesNotLeakTheDaemonsEnvironment(t *testing.T) {
	t.Setenv("WINWINGS_SECRET_PROBE", "should-not-appear")

	env := Base(Paths{Data: `D:\x\data`, Temp: `D:\x\tmp`})
	if v, ok := lookup(env, "WINWINGS_SECRET_PROBE"); ok {
		t.Errorf("an unrelated daemon variable reached the server environment: %q", v)
	}
}

func TestMergeLetsPanelVariablesWin(t *testing.T) {
	base := Base(Paths{Data: `D:\x\data`, Temp: `D:\x\tmp`})
	out := Merge(base, []string{"SERVER_MEMORY=4096", "TEMP=D:\\override"})

	if v, _ := lookup(out, "SERVER_MEMORY"); v != "4096" {
		t.Errorf("SERVER_MEMORY = %q, want 4096", v)
	}
	if v, _ := lookup(out, "TEMP"); v != `D:\override` {
		t.Errorf("TEMP = %q; an egg variable should override the base", v)
	}
}

// TestMergeIsCaseInsensitive covers the failure mode that makes this worth a
// function rather than an append.
//
// Windows environment variable names are case-insensitive, but an environment
// *block* is just a list of strings: CreateProcess accepts one containing both
// "PATH=" and "Path=", and the process then sees whichever it happens to look up
// first. An egg naming a variable in the wrong case must replace the base entry,
// not sit beside it.
func TestMergeIsCaseInsensitive(t *testing.T) {
	out := Merge([]string{"PATH=C:\\one", "TEMP=C:\\t"}, []string{"Path=C:\\two"})

	n := 0
	for _, e := range out {
		if k, _, _ := strings.Cut(e, "="); strings.EqualFold(k, "PATH") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the merged environment has %d PATH entries, want 1: %v", n, out)
	}
	if v, _ := lookup(out, "PATH"); v != `C:\two` {
		t.Errorf("PATH = %q, want the override", v)
	}
}

func TestMergeIgnoresMalformedEntries(t *testing.T) {
	out := Merge([]string{"A=1"}, []string{"no-equals-sign", "B=2"})
	if v, _ := lookup(out, "B"); v != "2" {
		t.Error("a malformed entry stopped later entries being applied")
	}
	for _, e := range out {
		if !strings.Contains(e, "=") {
			t.Errorf("a malformed entry reached the environment block: %q", e)
		}
	}
}
