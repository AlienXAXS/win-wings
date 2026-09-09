package winfs

import (
	"errors"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"
)

// FS is a sandboxed filesystem rooted at a single directory.
//
// Every operation is performed through an *os.Root, so path resolution — and
// therefore containment — is the standard library's responsibility rather than
// ours. See doc.go for why that matters.
type FS struct {
	root     *os.Root
	basePath string

	// Disk accounting. Windows has no per-directory quota primitive available
	// without administrator rights (NTFS quotas are per-user per-volume, FSRM is
	// Server-only), so usage is tracked in userspace exactly as upstream did.
	limit atomic.Int64
	usage atomic.Int64
}

// New opens a sandboxed filesystem rooted at basePath, which must already exist.
func New(basePath string, limit int64) (*FS, error) {
	root, err := os.OpenRoot(basePath)
	if err != nil {
		return nil, convertError(err)
	}

	fs := &FS{root: root, basePath: basePath}
	fs.limit.Store(limit)
	return fs, nil
}

// BasePath returns the host path this filesystem is rooted at.
func (fs *FS) BasePath() string { return fs.basePath }

// Close releases the root handle. The FS must not be used afterwards.
func (fs *FS) Close() error { return fs.root.Close() }

// Root exposes the underlying handle for callers that need to hand a sandboxed
// root to another package. It must not be closed by the caller.
func (fs *FS) Root() *os.Root { return fs.root }

