//go:build windows

package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRollGivesEachRunItsOwnLog covers the reason the console log is rolled at
// all: reading back a run should not mean scrolling past every previous one.
func TestRollGivesEachRunItsOwnLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.log")

	c := newConsoleBuffer(path, 16, 5, 3)
	defer c.Close()

	c.Roll() // First start: nothing to keep.
	c.Append([]byte("first run\n"))

	c.Roll()
	c.Append([]byte("second run\n"))

	c.Roll()
	c.Append([]byte("third run\n"))

	if got := read(t, path); !strings.Contains(got, "third run") || strings.Contains(got, "second run") {
		t.Errorf("console.log holds %q, want only the current run", got)
	}
	if got := read(t, path+".1"); !strings.Contains(got, "second run") {
		t.Errorf("console.log.1 holds %q, want the previous run", got)
	}
	if got := read(t, path+".2"); !strings.Contains(got, "first run") {
		t.Errorf("console.log.2 holds %q, want the run before that", got)
	}
}

// TestRollDoesNotBurnAGenerationOnAnEmptyLog matters because a worker that is
// restarted, or a start that fails before the process prints anything, would
// otherwise push the real output out of the retained set.
func TestRollDoesNotBurnAGenerationOnAnEmptyLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.log")

	c := newConsoleBuffer(path, 16, 5, 3)
	c.Append([]byte("output worth keeping\n"))
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh worker for the same server: the file exists on disk but this
	// buffer has never written to it.
	c2 := newConsoleBuffer(path, 16, 5, 3)
	defer c2.Close()
	c2.Roll()
	c2.Roll()
	c2.Roll()

	if got := read(t, path+".1"); !strings.Contains(got, "output worth keeping") {
		t.Errorf("console.log.1 holds %q, want the previous run's output", got)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Error("an empty log should not have been rotated into a generation of its own")
	}
}

// TestRollRetainsNothingWhenRetentionIsOff confirms the setting is honoured;
// an operator who has turned history off should not accumulate files.
func TestRollRetainsNothingWhenRetentionIsOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "console.log")

	c := newConsoleBuffer(path, 16, 5, 0)
	defer c.Close()

	c.Append([]byte("first run\n"))
	c.Roll()
	c.Append([]byte("second run\n"))

	if got := read(t, path); strings.Contains(got, "first run") {
		t.Errorf("console.log holds %q, want only the current run", got)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("history was retained even though max_files is zero")
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Base(path), err)
	}
	return string(b)
}
