//go:build windows

package worker

import (
	"os"
	"path/filepath"
	"sync"
)

// consoleBuffer retains recent console output so a reconnecting daemon can
// recover what it missed, and mirrors everything to a rotating log file.
//
// The ring is bounded by chunk count rather than bytes so that a server
// producing a flood of output cannot grow the worker's memory without limit.
type consoleBuffer struct {
	mu sync.Mutex

	chunks   []consoleChunk
	capacity int
	sequence uint64

	logPath  string
	log      *os.File
	logSize  int64
	maxSize  int64
	maxFiles int
}

type consoleChunk struct {
	seq  uint64
	data []byte
}

func newConsoleBuffer(logPath string, capacity int, maxSizeMB int64, maxFiles int) *consoleBuffer {
	if capacity <= 0 {
		capacity = 512
	}
	if maxSizeMB <= 0 {
		maxSizeMB = 5
	}
	if maxFiles < 0 {
		maxFiles = 0
	}
	return &consoleBuffer{
		chunks:   make([]consoleChunk, 0, capacity),
		capacity: capacity,
		logPath:  logPath,
		maxSize:  maxSizeMB * 1024 * 1024,
		maxFiles: maxFiles,
	}
}

// Append records a chunk and returns the sequence number assigned to it.
func (c *consoleBuffer) Append(data []byte) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sequence++
	cp := make([]byte, len(data))
	copy(cp, data)

	if len(c.chunks) == c.capacity {
		copy(c.chunks, c.chunks[1:])
		c.chunks[len(c.chunks)-1] = consoleChunk{seq: c.sequence, data: cp}
	} else {
		c.chunks = append(c.chunks, consoleChunk{seq: c.sequence, data: cp})
	}

	c.writeLog(cp)
	return c.sequence
}

// Since returns retained chunks with a sequence greater than seq.
func (c *consoleBuffer) Since(seq uint64) []consoleChunk {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []consoleChunk
	for _, ch := range c.chunks {
		if ch.seq > seq {
			out = append(out, ch)
		}
	}
	return out
}

// Sequence returns the most recently assigned sequence number.
func (c *consoleBuffer) Sequence() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sequence
}

// writeLog mirrors a chunk to the console log, rotating when it grows past the
// configured size. Caller must hold the lock.
//
// Logging failures are deliberately swallowed: losing console history is not a
// reason to take a running game server down with it.
func (c *consoleBuffer) writeLog(data []byte) {
	if c.logPath == "" {
		return
	}
	if c.log == nil {
		if err := os.MkdirAll(filepath.Dir(c.logPath), 0o700); err != nil {
			return
		}
		f, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		if st, err := f.Stat(); err == nil {
			c.logSize = st.Size()
		}
		c.log = f
	}

	n, err := c.log.Write(data)
	if err != nil {
		return
	}
	c.logSize += int64(n)

	if c.logSize >= c.maxSize {
		c.rotate()
	}
}

// rotate closes the current log and shifts the retained history. Caller must
// hold the lock.
func (c *consoleBuffer) rotate() {
	_ = c.log.Close()
	c.log = nil
	c.logSize = 0

	if c.maxFiles <= 0 {
		_ = os.Remove(c.logPath)
		return
	}

	// Drop the oldest, then shift each remaining file up one slot.
	oldest := c.logPath + "." + itoa(c.maxFiles)
	_ = os.Remove(oldest)
	for i := c.maxFiles - 1; i >= 1; i-- {
		_ = os.Rename(c.logPath+"."+itoa(i), c.logPath+"."+itoa(i+1))
	}
	_ = os.Rename(c.logPath, c.logPath+".1")
}

// Close releases the log file.
func (c *consoleBuffer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.log != nil {
		err := c.log.Close()
		c.log = nil
		return err
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
