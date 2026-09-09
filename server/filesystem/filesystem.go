package filesystem

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/gabriel-vasile/mimetype"
	ignore "github.com/sabhiram/go-gitignore"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/winfs"
)

type Filesystem struct {
	winFS *winfs.FS

	mu                sync.RWMutex
	lastLookupTime    *usageLookupTime
	lookupInProgress  atomic.Bool
	diskCheckInterval time.Duration
	denylist          *ignore.GitIgnore

	isTest bool
}

// New creates a new Filesystem instance for a given server.
func New(root string, size int64, denylist []string) (*Filesystem, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	winFS, err := winfs.New(root, size)
	if err != nil {
		return nil, err
	}

	return &Filesystem{
		winFS: winFS,

		diskCheckInterval: time.Duration(config.Get().System.DiskCheckInterval),
		lastLookupTime:    &usageLookupTime{},
		denylist:          ignore.CompileIgnoreLines(denylist...),
	}, nil
}

// Path returns the root path for the Filesystem instance.
func (fs *Filesystem) Path() string {
	return fs.winFS.BasePath()
}

// ReadDir reads directory entries.
func (fs *Filesystem) ReadDir(path string) ([]winfs.DirEntry, error) {
	return fs.winFS.ReadDir(path)
}

// ReadDirStat is like ReadDir except that it returns FileInfo for each entry
// instead of just a DirEntry.
func (fs *Filesystem) ReadDirStat(path string) ([]winfs.FileInfo, error) {
	return winfs.ReadDirMap(fs.winFS, path, func(e winfs.DirEntry) (winfs.FileInfo, error) {
		return e.Info()
	})
}

// File returns a reader for a file instance as well as the stat information.
func (fs *Filesystem) File(p string) (winfs.File, Stat, error) {
	f, err := fs.winFS.Open(p)
	if err != nil {
		return nil, Stat{}, err
	}
	st, err := statFromFile(f)
	if err != nil {
		_ = f.Close()
		return nil, Stat{}, err
	}
	return f, st, nil
}

// WinFS exposes the underlying sandboxed filesystem.
func (fs *Filesystem) WinFS() *winfs.FS {
	return fs.winFS
}

// Touch acts by creating the given file and path on the disk if it is not present
// already. If  it is present, the file is opened using the defaults which will truncate
// the contents. The opened file is then returned to the caller.
func (fs *Filesystem) Touch(p string, flag int) (winfs.File, error) {
	var currentSize int64
	st, err := fs.winFS.Stat(p)
	if err != nil && !errors.Is(err, winfs.ErrNotExist) {
		return nil, err
	} else if err == nil && !st.IsDir() {
		currentSize = st.Size()
	}

	file, err := fs.winFS.Touch(p, flag, 0o644)
	if err != nil {
		return nil, err
	}
	return newQuotaFile(fs, file, currentSize), nil
}

// Writefile writes a file to the system. If the file does not already exist one
// will be created. This will also properly recalculate the disk space used by
// the server when writing new files or modifying existing ones.
//
// DEPRECATED: use `Write` instead.
func (fs *Filesystem) Writefile(p string, r io.Reader) error {
	var currentSize int64
	st, err := fs.winFS.Stat(p)
	if err != nil && !errors.Is(err, winfs.ErrNotExist) {
		return errors.Wrap(err, "server/filesystem: writefile: failed to stat file")
	} else if err == nil {
		if st.IsDir() {
			// TODO: resolved
			return errors.WithStack(&Error{code: ErrCodeIsDirectory, resolved: ""})
		}
		currentSize = st.Size()
	}

	// Touch the file and return the handle to it at this point. This will
	// create or truncate the file, and create any necessary parent directories
	// if they are missing.
	file, err := fs.winFS.Touch(p, winfs.O_RDWR|winfs.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("error touching file: %w", err)
	}
	defer file.Close()

	// Do not use CopyBuffer here, it is wasteful as the file implements
	// io.ReaderFrom, which causes it to not use the buffer anyways.
	n, err := io.Copy(file, r)

	// Adjust the disk usage to account for the old size and the new size of the file.
	fs.winFS.Add(n - currentSize)

	if err := fs.chownFile(p); err != nil {
		return fmt.Errorf("error chowning file: %w", err)
	}
	// Return the error from io.Copy.
	return err
}

