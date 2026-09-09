package winfs

import (
	"os"
	"path"
	"time"
)

// Dir is an open handle on a directory inside the sandbox.
//
// It replaces the raw directory file descriptor that upstream wings threaded
// through its openat-based *at() calls. Operations on a Dir resolve a single
// path component against an already-open directory handle, so walking a large
// tree does not re-resolve every ancestor for every entry.
type Dir struct {
	root *os.Root
	path string
	// owned reports whether Close should release root. A Dir referring to the
	// filesystem's own root borrows it and must not close it.
	owned bool
}

// noopCloser is returned alongside a borrowed Dir so callers can always defer
// the close function SafeDir hands back.
func noopCloser() {}

// SafeDir resolves name to a handle on its parent directory plus the final
// path element.
//
// It replaces upstream's SafePath, which returned (dirfd, name, closeFd, err).
// The returned closer must always be called, including on error, exactly as
// before. The returned Dir is only valid until then.
func (fs *FS) SafeDir(name string) (*Dir, string, func(), error) {
	n, err := normalize(name)
	if err != nil {
		return &Dir{root: fs.root, path: "."}, "", noopCloser, pathErr("safedir", name, err)
	}
	if n == "." {
		return &Dir{root: fs.root, path: "."}, ".", noopCloser, nil
	}

	dir, base := path.Dir(n), path.Base(n)
	if dir == "." {
		return &Dir{root: fs.root, path: "."}, base, noopCloser, nil
	}

	sub, err := fs.root.OpenRoot(dir)
	if err != nil {
		return &Dir{root: fs.root, path: "."}, base, noopCloser, convertError(err)
	}
	return &Dir{root: sub, path: dir, owned: true}, base, func() { _ = sub.Close() }, nil
}

// Dir returns a handle on the named directory relative to the filesystem root.
// The caller owns the returned handle and must close it.
func (fs *FS) Dir(name string) (*Dir, error) {
	n, err := normalize(name)
	if err != nil {
		return nil, pathErr("dir", name, err)
	}
	if n == "." {
		return &Dir{root: fs.root, path: "."}, nil
	}
	sub, err := fs.root.OpenRoot(n)
	if err != nil {
		return nil, convertError(err)
	}
	return &Dir{root: sub, path: n, owned: true}, nil
}

// Path returns this directory's path relative to the filesystem root.
func (d *Dir) Path() string { return d.path }

// Close releases the handle, unless this Dir borrows the filesystem's root.
func (d *Dir) Close() error {
	if !d.owned {
		return nil
	}
	return d.root.Close()
}

// Sub opens a subdirectory of d. The caller owns the result and must close it.
func (d *Dir) Sub(name string) (*Dir, error) {
	sub, err := d.root.OpenRoot(name)
	if err != nil {
		return nil, convertError(err)
	}
	return &Dir{root: sub, path: path.Join(d.path, name), owned: true}, nil
}

// Open opens a file within this directory for reading.
func (d *Dir) Open(name string) (File, error) {
	f, err := d.root.Open(name)
	return f, convertError(err)
}

// OpenFile is the generalized open call, relative to this directory.
func (d *Dir) OpenFile(name string, flag int, mode FileMode) (File, error) {
	f, err := d.root.OpenFile(name, flag, mode)
	return f, convertError(err)
}

// Create creates or truncates a file within this directory.
func (d *Dir) Create(name string) (File, error) {
	f, err := d.root.Create(name)
	return f, convertError(err)
}

// Stat describes a file within this directory, following symlinks.
func (d *Dir) Stat(name string) (FileInfo, error) {
	fi, err := d.root.Stat(name)
	return fi, convertError(err)
}

// Lstat describes a file within this directory without following symlinks.
func (d *Dir) Lstat(name string) (FileInfo, error) {
	fi, err := d.root.Lstat(name)
	return fi, convertError(err)
}

// Mkdir creates a directory within this directory.
func (d *Dir) Mkdir(name string, mode FileMode) error {
	return convertError(d.root.Mkdir(name, mode))
}

// Remove removes a file or empty directory within this directory.
func (d *Dir) Remove(name string) error {
	return convertError(d.root.Remove(name))
}

// RemoveAll removes a path within this directory and everything under it.
func (d *Dir) RemoveAll(name string) error {
	return convertError(d.root.RemoveAll(name))
}

// Chmod changes the mode of a file within this directory.
func (d *Dir) Chmod(name string, mode FileMode) error {
	return convertError(d.root.Chmod(name, mode))
}

// Chtimes changes the timestamps of a file within this directory.
func (d *Dir) Chtimes(name string, atime, mtime time.Time) error {
	return convertError(d.root.Chtimes(name, atime, mtime))
}

// ReadDir reads this directory's entries, sorted by filename.
func (d *Dir) ReadDir() ([]DirEntry, error) {
	f, err := d.root.Open(".")
	if err != nil {
		return nil, convertError(err)
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, convertError(err)
	}
	return entries, nil
}
