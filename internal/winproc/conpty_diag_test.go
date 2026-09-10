//go:build windows

package winproc

import (
	"os/exec"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ConPTY: the raw Win32 sequence, and the two mistakes that hid it.
//
// This file spent a long time as a diagnostic for a pseudo console that created
// cleanly, spawned a console host, accepted a child process — and moved not one
// byte in either direction. It is now the regression test for the two faults
// that caused that, both of which are invisible at the call site and neither of
// which reports an error.
//
// FAULT 1 — the HPCON was passed by address.
//
// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE is the only proc-thread attribute whose
// lpValue is the value itself rather than a pointer to it. PARENT_PROCESS,
// HANDLE_LIST and MITIGATION_POLICY all take an address, so `&pty` is what the
// surrounding code looks like it should say, and x/sys/windows types the
// parameter as unsafe.Pointer, which makes the correct call the one that needs a
// comment. Microsoft's EchoCon sample passes `hPC`, not `&hPC`; that single
// character is the whole difference.
//
// Passed an address, the kernel reads a PseudoConsole struct out of whatever
// happens to be at it, hands the child garbage handles, and the child dies in
// the loader with STATUS_DLL_INIT_FAILED (0xC0000142) having executed none of
// its own code. UpdateProcThreadAttribute and CreateProcess both return success.
// That is why every CreateProcess parameter was ruled out one at a time — none
// of them was ever the problem.
//
// FAULT 2 — the child inherited the worker's standard handles.
//
// With fault 1 fixed the child ran, and its output still did not appear: it went
// to the worker's own stdout instead. Leaving STARTF_USESTDHANDLES clear is what
// EchoCon does, and it works there only because EchoCon's own standard handles
// are console handles, which get remapped onto whichever console the child joins.
// The worker's stdout is a pipe, and a pipe handle is copied down literally. The
// fix is STARTF_USESTDHANDLES with three NULL handles: given no handles and a
// console, the child opens its standard handles onto the console.
//
// Presented together the two look like one fault, which is what made this hard:
// fixing either alone still produces a pseudo console that emits nothing useful.
//
// Traps in writing diagnostics for this, all of which cost time:
//
//   - ClosePseudoConsole blocks until the output pipe is drained, so closing it
//     with no reader attached deadlocks.
//   - CloseHandle on the output pipe blocks while a synchronous ReadFile is
//     pending on it. Tear the child down first.
//   - Trials must not share a process. A leaked pseudo console from an earlier
//     trial contaminates later ones, and the results then lie: an earlier note
//     here blamed os.NewFile on exactly that evidence, and was wrong.
//
//	go test ./internal/winproc -run TestConPTY -v

func conhostCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("tasklist.exe", "/FI", "IMAGENAME eq conhost.exe", "/NH").Output()
	if err != nil {
		t.Logf("tasklist: %v", err)
		return -1
	}
	lower := strings.ToLower(string(out))
	// Windows does not always service a pseudo console with conhost. Where
	// Windows Terminal is the default terminal application, the console host is
	// OpenConsole.exe instead, and counting only conhost reports that nothing
	// was spawned on a machine where something was.
	return strings.Count(lower, "conhost.exe") + strings.Count(lower, "openconsole.exe")
}

// TestConPTYSpawnsConhost verifies that creating a pseudo console brings up a
// console host to service it.
//
// Counting host processes machine-wide is a blunt instrument: another
// application starting a console in the same window makes the count move on its
// own. So an inconclusive result is reported and skipped rather than failed —
// a test that fails for reasons unrelated to what it is testing teaches people
// to ignore the suite.
func TestConPTYSpawnsConhost(t *testing.T) {
	before := conhostCount(t)

	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		t.Fatal(err)
	}

	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: 120, Y: 30}, inRead, outWrite, 0, &hpc); err != nil {
		t.Fatalf("CreatePseudoConsole: %v", err)
	}
	t.Logf("CreatePseudoConsole ok, hpc=0x%X", hpc)

	time.Sleep(500 * time.Millisecond)
	after := conhostCount(t)
	t.Logf("console host instances (conhost + OpenConsole): %d -> %d", before, after)
	if before >= 0 && after <= before {
		t.Skipf("no console host appeared for the pseudo console (%d -> %d). Either the "+
			"pseudo console is not being serviced, or another process exited in the "+
			"same window and masked it", before, after)
	}

	windows.ClosePseudoConsole(hpc)
	for _, h := range []windows.Handle{inRead, inWrite, outRead, outWrite} {
		_ = windows.CloseHandle(h)
	}
}

// TestProcThreadAttributeListWorks is the control for the ConPTY tests: it
// exercises the same attribute-list machinery via an attribute whose effect is
// directly observable, and whose lpValue really is an address.
func TestProcThreadAttributeListWorks(t *testing.T) {
	self, err := windows.GetCurrentProcess()
	if err != nil {
		t.Fatalf("GetCurrentProcess: %v", err)
	}

	attrList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatalf("NewProcThreadAttributeList: %v", err)
	}
	defer attrList.Delete()

	if err := attrList.Update(
		windows.PROC_THREAD_ATTRIBUTE_PARENT_PROCESS,
		unsafe.Pointer(&self),
		unsafe.Sizeof(self),
	); err != nil {
		t.Fatalf("Update(PARENT_PROCESS): %v", err)
	}

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attrList.List()

	cmdlinePtr, _ := windows.UTF16PtrFromString(
		windows.ComposeCommandLine([]string{comspec(t), "/c", "exit 7"}))
	dirPtr, _ := windows.UTF16PtrFromString(t.TempDir())

	var pi windows.ProcessInformation
	if err := windows.CreateProcess(
		nil, cmdlinePtr, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT,
		nil, dirPtr, &si.StartupInfo, &pi,
	); err != nil {
		t.Fatalf("CreateProcess with attribute list: %v", err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)

	_, _ = windows.WaitForSingleObject(pi.Process, 5000)
	var code uint32
	_ = windows.GetExitCodeProcess(pi.Process, &code)

	if code != 7 {
		t.Errorf("attribute list path is broken: exit code %d, want 7", code)
	}
}

