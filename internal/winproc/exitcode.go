//go:build windows

package winproc

import "fmt"

// ExplainExitCode renders a process exit code, naming it when it is one of the
// NTSTATUS values Windows uses to report that a process never really started.
//
// This exists because those codes are reported in decimal and are meaningless in
// that form. A script that failed before its first line produces "exited with a
// non-zero status: 3221225794", which reads like the script chose to exit 3.2
// billion; in hexadecimal it is 0xC0000142, a documented condition with a small
// set of causes.
//
// The distinction matters for where to look. A code in this table means the
// process was created and then died in the loader, so nothing in the script ran
// and nothing in its output is relevant — the fault is in the environment the
// daemon supplied, not in the egg.
func ExplainExitCode(code uint32) string {
	if hint, ok := ntStatusHints[code]; ok {
		return fmt.Sprintf("%d (0x%08X, %s)", code, code, hint)
	}
	// Anything at or above 0xC0000000 is an NTSTATUS error the loader or a
	// hardware fault produced, rather than a value the program chose.
	if code >= 0xC0000000 {
		return fmt.Sprintf("%d (0x%08X, an unhandled Windows exception -- the process was "+
			"terminated by the operating system rather than exiting on its own)", code, code)
	}
	return fmt.Sprintf("%d", code)
}

// IsLoaderFailure reports whether an exit code means the process never ran.
//
// Any check that expects a process to fail has to distinguish the failure it
// asked for from the process not starting, or it passes on a broken host. The
// codes here are exactly the ones ExplainExitCode describes as the loader giving
// up before the program's first instruction.
func IsLoaderFailure(code uint32) bool {
	switch code {
	case 0xC0000142, 0xC0000135, 0xC0000139, 0xC000007B:
		return true
	}
	return false
}

// ntStatusHints covers the codes actually seen when launching a process under
// another account, each with the cause worth checking first rather than a
// transcription of the NTSTATUS name.
var ntStatusHints = map[uint32]string{
	0xC0000142: "STATUS_DLL_INIT_FAILED: a DLL failed to initialise, so the program never " +
		"ran. Usually an incomplete environment -- SystemRoot in particular, without which " +
		"PowerShell and anything .NET cannot start -- or the account being unable to reach " +
		"the window station it was launched on",
	0xC0000135: "STATUS_DLL_NOT_FOUND: a DLL the program needs is missing. For a game " +
		"server this is nearly always a Visual C++ redistributable; scripts\\provision-host.ps1 " +
		"installs every generation",
	0xC0000139: "STATUS_ENTRYPOINT_NOT_FOUND: a DLL was found but is the wrong version",
	0xC0000005: "STATUS_ACCESS_VIOLATION: the process crashed",
	0xC000007B: "STATUS_INVALID_IMAGE_FORMAT: a 32-bit process tried to load a 64-bit DLL " +
		"or the reverse",
	0xC0000409: "STATUS_STACK_BUFFER_OVERRUN: the process aborted itself on a corrupted stack",
	0xC000013A: "STATUS_CONTROL_C_EXIT: the process was interrupted",
	0xC0000017: "STATUS_NO_MEMORY: an allocation failed, which inside a Job Object usually " +
		"means the server's memory limit was reached",
}
