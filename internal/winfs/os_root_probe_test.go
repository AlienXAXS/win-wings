//go:build windows

package winfs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is a PROBE, not a unit test of our own code. It establishes what
// os.Root actually guarantees on Windows so we know what winfs must add on top.
//
// Two classes of assertion:
//
//   - mustNotEscape / mustNotOpen  — a failure here is a real sandbox escape and
//     fails the test. These encode guarantees we intend to rely on.
//   - probe                        — records observed behaviour without failing.
//     These are naming hazards (aliasing, device names) that are not escapes but
//     that any string-based denylist or path comparison in winfs must handle.
//
// Run with -v to read the observed behaviour:
//
//	go test ./internal/winfs -run TestOSRoot -v

const secret = "SECRET-CONTENT-DO-NOT-LEAK"

type env struct {
	t       *testing.T
	rootDir string
	outDir  string
	root    *os.Root
}

func newEnv(t *testing.T) *env {
	t.Helper()

	base := t.TempDir()
	rootDir := filepath.Join(base, "root")
	outDir := filepath.Join(base, "outside")

	for _, d := range []string{rootDir, outDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(outDir, "secret.txt"), []byte(secret), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	// A benign file inside the root, for aliasing probes.
	if err := os.WriteFile(filepath.Join(rootDir, "inside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatalf("write inside: %v", err)
	}

	r, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", rootDir, err)
	}
	t.Cleanup(func() { _ = r.Close() })

	return &env{t: t, rootDir: rootDir, outDir: outDir, root: r}
}

// mustNotEscape fails if reading name through the root yields the secret file.
func (e *env) mustNotEscape(name string) {
	e.t.Helper()
	b, err := e.root.ReadFile(name)
	switch {
	case err != nil:
		e.t.Logf("  blocked  %-44q %v", name, rootErr(err))
	case strings.Contains(string(b), secret):
		e.t.Errorf("  ESCAPED  %-44q read the secret through the root", name)
	default:
		e.t.Logf("  opened   %-44q %d bytes, not the secret", name, len(b))
	}
}

// mustNotOpen fails if name opens at all. Used for paths that address something
// wholly outside the root, where any success is an escape.
func (e *env) mustNotOpen(name string) {
	e.t.Helper()
	f, err := e.root.Open(name)
	if err != nil {
		e.t.Logf("  blocked  %-44q %v", name, rootErr(err))
		return
	}
	_ = f.Close()
	e.t.Errorf("  ESCAPED  %-44q opened a path outside the root", name)
}

// probe records behaviour without asserting. Returns whether the open succeeded.
func (e *env) probe(label, name string) bool {
	e.t.Helper()
	f, err := e.root.Open(name)
	if err != nil {
		e.t.Logf("  n/a      %-16s %-28q %v", label, name, rootErr(err))
		return false
	}
	_ = f.Close()
	e.t.Logf("  RESOLVED %-16s %-28q opened successfully", label, name)
	return true
}

// rootErr unwraps to the most informative part of an os.Root error.
func rootErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// --- Classic traversal -------------------------------------------------------

func TestOSRootTraversal(t *testing.T) {
	e := newEnv(t)

	t.Log("relative traversal:")
	for _, p := range []string{
		`../outside/secret.txt`,
		`..\outside\secret.txt`,
		`./../outside/secret.txt`,
		`sub/../../outside/secret.txt`,
		`../../../../../../../../Windows/win.ini`,
	} {
		e.mustNotEscape(p)
	}

	t.Log("absolute and drive-relative:")
	e.mustNotEscape(filepath.Join(e.outDir, "secret.txt"))
	e.mustNotOpen(`C:\Windows\win.ini`)
	e.mustNotOpen(`C:Windows\win.ini`) // drive-relative, resolves against CWD of C:
	e.mustNotOpen(`\Windows\win.ini`)  // rooted, no drive
}

func TestOSRootDeviceNamespace(t *testing.T) {
	e := newEnv(t)

	t.Log("device namespace prefixes bypass Win32 path normalisation:")
	e.mustNotOpen(`\\?\C:\Windows\win.ini`)
	e.mustNotOpen(`\\.\C:\Windows\win.ini`)
	e.mustNotOpen(`\\?\GLOBALROOT\Device\HarddiskVolume1\Windows\win.ini`)
	e.mustNotEscape(`\\?\` + filepath.Join(e.outDir, "secret.txt"))

	t.Log("UNC:")
	e.mustNotOpen(`\\localhost\C$\Windows\win.ini`)
	e.mustNotOpen(`\\?\UNC\localhost\C$\Windows\win.ini`)
}

// --- Reparse points ----------------------------------------------------------

// Junctions are the important case: unlike symlinks they need no admin rights
// and no Developer Mode, so any user with file-manager or SFTP access can make
// one.
func TestOSRootJunctionEscape(t *testing.T) {
	e := newEnv(t)

	link := filepath.Join(e.rootDir, "jn")
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, e.outDir).CombinedOutput()
	if err != nil {
		t.Skipf("could not create junction (%v): %s", err, strings.TrimSpace(string(out)))
	}
	t.Logf("created junction %s -> %s", link, e.outDir)

	e.mustNotEscape(`jn/secret.txt`)
	e.mustNotEscape(`jn\secret.txt`)
	e.mustNotOpen(`jn`)

	t.Log("nested root through a junction (the dirfd replacement path):")
	if sub, err := e.root.OpenRoot("jn"); err != nil {
		t.Logf("  blocked  OpenRoot(\"jn\") %v", rootErr(err))
	} else {
		defer sub.Close()
		if b, err := sub.ReadFile("secret.txt"); err == nil && strings.Contains(string(b), secret) {
			t.Errorf("  ESCAPED  OpenRoot(\"jn\") then ReadFile reached the secret")
		} else {
			t.Logf("  blocked  OpenRoot(\"jn\") succeeded but read failed: %v", err)
		}
	}
}

func TestOSRootSymlinkEscape(t *testing.T) {
	e := newEnv(t)

	link := filepath.Join(e.rootDir, "sl")
	if err := os.Symlink(e.outDir, link); err != nil {
		t.Skipf("cannot create symlinks here (needs admin or Developer Mode): %v", err)
	}
	t.Logf("created symlink %s -> %s", link, e.outDir)

	e.mustNotEscape(`sl/secret.txt`)
	e.mustNotEscape(`sl\secret.txt`)
}

// Creating a link that points outside should either fail, or be inert on
// traversal. Either is acceptable; silently working is not.
func TestOSRootSymlinkCreationEscape(t *testing.T) {
	e := newEnv(t)

	if err := e.root.Symlink(e.outDir, "made"); err != nil {
		t.Logf("  blocked  root.Symlink to an outside target: %v", rootErr(err))
	} else {
		t.Logf("  created  root.Symlink to an outside target succeeded; checking traversal")
		e.mustNotEscape(`made/secret.txt`)
	}

	if err := e.root.Link(filepath.Join(e.outDir, "secret.txt"), "hard.txt"); err != nil {
		t.Logf("  blocked  root.Link to an outside target: %v", rootErr(err))
	} else {
		e.mustNotEscape(`hard.txt`)
	}
}

func TestOSRootRenameEscape(t *testing.T) {
	e := newEnv(t)

	if err := e.root.Rename("inside.txt", `../outside/stolen.txt`); err != nil {
		t.Logf("  blocked  Rename out of the root: %v", rootErr(err))
	} else {
		t.Errorf("  ESCAPED  Rename moved a file outside the root")
	}

	if err := e.root.Rename("inside.txt", filepath.Join(e.outDir, "stolen.txt")); err != nil {
		t.Logf("  blocked  Rename to an absolute outside path: %v", rootErr(err))
	} else {
		t.Errorf("  ESCAPED  Rename moved a file outside the root via absolute path")
	}
}

// --- Naming hazards ----------------------------------------------------------
//
// None of the following are escapes. All of them mean one file has several
// valid names, which breaks any denylist or path comparison done on strings.
// Egg file denylists (server.Configuration.Egg.FileDenylist) are exactly that.

func TestOSRootNamingHazards(t *testing.T) {
	e := newEnv(t)

	t.Log("case insensitivity:")
	e.probe("exact", "inside.txt")
	e.probe("upper", "INSIDE.TXT")
	e.probe("mixed", "InSiDe.TxT")

	t.Log("trailing dots and spaces (stripped by the Win32 layer):")
	e.probe("trailing dot", "inside.txt.")
	e.probe("trailing dots", "inside.txt...")
	e.probe("trailing space", "inside.txt ")
	e.probe("dot+space", "inside.txt . ")

	t.Log("alternate data streams:")
	e.probe("default stream", `inside.txt::$DATA`)
	e.probe("named stream", `inside.txt:alt`)
	e.probe("named +type", `inside.txt:alt:$DATA`)

	t.Log("separators:")
	e.probe("forward", "inside.txt")
	e.probe("leading ./", "./inside.txt")
	e.probe("doubled sep", ".//inside.txt")
	e.probe("backslash sep", `.\inside.txt`)
}

// 8.3 short names alias long names on volumes where generation is enabled.
// If this resolves, a denylist entry for "config.yml" is bypassable as
// "CONFIG~1.YML".
func TestOSRootShortNameAliasing(t *testing.T) {
	e := newEnv(t)

	long := "VeryLongConfigurationFileName.yml"
	if err := os.WriteFile(filepath.Join(e.rootDir, long), []byte("inside"), 0o644); err != nil {
		t.Fatalf("write long name: %v", err)
	}

	if !e.probe("long name", long) {
		t.Fatal("long name did not open; probe is invalid")
	}
	for _, short := range []string{"VERYLO~1.YML", "veryLO~1.yml"} {
		if e.probe("short name", short) {
			t.Logf("  NOTE: 8.3 generation is ENABLED on this volume — string denylists are bypassable")
		}
	}
}

// Reserved device names are reserved in EVERY directory. Opening one does not
// escape the root, but it addresses a device rather than a file: writing to NUL
// silently discards, and CON/COM1 can block indefinitely.
func TestOSRootReservedDeviceNames(t *testing.T) {
	e := newEnv(t)

	if err := e.root.Mkdir("sub", 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	for _, name := range []string{"NUL", "CON", "AUX", "PRN", "COM1", "LPT1", "nul", "NUL.txt", `sub\NUL`} {
		f, err := e.root.OpenFile(name, os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			t.Logf("  n/a      device %-14q %v", name, rootErr(err))
			continue
		}
		n, werr := f.Write([]byte("probe"))
		_ = f.Close()

		st, serr := os.Stat(filepath.Join(e.rootDir, name))
		if serr != nil {
			t.Errorf("  DEVICE   %-14q wrote %d bytes (err=%v) but no file exists on disk"+
				" — this addressed a device, not a file", name, n, werr)
			continue
		}
		t.Logf("  RESOLVED %-14q real file on disk, size=%d — literal name, not the device",
			name, st.Size())
	}
}

// The nested-root pattern is our replacement for the dirfd family in
// internal/ufs. Confirm it re-anchors rather than inheriting the parent's reach.
func TestOSRootNestedRootConfinement(t *testing.T) {
	e := newEnv(t)

	if err := e.root.MkdirAll(`a/b`, 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := e.root.WriteFile(`a/b/deep.txt`, []byte("deep"), 0o644); err != nil {
		t.Fatalf("write deep: %v", err)
	}

	sub, err := e.root.OpenRoot("a")
	if err != nil {
		t.Fatalf("OpenRoot(a): %v", err)
	}
	defer sub.Close()

	if _, err := sub.ReadFile(`b/deep.txt`); err != nil {
		t.Errorf("nested root could not read its own content: %v", err)
	} else {
		t.Logf("  ok       nested root reads its own content")
	}

	// A nested root must not be able to walk back up into its parent.
	if _, err := sub.ReadFile(`../inside.txt`); err != nil {
		t.Logf("  blocked  nested root ../ into parent: %v", rootErr(err))
	} else {
		t.Errorf("  ESCAPED  nested root read a file from its parent via ../")
	}

	if b, err := sub.ReadFile(`../../outside/secret.txt`); err == nil && strings.Contains(string(b), secret) {
		t.Errorf("  ESCAPED  nested root reached the secret via ../../")
	} else {
		t.Logf("  blocked  nested root ../../ out of the tree: %v", err)
	}
}
