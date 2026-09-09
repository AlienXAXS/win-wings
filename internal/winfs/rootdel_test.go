//go:build windows

package winfs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenRootBlocksParentRemoval reproduces a server that could not be deleted.
//
// Windows refuses to remove a directory while any handle to it is open, and the
// sandbox holds one on the server's data directory for as long as the server
// exists. Destroying a server removed its tree without closing that handle
// first, so the delete failed with a sharing violation on exactly the data
// directory -- and nothing appeared to be holding it, because the process
// holding it was the daemon doing the deleting.
func TestOpenRootBlocksParentRemoval(t *testing.T) {
	base := t.TempDir()
	serverRoot := filepath.Join(base, "1920b9ff")
	data := filepath.Join(serverRoot, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}

	fs, err := New(data, 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(serverRoot); err == nil {
		t.Fatal("the tree was removed with the sandbox handle still open; if Windows now " +
			"permits this, the close-before-remove ordering in Destroy is no longer load-bearing")
	} else {
		t.Logf("removal blocked as expected: %v", err)
	}

	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := os.RemoveAll(serverRoot); err != nil {
		t.Fatalf("the tree still could not be removed after closing the sandbox: %v", err)
	}
}
