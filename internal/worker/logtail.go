//go:build windows

package worker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sys/windows"

	"github.com/pterodactyl/wings/internal/wire"
)

// Following a server's log file, for the eggs whose game says nothing on stdout.
//
// The Linux eggs do this with `tail -c0 -F`, and the two flags matter as much as
// the command: -c0 starts at the end so a restart does not replay the last run,
// and -F follows the *name* rather than the handle, so a server that recreates
// its log on boot is followed into the new file. Both are reproduced here.
//
// PowerShell's Get-Content -Wait is the obvious substitute and is not one. It
// opens the file with a share mode that stops the game rotating it, guesses the
// encoding, and follows the handle rather than the name, so it goes silent for
// the rest of the run the first time the log is recreated.

const (
	// tailPollInterval is how often the file is checked for new bytes. Console
	// output is read by humans; a quarter of a second is imperceptible and costs
	// four wakeups a second per server that uses this at all.
	tailPollInterval = 250 * time.Millisecond

	// tailReadSize bounds one read. A server that writes a megabyte between two
	// polls is streamed over several of them rather than in one allocation.
	tailReadSize = 64 * 1024
)

// followLogFile streams a server's log file onto the console until done is
// closed.
//
// It is started with the run and dies with it, like every other pump: a tail
// that outlived its run would carry the previous server's output into the next
// one's console.
func (w *Worker) followLogFile(src wire.LogSource, dir string, done <-chan struct{}) {
	defer w.guard("following the server's log file", nil)

	path, err := resolveLogPath(dir, src.Path)
	if err != nil {
		// Not fatal to the run. The server is up and its stdout is still being
		// read; what is lost is the log relay, and saying so is more useful than
		// killing a working server over a bad profile field.
		w.Log(wire.LogError, "not following the server's log file", "error", err)
		return
	}

	decode, err := decoderFor(src.Encoding)
	if err != nil {
		w.Log(wire.LogError, "not following the server's log file", "error", err)
		return
	}

	w.Log(wire.LogInfo, "following the server's log file onto the console",
		"path", path, "encoding", encodingName(src.Encoding))

	t := &tail{path: path, decode: decode, w: w}
	defer t.close()

	// A log that is already there when the run begins is the previous run's, and
	// replaying it would put a dead server's output at the top of a live server's
	// console. So reading starts at the length it had at this moment, which is
	// what tail -c0 does.
	//
	// The length is taken now and remembered, rather than seeking to the end when
	// the file is first opened. The server is already running by this point and
	// may well have written its first lines, and seeking to the end on the first
	// poll would silently skip exactly the output that says how it started.
	//
	// A log that is not there yet is a different case and is read whole: whatever
	// appears at that path from here on was written by this run.
	if info, err := os.Stat(path); err == nil {
		t.skipTo = info.Size()
		t.skipping = true
		w.Log(wire.LogDebug, "the server's log file is from a previous run; following it from its end",
			"path", path, "bytes", info.Size())
	}

	// Opened eagerly rather than on the first tick, so that a server writing
	// steadily from the moment it starts is followed from here rather than from a
	// quarter of a second later. A file that does not exist yet is picked up by
	// the polling below, which is the ordinary case.
	if err := t.open(); err != nil && !os.IsNotExist(err) {
		w.Log(wire.LogDebug, "could not open the server's log file yet; waiting for it",
			"path", path, "error", err.Error())
	}

	// Said once rather than every poll: a log that never appears is worth a line,
	// a log that has not appeared *yet* is the normal case for the first minute
	// of a server's life.
	announcedMissing := false

	ticker := time.NewTicker(tailPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			// One last pass, so the lines explaining why a server exited are not
			// lost to the race between the game writing them and the process
			// ending. There is nothing left to poll after this.
			t.drain()
			return
		case <-w.shutdown:
			return
		case <-ticker.C:
			switch err := t.drain(); {
			case err == nil:
				announcedMissing = false
			case os.IsNotExist(err):
				if !announcedMissing {
					announcedMissing = true
					w.Log(wire.LogDebug, "the server's log file does not exist yet; waiting for it",
						"path", path)
				}
			default:
				if !announcedMissing {
					announcedMissing = true
					w.Log(wire.LogWarn, "could not read the server's log file; still trying",
						"path", path, "error", err.Error())
				}
			}
		}
	}
}

