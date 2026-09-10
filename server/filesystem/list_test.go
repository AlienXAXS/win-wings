package filesystem

import (
	"path/filepath"
	"testing"

	. "github.com/franela/goblin"
	"golang.org/x/sys/windows"
)

func TestFilesystem_ListDirectory(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("ListDirectory", func() {
		g.AfterEach(func() {
			_ = fs.TruncateRootDirectory()
		})

		g.It("lists folders first and detects mimetypes", func() {
			g.Assert(rfs.CreateServerFileFromString("b.txt", "hello world\n")).IsNil()
			g.Assert(rfs.CreateServerFileFromString("a.txt", "hello again\n")).IsNil()
			g.Assert(fs.CreateDirectory("zdir", "/")).IsNil()

			out, err := fs.ListDirectory("/")
			g.Assert(err).IsNil()
			g.Assert(len(out)).Equal(3)
			g.Assert(out[0].Name()).Equal("zdir")
			g.Assert(out[0].Mimetype).Equal("inode/directory")
			g.Assert(out[1].Name()).Equal("a.txt")
			g.Assert(out[1].Mimetype).Equal("text/plain; charset=utf-8")
			g.Assert(out[2].Name()).Equal("b.txt")
		})

		g.It("still lists a file another process holds without read sharing", func() {
			g.Assert(rfs.CreateServerFileFromString("locked.log", "in use\n")).IsNil()

			// Hold the file with no sharing at all, as a running game server
			// commonly does with its log. Any attempt to open it for reading
			// now fails with a sharing violation.
			p := filepath.Join(fs.Path(), "locked.log")
			h, err := windows.CreateFile(windows.StringToUTF16Ptr(p),
				windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
			g.Assert(err).IsNil()
			defer windows.CloseHandle(h)

			out, err := fs.ListDirectory("/")
			g.Assert(err).IsNil()
			g.Assert(len(out)).Equal(1)
			g.Assert(out[0].Name()).Equal("locked.log")
			g.Assert(out[0].Mimetype).Equal("application/octet-stream")
		})
	})
}
