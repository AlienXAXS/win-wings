package filesystem

import (
	"syscall"
	"time"
)

// CTime returns the time the file was created.
//
// Upstream carried a TODO noting that its Linux implementation read Ctim, which
// is the inode change time rather than a creation time, and was never actually
// correct. Windows records a real creation time in the same attribute data that
// os.Stat already returns, so this is accurate here without any extra syscall.
func (s *Stat) CTime() time.Time {
	if st, ok := s.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, st.CreationTime.Nanoseconds())
	}
	return time.Time{}
}