// tail is the state of following one path across the files that appear at it.
type tail struct {
	path   string
	decode decoder
	w      *Worker

	f      *os.File
	id     fileID
	offset int64

	// carry holds the bytes of a multi-byte character split across two reads.
	carry []byte

	// skipping and skipTo carry the previous run's leftovers past. skipTo is the
	// length the file had when the tail began; only the file that was already
	// there is skipped, because one the server creates afterwards is its own and
	// is read whole.
	skipping bool
	skipTo   int64
}

// drain reads everything currently available, reopening the file if it has been
// rotated, truncated or replaced.
func (t *tail) drain() error {
	if t.f == nil {
		if err := t.open(); err != nil {
			return err
		}
	}

	for {
		buf := make([]byte, tailReadSize)
		n, err := t.f.Read(buf)
		if n > 0 {
			t.offset += int64(n)
			t.emit(buf[:n])
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			// A read error on a handle we hold is not recoverable by retrying the
			// same handle; drop it and let the next poll reopen the path.
			t.close()
			return err
		}
		// Nothing more in this file. That is either the ordinary case -- the game
		// has not written since the last poll -- or the file at this path is no
		// longer the one being held.
		return t.checkRotation()
	}
}

// checkRotation notices the three ways a log file stops being the file that is
// open: it was truncated in place, it was deleted and recreated, or it was
// renamed away and a new one took its name.
//
// Rotation is only checked when there is nothing left to read, so the cost is
// one file open per idle poll rather than one per read.
func (t *tail) checkRotation() error {
	size, err := t.f.Seek(0, 1)
	if err == nil {
		if end, serr := t.f.Seek(0, 2); serr == nil {
			if end < size {
				// Truncated in place: the file is the same one, so it is not
				// reopened, but everything after the truncation point is new.
				t.w.Log(wire.LogDebug, "the server's log file was truncated; following it from the start",
					"path", t.path)
				t.offset = 0
				t.carry = nil
				_, _ = t.f.Seek(0, 0)
				return nil
			}
			_, _ = t.f.Seek(size, 0)
		}
	}

	id, err := identifyPath(t.path)
	if err != nil {
		// The file has gone. Keep the handle: on Windows a deleted-but-open file
		// is still readable, and the next poll picks up its replacement.
		return nil
	}
	if id == t.id {
		return nil
	}

	t.w.Log(wire.LogDebug, "the server's log file was replaced; following the new one",
		"path", t.path)
	t.close()
	// Read from the start: a file the server created during this run belongs to
	// this run, however it came to be there.
	t.skipping = false
	return t.open()
}

// open takes a handle on the path, positioned where reading should resume.
func (t *tail) open() error {
	f, id, err := openForTailing(t.path)
	if err != nil {
		return err
	}

	t.f = f
	t.id = id
	t.carry = nil
	t.offset = 0

	if t.skipping {
		t.skipping = false
		// Whichever is smaller. A file shorter than it was has been replaced or
		// truncated since the tail began, and every byte in it is this run's.
		at := t.skipTo
		if end, err := f.Seek(0, 2); err == nil && end < at {
			at = end
		}
		if _, err := f.Seek(at, 0); err == nil {
			t.offset = at
		}
	}

	// A file read from the start may begin with a byte order mark. That belongs
	// to the encoding rather than to the log, and the decoder drops it.
	return nil
}

func (t *tail) close() {
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
	t.carry = nil
}

// emit decodes a chunk and puts it on the console.
func (t *tail) emit(b []byte) {
	if len(t.carry) > 0 {
		b = append(t.carry, b...)
		t.carry = nil
	}
	out, rest := t.decode(b)
	t.carry = rest
	if len(out) > 0 {
		t.w.emitConsole(out)
	}
}

// fileID identifies a file on this host, so that a new file appearing under a
// name is distinguishable from the one that was there before.
type fileID struct {
	volume     uint32
	indexHigh  uint32
	indexLow   uint32
	generation uint64
}

// openForTailing opens a path for reading in the sharing mode a log file being
// written by somebody else requires.
//
// FILE_SHARE_DELETE is the one that matters and the one Go's os.Open does not
// ask for. Without it, a game that deletes or renames its log on startup -- most
// of them, since that is how a log gets a fresh file per run -- is denied by the
// tail holding it, and the sharing violation surfaces inside the game as a
// failure to open its own log.
func openForTailing(path string) (*os.File, fileID, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fileID{}, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND {
			return nil, fileID{}, os.ErrNotExist
		}
		return nil, fileID{}, err
	}

	f := os.NewFile(uintptr(h), path)
	id, err := identifyHandle(h)
	if err != nil {
		_ = f.Close()
		return nil, fileID{}, err
	}
	return f, id, nil
}

