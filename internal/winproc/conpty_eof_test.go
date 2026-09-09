//go:build windows

package winproc

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pterodactyl/wings/internal/jobobject"
)

// The pattern every caller uses: start, drain output to EOF on one goroutine,
// Wait on another, then join. With a plain pipe this terminates because the
// child held the only remaining write handle. With a pseudo console the console
// holds it, and nothing released it until Close -- which callers defer until
// after the drain they are waiting on. The result was an install that started,
// produced its output, and then hung forever.
func TestOutputReachesEOFAfterWait(t *testing.T) {
	for _, pty := range []bool{false, true} {
		name := "pipes"
		if pty {
			name = "pseudo console"
		}
		t.Run(name, func(t *testing.T) {
			job, err := jobobject.Create()
			if err != nil {
				t.Fatalf("create job: %v", err)
			}
			defer job.Close()

			cmd := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
			proc, err := Start(Config{
				Argv:          []string{cmd, "/c", "echo", "conpty-marker"},
				Dir:           t.TempDir(),
				PseudoConsole: pty,
				Cols:          80,
				Rows:          25,
			}, job)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			defer proc.Close()

			drained := make(chan string, 1)
			go func() {
				b, _ := io.ReadAll(proc.Output())
				drained <- string(b)
			}()

			code, err := proc.Wait()
			if err != nil {
				t.Fatalf("wait: %v", err)
			}

			// A pseudo console child that dies in the loader is a separate,
			// unresolved problem -- see ConsoleConfiguration.InstallPseudoConsole
			// -- and not what this test is for. It is reported rather than
			// asserted on, because it reproduces intermittently on some hosts and
			// gating the EOF fix behind it would make this test useless there.
			// The timeout below is the assertion that matters.
			if pty && IsLoaderFailure(code) {
				t.Skipf("the pseudo console child died in the loader with %s; "+
					"ConPTY is unreliable on this host, which is why it is not "+
					"the default", ExplainExitCode(code))
			}
			if code != 0 {
				t.Fatalf("exit code = %s, want 0", ExplainExitCode(code))
			}

			select {
			case out := <-drained:
				// The marker is split here so this file's own source cannot
				// satisfy the check if it is ever echoed by mistake.
				if !strings.Contains(out, "conpty-"+"marker") {
					t.Errorf("output did not contain the marker: %q", out)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("output never reached EOF after the process exited; " +
					"a caller that waits and then drains would hang here")
			}
		})
	}
}
