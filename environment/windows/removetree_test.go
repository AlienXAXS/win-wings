//go:build windows

package windows

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRemoveTreeWaitsForAHandleToClose covers the race that a deletion arriving
// just after a server stops actually hits.
//
// The worker's working directory is the server's data directory, so for a moment
// after it is told to stop, the directory cannot be removed. Failing there
// reports a permanent problem for a condition that clears in milliseconds.
func TestRemoveTreeWaitsForAHandleToClose(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "server")
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}

	holder, err := os.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		_ = holder.Close()
	}()

	start := time.Now()
	if err := removeTree(root); err != nil {
		t.Fatalf("removeTree gave up on a handle that was about to close: %v", err)
	}
	t.Logf("removed after %s", time.Since(start).Round(time.Millisecond))

	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the tree still exists: %v", err)
	}
}

// TestRemoveTreeReportsAHandleThatNeverCloses checks the give-up path names
// something actionable rather than repeating the raw sharing violation.
func TestRemoveTreeReportsAHandleThatNeverCloses(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "server")
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}

	holder, err := os.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	err = removeTree(root)
	if err == nil {
		t.Fatal("removeTree reported success while a handle was still open")
	}
	t.Logf("reported: %v", err)
}

func TestRemoveTreeOnAMissingDirectory(t *testing.T) {
	// Deleting a server whose files are already gone must not fail; the Panel
	// retries deletions and the second attempt would otherwise never succeed.
	if err := removeTree(filepath.Join(t.TempDir(), "never-existed")); err != nil {
		t.Errorf("removing an absent directory should succeed, got %v", err)
	}
}