func (fs *Filesystem) Write(p string, r io.Reader, newSize int64, mode winfs.FileMode) error {
	var currentSize int64
	st, err := fs.winFS.Stat(p)
	if err != nil && !errors.Is(err, winfs.ErrNotExist) {
		return errors.Wrap(err, "server/filesystem: writefile: failed to stat file")
	} else if err == nil {
		if st.IsDir() {
			// TODO: resolved
			return errors.WithStack(&Error{code: ErrCodeIsDirectory, resolved: ""})
		}
		currentSize = st.Size()
	}

	// Check that the new size we're writing to the disk can fit. If there is currently
	// a file we'll subtract that current file size from the size of the buffer to determine
	// the amount of new data we're writing (or amount we're removing if smaller).
	if err := fs.HasSpaceFor(newSize - currentSize); err != nil {
		return err
	}

	// Ensure the parent directories exist and are owned by the server user
	// before creating the file. Touch would create any missing parents
	// implicitly, but as the user Wings runs as; creating them here lets us
	// chown the ones we add.
	if err := fs.mkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}

	// Touch the file and return the handle to it at this point. This will
	// create or truncate the file, and create any necessary parent directories
	// if they are missing.
	file, err := fs.winFS.Touch(p, winfs.O_RDWR|winfs.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer file.Close()

	if newSize == 0 {
		// Subtract the previous size of the file if the new size is 0.
		fs.winFS.Add(-currentSize)
	} else {
		// Do not use CopyBuffer here, it is wasteful as the file implements
		// io.ReaderFrom, which causes it to not use the buffer anyways.
		var n int64
		n, err = io.Copy(file, io.LimitReader(r, newSize))

		// Adjust the disk usage to account for the old size and the new size of the file.
		fs.winFS.Add(n - currentSize)
	}

	if err := fs.chownFile(p); err != nil {
		return err
	}
	// Return any remaining error.
	return err
}

// CreateDirectory creates a new directory (name) at a specified path (p) for
// the server.
func (fs *Filesystem) CreateDirectory(name string, p string) error {
	return fs.mkdirAll(filepath.Join(p, name), 0o755)
}

func (fs *Filesystem) Rename(oldpath, newpath string) error {
	return fs.winFS.Rename(oldpath, newpath)
}

func (fs *Filesystem) Symlink(oldpath, newpath string) error {
	return fs.winFS.Symlink(oldpath, newpath)
}

// chownFile is a no-op on Windows.
//
// Upstream chowned every file it created to the server's uid/gid so that files
// were not left owned by the account wings ran as. Windows has no equivalent
// worth performing per-file: a new file inherits its parent directory's ACL, and
// server separation is achieved by giving each server's data directory an ACL
// naming that server's own local account. Walking a large server tree to stamp
// ownership on every file would be expensive and would change nothing.
//
// See config.AccountConfiguration for the isolation model this replaces.
func (fs *Filesystem) chownFile(_ string) error {
	return nil
}

// mkdirAll creates the directory p along with any missing parents.
func (fs *Filesystem) mkdirAll(p string, mode winfs.FileMode) error {
	return fs.winFS.MkdirAll(p, mode)
}

// Chown is a no-op on Windows.
//
// It is kept because the Panel triggers it and the router exposes it. See
// chownFile for why there is nothing to do: file ownership is not the mechanism
// that separates servers here, directory ACLs are.
func (fs *Filesystem) Chown(_ string) error {
	return nil
}

func (fs *Filesystem) Chmod(path string, mode winfs.FileMode) error {
	return fs.winFS.Chmod(path, mode)
}

// Begin looping up to 50 times to try and create a unique copy file name. This will take
// an input of "file.txt" and generate "file copy.txt". If that name is already taken, it will
// then try to write "file copy 2.txt" and so on, until reaching 50 loops. At that point we
// won't waste anymore time, just use the current timestamp and make that copy.
//
// Could probably make this more efficient by checking if there are any files matching the copy
// pattern, and trying to find the highest number and then incrementing it by one rather than
// looping endlessly.
func (fs *Filesystem) findCopySuffix(dir *winfs.Dir, name, extension string) (string, error) {
	var i int
	suffix := " copy"

	for i = 0; i < 51; i++ {
		if i > 0 {
			suffix = " copy " + strconv.Itoa(i)
		}

		n := name + suffix + extension
		// If we stat the file and it does not exist that means we're good to create the copy. If it
		// does exist, we'll just continue to the next loop and try again.
		if _, err := dir.Lstat(n); err != nil {
			if !errors.Is(err, winfs.ErrNotExist) {
				return "", err
			}
			break
		}

		if i == 50 {
			suffix = "copy." + time.Now().Format(time.RFC3339)
		}
	}

	return name + suffix + extension, nil
}

// Copy copies a given file to the same location and appends a suffix to the
// file to indicate that it has been copied.
func (fs *Filesystem) Copy(p string) error {
	dir, name, closeFd, err := fs.winFS.SafeDir(p)
	defer closeFd()
	if err != nil {
		return err
	}
	source, err := dir.OpenFile(name, winfs.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		// If this is a directory or not a regular file, just throw a not-exist error
		// since anything calling this function should understand what that means.
		return winfs.ErrNotExist
	}
	currentSize := info.Size()

	// Check that copying this file wouldn't put the server over its limit.
	if err := fs.HasSpaceFor(currentSize); err != nil {
		return err
	}

	base := info.Name()
	extension := filepath.Ext(base)
	baseName := strings.TrimSuffix(base, extension)

	// Ensure that ".tar" is also counted as apart of the file extension.
	// There might be a better way to handle this for other double file extensions,
	// but this is a good workaround for now.
	if strings.HasSuffix(baseName, ".tar") {
		extension = ".tar" + extension
		baseName = strings.TrimSuffix(baseName, ".tar")
	}

	newName, err := fs.findCopySuffix(dir, baseName, extension)
	if err != nil {
		return err
	}
	dst, err := dir.OpenFile(newName, winfs.O_WRONLY|winfs.O_CREATE, info.Mode())
	if err != nil {
		return err
	}
	defer dst.Close()

	// Do not use CopyBuffer here, it is wasteful as the file implements
	// io.ReaderFrom, which causes it to not use the buffer anyways.
	n, err := io.Copy(dst, io.LimitReader(source, currentSize))
	fs.winFS.Add(n)

	// Return the error from io.Copy. Nothing to chown: the copy inherits the
	// destination directory's ACL, which is the server's own.
	return err
}

