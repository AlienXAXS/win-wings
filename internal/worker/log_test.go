//go:build windows

package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pterodactyl/wings/internal/wire"
)

func TestPairsIgnoresAnOddTrailingArgument(t *testing.T) {
	// A logging call must never take a server down, so a miscounted argument
	// list is dropped rather than panicking or producing a key with no value.
	got := pairs([]any{"pid", 42, "orphan"})
	if len(got) != 1 || got["pid"] != "42" {
		t.Fatalf("pairs = %v, want {pid: 42}", got)
	}
}

func TestPairsWithNoFields(t *testing.T) {
	if got := pairs(nil); got != nil {
		t.Errorf("pairs(nil) = %v, want nil", got)
	}
	if got := pairs([]any{"lonely"}); got != nil {
		t.Errorf("pairs with one argument = %v, want nil", got)
	}
}

func TestFormatFieldsIsOrdered(t *testing.T) {
	// Stable ordering so two runs of the same code produce comparable lines;
	// Go randomises map iteration, so this needs the explicit sort.
	f := map[string]string{"zeta": "3", "alpha": "1", "mid": "2"}
	if got, want := formatFields(f), " alpha=1 mid=2 zeta=3"; got != want {
		t.Errorf("formatFields = %q, want %q", got, want)
	}
}

func TestAccountOrSelfNamesTheUnisolatedCase(t *testing.T) {
	if got := accountOrSelf("ww-abc"); got != "ww-abc" {
		t.Errorf("accountOrSelf = %q, want the account name", got)
	}
	// The empty case must not render as an empty string in a log line: running
	// without isolation is the fact worth seeing.
	if got := accountOrSelf(""); !strings.Contains(got, "no isolation") {
		t.Errorf("accountOrSelf(\"\") = %q, want it to name the lack of isolation", got)
	}
}

func TestLogRoundTripsOverTheWire(t *testing.T) {
	b, err := wire.Marshal(wire.TypeLog, 0, wire.Log{
		Level:   wire.LogWarn,
		Message: "could not grant a desktop",
		Fields:  map[string]string{"account": "ww-abc"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var env wire.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if env.Type != wire.TypeLog {
		t.Fatalf("type = %q, want %q", env.Type, wire.TypeLog)
	}

	var got wire.Log
	if err := wire.Unmarshal(env, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Level != wire.LogWarn || got.Message != "could not grant a desktop" ||
		got.Fields["account"] != "ww-abc" {
		t.Errorf("round trip = %+v", got)
	}
}
