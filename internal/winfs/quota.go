package winfs

import (
	"math"
)

// Disk accounting.
//
// Ported from upstream's ufs.Quota with the same semantics, including the
// sentinel values, because the Panel and the rest of wings rely on them:
//
//	limit == -1  no write operation is permitted
//	limit ==  0  unlimited
//	usage == -1  usage has not been calculated yet
//
// Only deletions are accounted automatically. Writes are reserved explicitly by
// the caller before they happen, since a write that would exceed the limit has
// to be refused before any bytes land on disk.

// Limit returns the size limit of the filesystem in bytes.
func (fs *FS) Limit() int64 { return fs.limit.Load() }

// SetLimit sets the size limit and returns the previous value.
func (fs *FS) SetLimit(newLimit int64) int64 { return fs.limit.Swap(newLimit) }

// Usage returns the currently tracked usage in bytes.
func (fs *FS) Usage() int64 { return fs.usage.Load() }

// SetUsage overwrites the tracked usage and returns the previous value.
func (fs *FS) SetUsage(newUsage int64) int64 { return fs.usage.Swap(newUsage) }

// Add adjusts the tracked usage by i, saturating at 0 and MaxInt64 rather than
// overflowing, and returns the new total.
func (fs *FS) Add(i int64) int64 {
	for {
		usage := fs.Usage()
		var next int64

		switch {
		case i > 0:
			if usage > math.MaxInt64-i {
				next = math.MaxInt64
			} else {
				next = usage + i
			}
		case i < 0:
			if i == math.MinInt64 {
				next = 0
			} else if usage <= -i {
				next = 0
			} else {
				next = usage + i
			}
		default:
			return usage
		}

		if fs.usage.CompareAndSwap(usage, next) {
			return next
		}
	}
}

// CanFit reports whether size additional bytes can be written without exceeding
// the limit.
func (fs *FS) CanFit(size int64) bool {
	limit := fs.Limit()
	switch limit {
	case -1:
		// No write operations are allowed.
		return false
	case 0:
		// Unlimited.
		return true
	}

	usage := fs.Usage()
	if usage == -1 {
		// Usage has not been calculated yet, so allow the write rather than
		// blocking every operation until the first walk completes.
		return true
	}

	if size <= 0 {
		return true
	}
	if usage >= limit {
		return false
	}
	return size <= limit-usage
}

// accountRemoval subtracts the size of the entry at name from tracked usage.
//
// Called before the removal actually happens, since the size cannot be read
// afterwards. A failure to stat is ignored: losing accounting precision is
// preferable to refusing a deletion, and the next full disk usage walk corrects
// any drift.
func (fs *FS) accountRemoval(name string) {
	fi, err := fs.Lstat(name)
	if err != nil {
		return
	}
	if fi.IsDir() {
		fs.accountTreeRemoval(name)
		return
	}
	fs.Add(-fi.Size())
}

// accountTreeRemoval subtracts the size of every regular file beneath name.
func (fs *FS) accountTreeRemoval(name string) {
	var total int64
	_ = fs.WalkDir(name, func(dir *Dir, entry, _ string, d DirEntry, err error) error {
		if err != nil || d == nil || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := dir.Lstat(entry); err == nil {
			total += fi.Size()
		}
		return nil
	})
	if total > 0 {
		fs.Add(-total)
	}
}
