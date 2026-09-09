//go:build windows

package winfs

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// TestEscapeErrorText guards the one piece of this package that depends on an
// implementation detail of the standard library.
//
// os.Root signals a containment failure with an unexported error that matches
// none of the io/fs sentinels, so convertError has to recognise it by message.
// If a Go upgrade rewords it, every sandbox escape would silently stop being
// reported as ErrBadPathResolution and would surface to API and SFTP clients as
// an opaque error instead. Fail here rather than there.
func TestEscapeErrorText(t *testing.T) {
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer r.Close()

	_, err = r.Open("../escape.txt")
	if err == nil {
		t.Fatal("expected an error opening a path outside the root")
	}

	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("expected a *fs.PathError, got %T: %v", err, err)
	}

	if pe.Err.Error() != escapeErrText {
		t.Fatalf("os.Root escape message changed: got %q, escapeErrText is %q.\n"+
			"Update escapeErrText in types.go to match, or convertError will stop "+
			"mapping escapes to ErrBadPathResolution.", pe.Err.Error(), escapeErrText)
	}

	// Also confirm the assumption that drove message matching in the first place:
	// the error is not reachable through any io/fs sentinel.
	for name, sentinel := range map[string]error{
		"ErrNotExist":   fs.ErrNotExist,
		"ErrInvalid":    fs.ErrInvalid,
		"ErrPermission": fs.ErrPermission,
		"ErrExist":      fs.ErrExist,
	} {
		if errors.Is(err, sentinel) {
			t.Errorf("os.Root escape is now reachable via errors.Is(err, %s) — "+
				"prefer that over matching the message", name)
		}
	}
}

// TestConvertErrorMapsEscape checks the mapping end to end.
func TestConvertErrorMapsEscape(t *testing.T) {
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer r.Close()

	_, err = r.Open("../escape.txt")
	if got := convertError(err); !errors.Is(got, ErrBadPathResolution) {
		t.Fatalf("convertError did not map an escape to ErrBadPathResolution: %v", got)
	}

	// A genuine not-found must not be reported as an escape.
	_, err = r.Open("nope.txt")
	if got := convertError(err); errors.Is(got, ErrBadPathResolution) {
		t.Fatalf("convertError wrongly mapped a missing file to ErrBadPathResolution: %v", got)
	} else if !errors.Is(got, ErrNotExist) {
		t.Fatalf("expected ErrNotExist for a missing file, got %v", got)
	}
}
