//go:build windows

package winproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The case from the StarRupture egg: a path relative to the server's own files.
//
// CreateProcess resolves a relative lpApplicationName against the calling
// process's current directory, which for a worker is the instance directory --
// one level above the server's data. So this has to be resolved here, against
// the directory the egg actually means.
func TestRelativePathResolvesAgainstTheServerDirectory(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	want := touch(t, filepath.Join(data, "StarRupture", "Binaries", "Win64", "Server.exe"))

	// The trap: the same relative path under the instance directory, where the
	// worker's own current directory would have sent the lookup. It must not win,
	// and its absence must not cause a failure either.
	touch(t, filepath.Join(root, "decoy.txt"))

	got, err := ResolveExecutable(`StarRupture\Binaries\Win64\Server.exe`, data, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.EqualFold(got, want) {
		t.Fatalf("resolved to %s, want %s", got, want)
	}
}

func TestForwardSlashesResolveToo(t *testing.T) {
	data := t.TempDir()
	want := touch(t, filepath.Join(data, "bin", "server.exe"))

	got, err := ResolveExecutable("bin/server.exe", data, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.EqualFold(got, want) {
		t.Fatalf("resolved to %s, want %s", got, want)
	}
}

func TestAbsolutePathIsUsedAsGiven(t *testing.T) {
	dir := t.TempDir()
	want := touch(t, filepath.Join(dir, "thing.exe"))

	got, err := ResolveExecutable(want, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.EqualFold(got, want) {
		t.Fatalf("resolved to %s, want %s", got, want)
	}
}

// A bare name gets no PATH search from CreateProcess when lpApplicationName is
// given, so it has to be searched here -- against the PATH the child is being
// handed, not the worker's own.
func TestBareNameSearchesTheChildsPath(t *testing.T) {
	data := t.TempDir()
	other := t.TempDir()
	want := touch(t, filepath.Join(other, "java.exe"))

	env := []string{"Path=" + other, "PATHEXT=.COM;.EXE;.BAT"}
	got, err := ResolveExecutable("java", data, env)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.EqualFold(got, want) {
		t.Fatalf("resolved to %s, want %s", got, want)
	}
}

// The server's own directory is searched before the PATH, so an egg shipping its
// own build of a runtime gets that one.
func TestServerDirectoryBeatsThePath(t *testing.T) {
	data := t.TempDir()
	other := t.TempDir()
	want := touch(t, filepath.Join(data, "java.exe"))
	touch(t, filepath.Join(other, "java.exe"))

	got, err := ResolveExecutable("java", data, []string{"PATH=" + other})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.EqualFold(got, want) {
		t.Fatalf("resolved to %s, want %s", got, want)
	}
}

func TestMissingExecutableSaysWhereItLooked(t *testing.T) {
	data := t.TempDir()

	_, err := ResolveExecutable(`bin\missing.exe`, data, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), data) {
		t.Fatalf("error does not say where it looked: %v", err)
	}
}
