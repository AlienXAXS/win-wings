package winfs

import (
	"errors"
	iofs "io/fs"
	"path"
)

// SkipDir, when returned from a WalkDirFunc, skips the remaining contents of
// the directory it was called on.
var SkipDir = iofs.SkipDir

// SkipAll, when returned from a WalkDirFunc, terminates the walk without error.
var SkipAll = iofs.SkipAll

// WalkDirFunc is called for each entry visited by a walk.
//
// dir is a handle on the directory containing the entry, valid only for the
// duration of the call; do not retain or close it. name is the entry's name
// within dir, and relative is its path relative to the walk's starting point.
//
// This mirrors upstream's callback shape, with the raw dirfd replaced by a Dir.
type WalkDirFunc func(dir *Dir, name, relative string, entry DirEntry, err error) error

// WalkDir walks the tree rooted at name, calling fn for each entry.
//
// Directories are visited before their contents. Symlinks are reported but not
// followed — os.Root refuses to traverse them out of the sandbox in any case,
// and following them inside it risks cycles.
func (fs *FS) WalkDir(name string, fn WalkDirFunc) error {
	d, err := fs.Dir(name)
	if err != nil {
		// Report the failure through the callback so callers can decide, matching
		// the behaviour of io/fs.WalkDir.
		return fn(nil, path.Base(name), "", nil, err)
	}
	defer d.Close()

	err = walkDir(d, "", fn)
	if errors.Is(err, SkipDir) || errors.Is(err, SkipAll) {
		return nil
	}
	return err
}

// WalkDir walks the tree rooted at this directory.
func (d *Dir) WalkDir(fn WalkDirFunc) error {
	err := walkDir(d, "", fn)
	if errors.Is(err, SkipDir) || errors.Is(err, SkipAll) {
		return nil
	}
	return err
}

// WalkFrom walks the subtree rooted at name within this directory.
//
// This is the replacement for upstream's WalkDirat(dirfd, name, fn), which is
// how callers walk a path they have already resolved through SafeDir: the Dir
// is the parent handle and name is the final element.
func (d *Dir) WalkFrom(name string, fn WalkDirFunc) error {
	if name == "." || name == "" {
		return d.WalkDir(fn)
	}

	sub, err := d.Sub(name)
	if err != nil {
		// Not a directory, or unreadable. Report it through the callback the same
		// way the walk reports any other per-entry failure.
		return fn(d, name, "", nil, err)
	}
	defer sub.Close()

	err = walkDir(sub, "", fn)
	if errors.Is(err, SkipDir) || errors.Is(err, SkipAll) {
		return nil
	}
	return err
}

func walkDir(d *Dir, prefix string, fn WalkDirFunc) error {
	entries, err := d.ReadDir()
	if err != nil {
		// Hand the read failure to the callback rather than aborting the whole
		// walk: a single unreadable directory should not fail a backup or a disk
		// usage calculation for an entire server.
		if err := fn(d, "", prefix, nil, err); err != nil {
			return err
		}
		return nil
	}

	for _, entry := range entries {
		name := entry.Name()
		relative := path.Join(prefix, name)

		if err := fn(d, name, relative, entry, nil); err != nil {
			if errors.Is(err, SkipDir) {
				// Skip this entry's contents, continue with its siblings.
				if entry.IsDir() {
					continue
				}
				return nil
			}
			return err
		}

		if !entry.IsDir() {
			continue
		}

		sub, err := d.Sub(name)
		if err != nil {
			if err := fn(d, name, relative, entry, err); err != nil {
				return err
			}
			continue
		}

		err = walkDir(sub, relative, fn)
		_ = sub.Close()
		if err != nil {
			return err
		}
	}

	return nil
}
