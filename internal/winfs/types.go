package winfs

import (
	"errors"
	"io"
	iofs "io/fs"
	"os"
)

// DirEntry is an entry read from a directory.
type DirEntry = iofs.DirEntry

// FileInfo describes a file and is returned by Stat and Lstat.
type FileInfo = iofs.FileInfo

// FileMode represents a file's mode and permission bits.
//
// Windows honours very little of this. Only the 0200 bit (owner writable) has
// any effect, controlling the read-only attribute; the rest is carried so that
// archive and transfer code can round-trip modes recorded on other systems.
type FileMode = iofs.FileMode

// File describes a readable and/or writable file from a Filesystem.
//
// This mirrors upstream ufs.File minus Fd(), which exposed a Unix file
// descriptor. Windows has no equivalent worth exporting, and no caller outside
// the filesystem layer needed it.
type File interface {
	// Name returns the base name of the file.
	Name() string

	// Stat returns the FileInfo structure describing the file.
	Stat() (FileInfo, error)

	// ReadDir reads the contents of the directory associated with the file and
	// returns a slice of DirEntry values in directory order.
	ReadDir(n int) ([]DirEntry, error)

	// Readdirnames reads the contents of the directory associated with the file
	// and returns a slice of up to n names, in directory order.
	Readdirnames(n int) (names []string, err error)

	// Truncate changes the size of the file. It does not change the I/O offset.
	Truncate(size int64) error

	io.Closer

	io.Reader
	io.ReaderAt
	io.ReaderFrom

	io.Writer
	io.WriterAt

	io.Seeker
}

// Assert that the standard library's file satisfies our interface, since every
// File this package returns is one.
var _ File = (*os.File)(nil)

// File mode bits, re-exported so callers do not need to import io/fs directly.
const (
	ModeDir        = iofs.ModeDir
	ModeAppend     = iofs.ModeAppend
	ModeExclusive  = iofs.ModeExclusive
	ModeTemporary  = iofs.ModeTemporary
	ModeSymlink    = iofs.ModeSymlink
	ModeDevice     = iofs.ModeDevice
	ModeNamedPipe  = iofs.ModeNamedPipe
	ModeSocket     = iofs.ModeSocket
	ModeSetuid     = iofs.ModeSetuid
	ModeSetgid     = iofs.ModeSetgid
	ModeCharDevice = iofs.ModeCharDevice
	ModeSticky     = iofs.ModeSticky
	ModeIrregular  = iofs.ModeIrregular
	ModeType       = iofs.ModeType
	ModePerm       = iofs.ModePerm
)

// Open flags. These come from the os package rather than golang.org/x/sys/unix,
// so they carry the values Windows actually uses.
//
// The Unix-only flags upstream exported (O_NOFOLLOW, O_DIRECTORY, O_CLOEXEC,
// O_LARGEFILE) are deliberately absent: os.Root refuses to traverse reparse
// points regardless, handles are not inherited by default on Windows, and there
// is no large-file distinction.
const (
	O_RDONLY = os.O_RDONLY
	O_WRONLY = os.O_WRONLY
	O_RDWR   = os.O_RDWR
	O_APPEND = os.O_APPEND
	O_CREATE = os.O_CREATE
	O_EXCL   = os.O_EXCL
	O_SYNC   = os.O_SYNC
	O_TRUNC  = os.O_TRUNC
)

// escapeErrText is the message os.Root uses when a path resolves outside the
// root. Guarded by TestEscapeErrorText.
const escapeErrText = "path escapes from parent"

var (
	// ErrIsDirectory is an error for when an operation that operates only on
	// files is given a path to a directory.
	ErrIsDirectory = errors.New("is a directory")
	// ErrNotDirectory is an error for when an operation that operates only on
	// directories is given a path to a file.
	ErrNotDirectory = errors.New("not a directory")
	// ErrBadPathResolution is an error for when a sand-boxed filesystem
	// resolves a given path to a forbidden location.
	ErrBadPathResolution = errors.New("bad path resolution")
	// ErrNotRegular is an error for when an operation that operates only on
	// regular files is passed something other than a regular file.
	ErrNotRegular = errors.New("not a regular file")

	// ErrClosed is an error for when an entry was accessed after being closed.
	ErrClosed = iofs.ErrClosed
	// ErrInvalid is an error for when an invalid argument was used.
	ErrInvalid = iofs.ErrInvalid
	// ErrExist is an error for when an entry already exists.
	ErrExist = iofs.ErrExist
	// ErrNotExist is an error for when an entry does not exist.
	ErrNotExist = iofs.ErrNotExist
	// ErrPermission is an error for when the required permissions to perform an
	// operation are missing.
	ErrPermission = iofs.ErrPermission
)

// LinkError records an error during a link, symlink or rename operation.
type LinkError = os.LinkError

// PathError records an error and the operation and file path that caused it.
type PathError = iofs.PathError

// SyscallError records an error from a specific system call.
type SyscallError = os.SyscallError

// NewSyscallError returns, as an error, a new [*os.SyscallError] with the given
// system call name and error details. If err is nil, it returns nil.
func NewSyscallError(syscall string, err error) error {
	return os.NewSyscallError(syscall, err)
}

// convertError normalises errors returned by os.Root into this package's
// sentinels.
//
// os.Root reports every containment failure as "path escapes from parent",
// which callers need to see as ErrBadPathResolution so the HTTP and SFTP layers
// return the right status. The escape text is matched rather than compared by
// identity because the error is unexported by the standard library.
func convertError(err error) error {
	if err == nil {
		return nil
	}

	var pe *PathError
	if errors.As(err, &pe) {
		if isEscapeError(pe.Err) {
			pe.Err = ErrBadPathResolution
		}
		return pe
	}

	var le *LinkError
	if errors.As(err, &le) {
		if isEscapeError(le.Err) {
			le.Err = ErrBadPathResolution
		}
		return le
	}

	if isEscapeError(err) {
		return ErrBadPathResolution
	}
	return err
}

func isEscapeError(err error) bool {
	if err == nil {
		return false
	}
	// The value os.Root returns is unexported and matches none of the io/fs
	// sentinels, so the message is the only available signal. TestEscapeErrorText
	// guards the exact string: if a Go upgrade rewords it, that test fails rather
	// than this silently returning false and downgrading every escape to a
	// generic error.
	return err.Error() == escapeErrText
}
