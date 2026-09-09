package winproc

import (
	"strings"
	"testing"
)

// TestExplainExitCodeNamesLoaderFailures covers the codes that send people to
// the wrong place.
//
// 3221225794 in a log reads as a number the script chose. It is 0xC0000142, and
// it means the script never ran at all -- so the install output above it is not
// evidence of anything and the fault is in the environment the daemon supplied.
func TestExplainExitCodeNamesLoaderFailures(t *testing.T) {
	got := ExplainExitCode(3221225794)
	for _, want := range []string{"0xC0000142", "STATUS_DLL_INIT_FAILED", "SystemRoot"} {
		if !strings.Contains(got, want) {
			t.Errorf("ExplainExitCode(3221225794) = %q, missing %q", got, want)
		}
	}
}

func TestExplainExitCodeLeavesOrdinaryCodesAlone(t *testing.T) {
	// A script's own exit 1 is not a Windows condition and must not be dressed
	// up as one.
	if got := ExplainExitCode(1); got != "1" {
		t.Errorf("ExplainExitCode(1) = %q, want %q", got, "1")
	}
	if got := ExplainExitCode(0); got != "0" {
		t.Errorf("ExplainExitCode(0) = %q, want %q", got, "0")
	}
}

func TestExplainExitCodeFallsBackForUnknownNTStatus(t *testing.T) {
	got := ExplainExitCode(0xC0000123)
	if !strings.Contains(got, "0xC0000123") || !strings.Contains(got, "operating system") {
		t.Errorf("an unlisted NTSTATUS should still be identified as one, got %q", got)
	}
}