// normalize converts an externally-supplied path into the relative form os.Root
// expects, rejecting anything that is not a plain root-relative path.
//
// This deliberately does not attempt to resolve "..", collapse links, or decide
// whether a path is safe — os.Root owns that. Doing our own resolution here is
// precisely the mistake behind upstream's history of escape advisories.
//
// What it must do is refuse path syntaxes that would otherwise be silently
// *mangled* into something valid. Naively stripping leading separators turns the
// UNC path \\localhost\C$\x into the relative path localhost/C$/x. That does not
// escape the sandbox, but it addresses a different file than the caller named —
// and one a denylist check against the original string would never have seen.
func normalize(name string) (string, error) {
	s := strings.ReplaceAll(name, `\`, "/")

	// A leading "//" is a UNC share (//server/share) or the Windows device
	// namespace (//?/, //./). Neither is a path within this sandbox.
	if strings.HasPrefix(s, "//") {
		return "", ErrBadPathResolution
	}

	// A colon is never valid in a Windows filename. Its presence means a drive
	// letter ("C:/x"), a drive-relative path ("C:x"), or an alternate data
	// stream ("file.txt:hidden"). os.Root rejects all three, but catching them
	// here keeps the reported error accurate rather than a syntax complaint.
	if strings.Contains(s, ":") {
		return "", ErrBadPathResolution
	}

	// A leading single separator is root-relative, which is how the rest of
	// wings addresses server files. Strip it; "/a/../../b" becomes "a/../../b",
	// which os.Root then rejects on its own merits.
	s = strings.TrimLeft(s, "/")
	if s == "" {
		return ".", nil
	}
	return s, nil
}

// pathErr builds a *PathError for a path rejected before it reached os.Root.
func pathErr(op, name string, err error) error {
	return &PathError{Op: op, Path: name, Err: err}
}

// --- Operations on the root --------------------------------------------------

// Open opens the named file for reading.
func (fs *FS) Open(name string) (File, error) {
	n, err := normalize(name)
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	f, err := fs.root.Open(n)
	return f, convertError(err)
}

// OpenFile is the generalized open call.
func (fs *FS) OpenFile(name string, flag int, mode FileMode) (File, error) {
	n, err := normalize(name)
	if err != nil {
		return nil, pathErr("openfile", name, err)
	}
	f, err := fs.root.OpenFile(n, flag, mode)
	return f, convertError(err)
}

// Create creates or truncates the named file.
func (fs *FS) Create(name string) (File, error) {
	n, err := normalize(name)
	if err != nil {
		return nil, pathErr("create", name, err)
	}
	f, err := fs.root.Create(n)
	return f, convertError(err)
}

// Touch opens the named file, creating it and any missing parent directories.
func (fs *FS) Touch(name string, flag int, mode FileMode) (File, error) {
	n, err := normalize(name)
	if err != nil {
		return nil, pathErr("touch", name, err)
	}

	if f, err := fs.root.OpenFile(n, flag, mode); err == nil {
		return f, nil
	} else if !os.IsNotExist(err) {
		return nil, convertError(err)
	}

	if dir := path.Dir(n); dir != "." {
		if err := fs.root.MkdirAll(dir, 0o755); err != nil {
			return nil, convertError(err)
		}
	}

	f, err := fs.root.OpenFile(n, flag|O_CREATE, mode)
	return f, convertError(err)
}

// Stat returns a FileInfo describing the named file, following symlinks.
func (fs *FS) Stat(name string) (FileInfo, error) {
	n, err := normalize(name)
	if err != nil {
		return nil, pathErr("stat", name, err)
	}
	fi, err := fs.root.Stat(n)
	return fi, convertError(err)
}

// Lstat returns a FileInfo describing the named file without following symlinks.
func (fs *FS) Lstat(name string) (FileInfo, error) {
	n, err := normalize(name)
	if err != nil {
		return nil, pathErr("lstat", name, err)
	}
	fi, err := fs.root.Lstat(n)
	return fi, convertError(err)
}

// Mkdir creates a single directory.
func (fs *FS) Mkdir(name string, mode FileMode) error {
	n, err := normalize(name)
	if err != nil {
		return pathErr("mkdir", name, err)
	}
	return convertError(fs.root.Mkdir(n, mode))
}

// MkdirAll creates a directory and any missing parents.
func (fs *FS) MkdirAll(name string, mode FileMode) error {
	n, err := normalize(name)
	if err != nil {
		return pathErr("mkdirall", name, err)
	}
	return convertError(fs.root.MkdirAll(n, mode))
}

// Remove removes the named file or empty directory, adjusting tracked disk
// usage by the size of what was removed.
func (fs *FS) Remove(name string) error {
	n, err := normalize(name)
	if err != nil {
		return pathErr("remove", name, err)
	}
	fs.accountRemoval(n)
	return convertError(fs.root.Remove(n))
}

// RemoveAll removes the named path and any children it contains.
func (fs *FS) RemoveAll(name string) error {
	n, err := normalize(name)
	if err != nil {
		return pathErr("removeall", name, err)
	}
	if n == "." {
		// Refuse to remove the sandbox root itself; upstream had the same guard.
		return pathErr("removeall", name, ErrBadPathResolution)
	}
	fs.accountRemoval(n)
	return convertError(fs.root.RemoveAll(n))
}

// RemoveContents empties the named directory without removing it.
func (fs *FS) RemoveContents(name string) error {
	n, err := normalize(name)
	if err != nil {
		return pathErr("removecontents", name, err)
	}
	entries, err := fs.ReadDir(n)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := fs.RemoveAll(path.Join(n, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Rename moves oldpath to newpath. Both must resolve inside the sandbox.
//
// The semantics match upstream's implementation, which the Panel and SFTP layers
// depend on and which os.Root does not provide on its own:
//
//   - renaming the sandbox root itself is refused
//   - an existing destination is refused rather than silently overwritten
//   - missing parent directories of the destination are created
//
// The overwrite case matters most. Windows will happily replace the destination,
// so without this check a file manager rename could destroy an unrelated file.
func (fs *FS) Rename(oldpath, newpath string) error {
	o, err := normalize(oldpath)
	if err != nil {
		return pathErr("rename", oldpath, err)
	}
	n, err := normalize(newpath)
	if err != nil {
		return pathErr("rename", newpath, err)
	}

	if o == n {
		return nil
	}
	if o == "." {
		return pathErr("rename", oldpath, ErrBadPathResolution)
	}
	if n == "." {
		return pathErr("rename", newpath, ErrBadPathResolution)
	}

	// Surface a proper not-exist error for the source rather than whatever the
	// rename itself would report.
	if _, err := fs.root.Lstat(o); err != nil {
		return convertError(err)
	}

	// Refuse to clobber an existing destination.
	if _, err := fs.root.Lstat(n); err == nil {
		return pathErr("rename", newpath, ErrExist)
	} else if !errors.Is(err, ErrNotExist) {
		return convertError(err)
	}

	// Create the destination's parents if they are missing.
	if dir := path.Dir(n); dir != "." {
		if _, err := fs.root.Lstat(dir); err != nil {
			if !errors.Is(err, ErrNotExist) {
				return convertError(err)
			}
			if err := fs.root.MkdirAll(dir, 0o755); err != nil {
				return convertError(err)
			}
		}
	}

	if err := fs.root.Rename(o, n); err != nil {
		converted := convertError(err)
		if errors.Is(converted, ErrBadPathResolution) {
			return converted
		}
		return &LinkError{Op: "rename", Old: oldpath, New: newpath, Err: converted}
	}
	return nil
}

// Chmod changes the mode of the named file. On Windows only the 0200 bit has
// any effect, toggling the read-only attribute.
func (fs *FS) Chmod(name string, mode FileMode) error {
	n, err := normalize(name)
	if err != nil {
		return pathErr("chmod", name, err)
	}
	return convertError(fs.root.Chmod(n, mode))
}

// Chtimes changes the access and modification times of the named file.
func (fs *FS) Chtimes(name string, atime, mtime time.Time) error {
	n, err := normalize(name)
	if err != nil {
		return pathErr("chtimes", name, err)
	}
	return convertError(fs.root.Chtimes(n, atime, mtime))
}

// Symlink creates newname as a symbolic link to oldname.
//
// Creating symlinks on Windows requires either administrator rights or
// Developer Mode, so this generally fails for an unprivileged daemon. That is
// not a problem worth solving: the sandbox refuses to traverse links that leave
// it either way.
func (fs *FS) Symlink(oldname, newname string) error {
	n, err := normalize(newname)
	if err != nil {
		return pathErr("symlink", newname, err)
	}
	return convertError(fs.root.Symlink(oldname, n))
}

// Readlink returns the destination of the named symbolic link.
func (fs *FS) Readlink(name string) (string, error) {
	n, err := normalize(name)
	if err != nil {
		return "", pathErr("readlink", name, err)
	}
	s, err := fs.root.Readlink(n)
	return s, convertError(err)
}

// ReadDir reads the named directory, returning its entries sorted by filename.
func (fs *FS) ReadDir(name string) ([]DirEntry, error) {
	n, err := normalize(name)
	if err != nil {
		return nil, pathErr("readdir", name, err)
	}
	f, err := fs.root.Open(n)
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

// ReadDirMap reads a directory and maps each entry through fn.
func ReadDirMap[T any](fs *FS, name string, fn func(DirEntry) (T, error)) ([]T, error) {
	entries, err := fs.ReadDir(name)
	if err != nil {
		return nil, err
	}

	out := make([]T, 0, len(entries))
	for _, e := range entries {
		v, err := fn(e)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
