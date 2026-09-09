//go:build windows

package winfs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func newFS(t *testing.T) (*FS, string, string) {
	t.Helper()

	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	outDir := filepath.Join(base, "outside")
	for _, d := range []string{rootDir, outDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outDir, "secret.txt"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}

	fs, err := New(rootDir, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs, rootDir, outDir
}

// The wrapper rewrites paths before handing them to os.Root — stripping leading
// separators and normalising backslashes. That is the one place this package
// touches a path, so it is the one place it could reintroduce an escape.
func TestFSContainmentThroughWrapper(t *testing.T) {
	fs, _, outDir := newFS(t)

	for _, p := range []string{
		`../outside/secret.txt`,
		`/../outside/secret.txt`,
		`//../outside/secret.txt`,
		`\..\outside\secret.txt`,
		`/a/../../outside/secret.txt`,
		`./../outside/secret.txt`,
		filepath.Join(outDir, "secret.txt"),
		`C:\Windows\win.ini`,
		`\\?\C:\Windows\win.ini`,
		`\\localhost\C$\Windows\win.ini`,
	} {
		f, err := fs.Open(p)
		if err == nil {
			b := make([]byte, len(secret))
			n, _ := f.Read(b)
			_ = f.Close()
			if strings.Contains(string(b[:n]), secret) {
				t.Errorf("ESCAPED %q read the secret through FS.Open", p)
			} else {
				t.Errorf("ESCAPED %q opened a path outside the root", p)
			}
			continue
		}
		if !errors.Is(err, ErrBadPathResolution) {
			t.Errorf("%q blocked but not as ErrBadPathResolution: %v", p, err)
			continue
		}
		t.Logf("blocked %-40q %v", p, err)
	}
}

// SafeDir splits a path into a parent handle and a leaf name. A traversal in
// the directory part must fail there, not silently resolve.
func TestSafeDirContainment(t *testing.T) {
	fs, _, _ := newFS(t)

	if err := fs.MkdirAll("a/b", 0o755); err != nil {
		t.Fatal(err)
	}

	d, name, closeFn, err := fs.SafeDir("a/b/file.txt")
	defer closeFn()
	if err != nil {
		t.Fatalf("SafeDir on a valid path: %v", err)
	}
	if name != "file.txt" {
		t.Errorf("leaf name = %q, want %q", name, "file.txt")
	}
	if d.Path() != "a/b" {
		t.Errorf("parent path = %q, want %q", d.Path(), "a/b")
	}

	d2, _, close2, err := fs.SafeDir("../outside/secret.txt")
	defer close2()
	if err == nil {
		t.Error("SafeDir resolved a path whose parent escapes the root")
	} else if !errors.Is(err, ErrBadPathResolution) {
		t.Errorf("SafeDir escape not reported as ErrBadPathResolution: %v", err)
	}
	_ = d2
}

func TestSafeDirRootRelative(t *testing.T) {
	fs, _, _ := newFS(t)

	for _, in := range []string{"file.txt", "/file.txt"} {
		d, name, closeFn, err := fs.SafeDir(in)
		if err != nil {
			t.Fatalf("SafeDir(%q): %v", in, err)
		}
		if name != "file.txt" || d.Path() != "." {
			t.Errorf("SafeDir(%q) = (%q, %q), want (\".\", \"file.txt\")", in, d.Path(), name)
		}
		closeFn()
	}
}

func TestTouchCreatesParents(t *testing.T) {
	fs, rootDir, _ := newFS(t)

	f, err := fs.Touch("deeply/nested/dir/file.txt", O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f.Close()

	if _, err := os.Stat(filepath.Join(rootDir, "deeply", "nested", "dir", "file.txt")); err != nil {
		t.Fatalf("expected the file to exist on disk: %v", err)
	}
}

func TestWalkDir(t *testing.T) {
	fs, _, _ := newFS(t)

	for _, d := range []string{"a", "a/b", "c"} {
		if err := fs.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"top.txt", "a/one.txt", "a/b/two.txt", "c/three.txt"} {
		wf, err := fs.Create(f)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = wf.Write([]byte("x"))
		_ = wf.Close()
	}

	var seen []string
	err := fs.WalkDir(".", func(dir *Dir, name, relative string, entry DirEntry, err error) error {
		if err != nil {
			t.Errorf("walk error at %q: %v", relative, err)
			return nil
		}
		seen = append(seen, relative)

		// The handle must be usable for a single-component stat, which is the
		// whole reason the walk carries one.
		if !entry.IsDir() {
			if _, err := dir.Lstat(name); err != nil {
				t.Errorf("Lstat(%q) via walk handle: %v", name, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	sort.Strings(seen)
	want := []string{"a", "a/b", "a/b/two.txt", "a/one.txt", "c", "c/three.txt", "top.txt"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("walk visited\n  %v\nwant\n  %v", seen, want)
	}
}

func TestWalkDirSkipDir(t *testing.T) {
	fs, _, _ := newFS(t)

	if err := fs.MkdirAll("skipme/child", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := fs.Create("skipme/child/deep.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	var seen []string
	err = fs.WalkDir(".", func(_ *Dir, name, relative string, _ DirEntry, err error) error {
		if err != nil {
			return nil
		}
		seen = append(seen, relative)
		if name == "skipme" {
			return SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	for _, s := range seen {
		if strings.HasPrefix(s, "skipme/") {
			t.Errorf("SkipDir did not prevent descent: visited %q", s)
		}
	}
}

func TestQuotaSentinels(t *testing.T) {
	fs, _, _ := newFS(t)

	fs.SetLimit(-1)
	if fs.CanFit(1) {
		t.Error("limit -1 must refuse all writes")
	}

	fs.SetLimit(0)
	if !fs.CanFit(1 << 40) {
		t.Error("limit 0 means unlimited")
	}

	fs.SetLimit(1000)
	fs.SetUsage(-1)
	if !fs.CanFit(1) {
		t.Error("usage -1 (not yet calculated) must not block writes")
	}

	fs.SetUsage(400)
	if !fs.CanFit(600) {
		t.Error("a write that exactly fills the limit must be allowed")
	}
	if fs.CanFit(601) {
		t.Error("a write that exceeds the limit must be refused")
	}

	fs.SetUsage(1000)
	if fs.CanFit(1) {
		t.Error("a full filesystem must refuse further writes")
	}
}

func TestQuotaAddSaturates(t *testing.T) {
	fs, _, _ := newFS(t)

	fs.SetUsage(10)
	if got := fs.Add(-100); got != 0 {
		t.Errorf("Add below zero = %d, want 0", got)
	}

	const maxInt64 = 1<<63 - 1
	fs.SetUsage(maxInt64 - 5)
	if got := fs.Add(100); got != maxInt64 {
		t.Errorf("Add past MaxInt64 = %d, want %d", got, int64(maxInt64))
	}
}

func TestRemoveAccountsUsage(t *testing.T) {
	fs, _, _ := newFS(t)

	f, err := fs.Create("sized.bin")
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4096)
	if _, err := f.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	fs.SetUsage(4096)
	if err := fs.Remove("sized.bin"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := fs.Usage(); got != 0 {
		t.Errorf("usage after removing a 4096-byte file = %d, want 0", got)
	}
}

func TestRemoveAllAccountsTree(t *testing.T) {
	fs, _, _ := newFS(t)

	if err := fs.MkdirAll("tree/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tree/a.bin", "tree/sub/b.bin"} {
		f, err := fs.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(make([]byte, 1000)); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}

	fs.SetUsage(2000)
	if err := fs.RemoveAll("tree"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if got := fs.Usage(); got != 0 {
		t.Errorf("usage after removing a 2000-byte tree = %d, want 0", got)
	}
}

func TestRemoveAllRefusesRoot(t *testing.T) {
	fs, _, _ := newFS(t)

	for _, p := range []string{"", "/", ".", "//"} {
		if err := fs.RemoveAll(p); !errors.Is(err, ErrBadPathResolution) {
			t.Errorf("RemoveAll(%q) = %v, want ErrBadPathResolution", p, err)
		}
	}
}