// identifyPath returns the identity of whatever file is at a path now.
func identifyPath(path string) (fileID, error) {
	f, id, err := openForTailing(path)
	if err != nil {
		return fileID{}, err
	}
	_ = f.Close()
	return id, nil
}

func identifyHandle(h windows.Handle) (fileID, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fileID{}, err
	}
	return fileID{
		volume:    info.VolumeSerialNumber,
		indexHigh: info.FileIndexHigh,
		indexLow:  info.FileIndexLow,
		// A file index is reused once a file is deleted, so two files at the same
		// path can share one. The creation time separates them.
		generation: uint64(info.CreationTime.HighDateTime)<<32 | uint64(info.CreationTime.LowDateTime),
	}, nil
}

// resolveLogPath turns a profile's log path into an absolute path inside the
// server's data directory, refusing anything that leaves it.
//
// The daemon checks this too. It is checked again here because the worker is
// what actually opens the file, and it does so as the daemon's account: a path
// that escaped would read anything on the host into a console the server's owner
// can watch.
func resolveLogPath(dir, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("worker: the console log source has no path")
	}
	return resolveContained(dir, rel, "console log path")
}

// decoder converts a chunk of a log file to UTF-8, returning any trailing bytes
// that are the start of a character the next chunk completes.
type decoder func([]byte) (out, carry []byte)

func encodingName(enc string) string {
	if strings.TrimSpace(enc) == "" {
		return "utf-8"
	}
	return strings.ToLower(strings.TrimSpace(enc))
}

// decoderFor returns the decoder for an encoding name.
//
// UTF-16 is here because it is what a .NET server gets from a StreamWriter left
// on its defaults, and a UTF-16 log put on a console verbatim reads as every
// character followed by a NUL -- which most terminals render as nothing at all,
// so the console looks empty rather than wrong.
func decoderFor(enc string) (decoder, error) {
	switch encodingName(enc) {
	case "utf-8", "utf8", "ascii", "ansi":
		return passthroughDecoder, nil
	case "utf-16le", "utf16le", "unicode":
		return utf16Decoder(false), nil
	case "utf-16be", "utf16be", "bigendianunicode":
		return utf16Decoder(true), nil
	default:
		return nil, fmt.Errorf("worker: %q is not a console log encoding this worker understands "+
			"(utf-8, utf-16le, utf-16be)", enc)
	}
}

// passthroughDecoder emits bytes unchanged, holding back an incomplete UTF-8
// sequence so a multi-byte character split across two reads is not printed as
// two replacement characters.
func passthroughDecoder(b []byte) ([]byte, []byte) {
	// A byte order mark is part of the encoding, not of the log. Dropped wherever
	// it appears rather than only at the very start, because the tail outlives
	// the file: a log rotated mid-run begins again with one.
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})

	// A UTF-8 character is at most four bytes, so only the last three can be the
	// start of an incomplete one.
	for i := len(b) - 1; i >= 0 && i >= len(b)-3; i-- {
		if utf8.RuneStart(b[i]) {
			if r, size := utf8.DecodeRune(b[i:]); r == utf8.RuneError && size <= 1 {
				return b[:i], b[i:]
			}
			break
		}
	}
	return b, nil
}

// utf16Decoder decodes UTF-16 to UTF-8, holding back a trailing odd byte or a
// lone surrogate.
func utf16Decoder(bigEndian bool) decoder {
	return func(b []byte) ([]byte, []byte) {
		if len(b) < 2 {
			return nil, b
		}

		var carry []byte
		if len(b)%2 == 1 {
			carry = b[len(b)-1:]
			b = b[:len(b)-1]
		}

		units := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if bigEndian {
				units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
			} else {
				units = append(units, uint16(b[i+1])<<8|uint16(b[i]))
			}
		}

		// As above: the mark belongs to the encoding, and a rotated log starts
		// with a fresh one.
		if len(units) > 0 && units[0] == 0xFEFF {
			units = units[1:]
		}

		// A surrogate pair split across two reads would otherwise decode as two
		// replacement characters, so the high half waits for its low half.
		if n := len(units); n > 0 && units[n-1] >= 0xD800 && units[n-1] <= 0xDBFF {
			unit := units[n-1]
			units = units[:n-1]
			if bigEndian {
				carry = append([]byte{byte(unit >> 8), byte(unit)}, carry...)
			} else {
				carry = append([]byte{byte(unit), byte(unit >> 8)}, carry...)
			}
		}

		return []byte(string(utf16.Decode(units))), carry
	}
}