// TruncateRootDirectory removes _all_ files and directories from a server's
// data directory and resets the used disk space to zero.
func (fs *Filesystem) TruncateRootDirectory() error {
	var limit int64
	if !fs.isTest {
		limit = fs.winFS.Limit()
	}

	// The sandbox handle must be released before the directory can be removed.
	//
	// Upstream removed the tree first and closed afterwards, which is fine on
	// Linux where an open handle does not prevent unlinking. Windows refuses to
	// delete a directory that anything still holds open, so that order fails
	// here with a sharing violation.
	if err := fs.winFS.Close(); err != nil {
		return err
	}

	if err := os.RemoveAll(fs.Path()); err != nil {
		return err
	}
	if err := os.Mkdir(fs.Path(), 0o755); err != nil {
		return err
	}

	winFS, err := winfs.New(fs.Path(), limit)
	if err != nil {
		return err
	}
	fs.winFS = winFS
	fs.winFS.SetUsage(0)
	return nil
}

// Close releases the filesystem's sandbox handle.
//
// Callers that create a Filesystem for a short-lived purpose must call this.
// Windows will not allow the server's directory to be renamed or deleted while
// the handle is open, so leaking one blocks transfers and deletions.
func (fs *Filesystem) Close() error {
	return fs.winFS.Close()
}

// Delete removes a file or folder from the system. Prevents the user from
// accidentally (or maliciously) removing their root server data directory.
func (fs *Filesystem) Delete(p string) error {
	return fs.winFS.RemoveAll(p)
}

//type fileOpener struct {
//	fs   *Filesystem
//	busy uint
//}
//
//// Attempts to open a given file up to "attempts" number of times, using a backoff. If the file
//// cannot be opened because of a "text file busy" error, we will attempt until the number of attempts
//// has been exhaused, at which point we will abort with an error.
//func (fo *fileOpener) open(path string, flags int, perm winfs.FileMode) (winfs.File, error) {
//	for {
//		f, err := fo.fs.winFS.OpenFile(path, flags, perm)
//
//		// If there is an error because the text file is busy, go ahead and sleep for a few
//		// hundred milliseconds and then try again up to three times before just returning the
//		// error back to the caller.
//		//
//		// Based on code from: https://github.com/golang/go/issues/22220#issuecomment-336458122
//		if err != nil && fo.busy < 3 && strings.Contains(err.Error(), "text file busy") {
//			time.Sleep(100 * time.Millisecond << fo.busy)
//			fo.busy++
//			continue
//		}
//
//		return f, err
//	}
//}

// ListDirectory lists the contents of a given directory and returns stat
// information about each file and folder within it.
func (fs *Filesystem) ListDirectory(p string) ([]Stat, error) {
	// Read entries from the path on the filesystem, using the mapped reader, so
	// we can map the DirEntry slice into a Stat slice with mimetype information.
	out, err := winfs.ReadDirMap(fs.winFS, p, func(e winfs.DirEntry) (Stat, error) {
		info, err := e.Info()
		if err != nil {
			return Stat{}, err
		}

		var d string
		if e.Type().IsDir() {
			d = "inode/directory"
		} else {
			d = "application/octet-stream"
		}
		var m *mimetype.MIME
		if e.Type().IsRegular() {
			// TODO: I should probably find a better way to do this.
			eO := e.(interface {
				Open() (winfs.File, error)
			})
			f, err := eO.Open()
			if err != nil {
				return Stat{}, err
			}
			m, err = mimetype.DetectReader(f)
			if err != nil {
				log.Error(err.Error())
			}
			_ = f.Close()
		}

		st := Stat{FileInfo: info, Mimetype: d}
		if m != nil {
			st.Mimetype = m.String()
		}
		return st, nil
	})
	if err != nil {
		return nil, err
	}

	// Sort entries alphabetically.
	slices.SortStableFunc(out, func(a, b Stat) int {
		switch {
		case a.Name() == b.Name():
			return 0
		case a.Name() > b.Name():
			return 1
		default:
			return -1
		}
	})

	// Sort folders before other file types.
	slices.SortStableFunc(out, func(a, b Stat) int {
		switch {
		case a.IsDir() && b.IsDir():
			return 0
		case a.IsDir():
			return -1
		default:
			return 1
		}
	})

	return out, nil
}

func (fs *Filesystem) Chtimes(path string, atime, mtime time.Time) error {
	if fs.isTest {
		return nil
	}
	return fs.winFS.Chtimes(path, atime, mtime)
}