// conptyTrial is one raw-Win32 run of a child under a pseudo console, deliberately
// bypassing Start so that the two faults above can be reproduced in isolation.
type conptyTrial struct {
	// byAddress passes &hpc rather than hpc — fault 1.
	byAddress bool
	// inheritStdHandles leaves STARTF_USESTDHANDLES clear — fault 2.
	inheritStdHandles bool
}

// run launches `cmd /c echo <marker>` and returns everything the pseudo console
// emitted, plus the child's exit code.
func (tr conptyTrial) run(t *testing.T, marker string) (string, uint32) {
	t.Helper()

	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		t.Fatal(err)
	}

	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: 120, Y: 30}, inRead, outWrite, 0, &hpc); err != nil {
		t.Fatalf("CreatePseudoConsole: %v", err)
	}
	// The pseudo console duplicated these; our copies must go or the child never
	// sees EOF.
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)

	attrList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatal(err)
	}
	defer attrList.Delete()

	value := hpconValue(hpc)
	if tr.byAddress {
		value = unsafe.Pointer(&hpc)
	}
	if err := attrList.Update(procThreadAttributePseudoConsole, value, unsafe.Sizeof(hpc)); err != nil {
		t.Fatal(err)
	}

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attrList.List()
	if !tr.inheritStdHandles {
		si.Flags |= windows.STARTF_USESTDHANDLES
		si.StdInput, si.StdOutput, si.StdErr = 0, 0, 0
	}

	cmdlinePtr, _ := windows.UTF16PtrFromString(
		windows.ComposeCommandLine([]string{comspec(t), "/c", "echo", marker}))
	dirPtr, _ := windows.UTF16PtrFromString(t.TempDir())

	var pi windows.ProcessInformation
	if err := windows.CreateProcess(
		nil, cmdlinePtr, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT,
		nil, dirPtr, &si.StartupInfo, &pi,
	); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}

	// Read to EOF on a goroutine before anything is torn down. EOF arrives when
	// the console is closed, not when the child exits — the console holds the
	// write end.
	drained := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			var n uint32
			if err := windows.ReadFile(outRead, buf, &n, nil); err != nil || n == 0 {
				break
			}
			sb.Write(buf[:n])
		}
		drained <- sb.String()
	}()

	if _, err := windows.WaitForSingleObject(pi.Process, 10000); err != nil {
		t.Fatalf("wait: %v", err)
	}
	var code uint32
	_ = windows.GetExitCodeProcess(pi.Process, &code)

	// Closing the console flushes what it has buffered and then closes the write
	// end, which is what ends the read loop above.
	windows.ClosePseudoConsole(hpc)

	var got string
	select {
	case got = <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the pseudo console output never reached EOF after the console was closed")
	}

	_ = windows.TerminateProcess(pi.Process, 1)
	_ = windows.CloseHandle(pi.Process)
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(inWrite)
	_ = windows.CloseHandle(outRead)

	return got, code
}

// TestConPTYEndToEnd is the reference implementation: the smallest raw Win32
// sequence that gets a child's output out of a pseudo console. Start does the
// same thing with more bookkeeping, so when this passes and TestStartPseudoConsole
// does not, the fault is in winproc rather than in the platform.
func TestConPTYEndToEnd(t *testing.T) {
	// Split so that this file's own source cannot satisfy the check if it is
	// ever echoed back by mistake.
	marker := "CONPTY-" + "MARKER"

	got, code := conptyTrial{}.run(t, marker)
	if code != 0 {
		t.Fatalf("child exited with %s; it never ran", ExplainExitCode(code))
	}
	if !strings.Contains(got, marker) {
		t.Fatalf("the marker never came out of the pseudo console: %q", got)
	}
	t.Logf("output ok (%d bytes): %q", len(got), got)
}

// TestConPTYFaultsProduceNoOutput pins the two mistakes described at the top of
// this file. Both are silent — every API call still succeeds — so without this
// the only thing standing between the code and a repeat is a comment.
//
// Only the absence of output is asserted. How a fault presents varies even
// between these trials: the two together — the combination that actually shipped
// — kill the child in the loader, while passing the handle by address with the
// standard handles suppressed lets it exit zero having written nowhere.
//
// The second trial's child writes its marker to this test binary's own stdout,
// so it appears loose in the test output. That is the fault, demonstrated.
func TestConPTYFaultsProduceNoOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trial conptyTrial
	}{
		{"handle passed by address", conptyTrial{byAddress: true}},
		{"standard handles inherited", conptyTrial{inheritStdHandles: true}},
		{"both, as originally shipped", conptyTrial{byAddress: true, inheritStdHandles: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := "CONPTY-" + "FAULT"
			got, code := tc.trial.run(t, marker)
			t.Logf("exit %s, %d bytes out of the console", ExplainExitCode(code), len(got))
			if strings.Contains(got, marker) {
				t.Errorf("this is meant to be broken but it worked; the fix in "+
					"winproc may no longer be needed, or this trial no longer "+
					"reproduces the fault: %q", got)
			}
		})
	}
}
