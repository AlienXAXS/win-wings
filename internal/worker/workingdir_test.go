//go:build windows

package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pterodactyl/wings/internal/wire"
)

// workingDirWorker builds a worker rooted at a real data directory, which is
// what serverWorkingDir resolves against and stats.
func workingDirWorker(t *testing.T) *Worker {
	t.Helper()

	return &Worker{cfg: Config{UUID: "working-dir-test", WorkingDir: t.TempDir()}}
}

func TestServerWorkingDirDefaultsToTheDataDirectory(t *testing.T) {
	// The ordinary egg sets nothing, and must keep starting exactly where it
	// always did.
	w := workingDirWorker(t)

	got, err := w.serverWorkingDir(wire.Start{})
	if err != nil {
		t.Fatalf("serverWorkingDir: %v", err)
	}
	if got != w.cfg.WorkingDir {
		t.Errorf("working dir = %q, want %q", got, w.cfg.WorkingDir)
	}
}

func TestServerWorkingDirResolvesInsideTheDataDirectory(t *testing.T) {
	w := workingDirWorker(t)

	want := filepath.Join(w.cfg.WorkingDir, "ServerFile", "Binaries")
	if err := os.MkdirAll(want, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := w.serverWorkingDir(wire.Start{WorkingDir: `ServerFile\Binaries`})
	if err != nil {
		t.Fatalf("serverWorkingDir: %v", err)
	}
	if got != want {
		t.Errorf("working dir = %q, want %q", got, want)
	}
}

func TestServerWorkingDirRefusesPathsThatLeaveTheDataDirectory(t *testing.T) {
	// The daemon checks this too. It is checked again here because the worker is
	// what launches the process: a value that escaped would start a game
	// anywhere on the host the daemon's account can reach.
	w := workingDirWorker(t)

	for _, rel := range []string{
		`..`,
		`..\..\windows`,
		`ServerFile\..\..\elsewhere`,
		`C:\Games\Server`,
		`\\host\share`,
		`\absolute`,
		`/absolute`,
		`C:relative`,
	} {
		if got, err := w.serverWorkingDir(wire.Start{WorkingDir: rel}); err == nil {
			t.Errorf("working dir %q resolved to %q, want an error", rel, got)
		}
	}
}

func TestServerWorkingDirRefusesAMissingDirectory(t *testing.T) {
	// Checked rather than left to CreateProcess, which reports a missing
	// lpCurrentDirectory as "The directory name is invalid" without saying which
	// directory it means or where the value came from.
	w := workingDirWorker(t)

	if _, err := w.serverWorkingDir(wire.Start{WorkingDir: "NotInstalledYet"}); err == nil {
		t.Fatal("a working directory that does not exist was accepted")
	}
}

func TestServerWorkingDirRefusesAFile(t *testing.T) {
	w := workingDirWorker(t)

	path := filepath.Join(w.cfg.WorkingDir, "ServerFile")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := w.serverWorkingDir(wire.Start{WorkingDir: "ServerFile"}); err == nil {
		t.Fatal("a file was accepted as a working directory")
	}
}
