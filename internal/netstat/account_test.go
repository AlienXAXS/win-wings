//go:build windows

package netstat

import (
	"testing"
	"time"
)

// clock is a settable time source for the accountant's sweep logic.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestAccountant() (*Accountant, *clock) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	a := NewAccountant()
	a.now = c.now
	a.lastSweep = c.t
	return a, c
}

func TestClaimedTrafficIsCharged(t *testing.T) {
	a, _ := newTestAccountant()
	a.Claim("srv", []uint32{100, 101})

	a.Record(100, Counters{Tx: 10})
	a.Record(101, Counters{Rx: 20})
	a.Record(102, Counters{Rx: 999}) // somebody else's

	if got := a.Totals("srv"); got != (Counters{Rx: 20, Tx: 10}) {
		t.Fatalf("totals = %+v, want rx=20 tx=10", got)
	}
}

func TestTrafficBeforeClaimIsFoldedIn(t *testing.T) {
	// The server's first packets go out before its worker has reported the
	// process list. They must not be lost.
	a, _ := newTestAccountant()
	a.Record(100, Counters{Tx: 7})
	a.Record(100, Counters{Rx: 3})

	a.Claim("srv", []uint32{100})
	if got := a.Totals("srv"); got != (Counters{Rx: 3, Tx: 7}) {
		t.Fatalf("totals after claim = %+v, want rx=3 tx=7", got)
	}

	// And only once.
	a.Claim("srv", []uint32{100})
	if got := a.Totals("srv"); got != (Counters{Rx: 3, Tx: 7}) {
		t.Fatalf("totals after repeat claim = %+v, want unchanged", got)
	}
}

func TestReleasedProcessTakesNothingAndReusedPIDIsNotCharged(t *testing.T) {
	a, _ := newTestAccountant()
	a.Claim("srv", []uint32{100, 200})
	a.Record(200, Counters{Tx: 50})

	// The helper at 200 exits; the worker's next report omits it.
	a.Claim("srv", []uint32{100})
	if got := a.Totals("srv"); got.Tx != 50 {
		t.Fatalf("bytes already charged should stay: %+v", got)
	}

	// Something unrelated is given PID 200 and talks. Not ours.
	a.Record(200, Counters{Tx: 1000})
	if got := a.Totals("srv"); got.Tx != 50 {
		t.Fatalf("reused PID was charged to the server: %+v", got)
	}
}

func TestClaimMovesPIDBetweenOwners(t *testing.T) {
	a, _ := newTestAccountant()
	a.Claim("a", []uint32{100})
	a.Claim("b", []uint32{100})
	a.Record(100, Counters{Rx: 5})

	if got := a.Totals("a"); got != (Counters{}) {
		t.Fatalf("stale owner charged: %+v", got)
	}
	if got := a.Totals("b"); got.Rx != 5 {
		t.Fatalf("new owner not charged: %+v", got)
	}

	// Releasing a's (now empty) claim set must not disturb b's ownership.
	a.Claim("a", nil)
	a.Record(100, Counters{Rx: 5})
	if got := a.Totals("b"); got.Rx != 10 {
		t.Fatalf("ownership lost after the previous owner re-claimed: %+v", got)
	}
}

func TestResetKeepsOwnership(t *testing.T) {
	a, _ := newTestAccountant()
	a.Claim("srv", []uint32{100})
	a.Record(100, Counters{Tx: 10})
	a.Reset("srv")
	if got := a.Totals("srv"); got != (Counters{}) {
		t.Fatalf("reset left %+v", got)
	}
	a.Record(100, Counters{Tx: 4})
	if got := a.Totals("srv"); got.Tx != 4 {
		t.Fatalf("ownership lost on reset: %+v", got)
	}
}

func TestReleaseForgetsEverything(t *testing.T) {
	a, _ := newTestAccountant()
	a.Claim("srv", []uint32{100})
	a.Record(100, Counters{Tx: 10})
	a.Release("srv")

	if got := a.Totals("srv"); got != (Counters{}) {
		t.Fatalf("totals survive release: %+v", got)
	}
	a.Record(100, Counters{Tx: 10})
	if _, owned := a.owner[100]; owned {
		t.Fatal("PID still owned after release")
	}
	if got := a.Totals("srv"); got != (Counters{}) {
		t.Fatalf("released owner charged: %+v", got)
	}
}

func TestUnclaimedTrafficIsSweptWhenQuiet(t *testing.T) {
	// Every process on the host flows through here. Entries for processes that
	// never become servers must age out, and the sweep must not touch ones
	// that are still talking.
	a, c := newTestAccountant()

	a.Record(1, Counters{Tx: 1}) // goes quiet
	c.advance(pendingTTL / 2)
	a.Record(2, Counters{Tx: 1}) // keeps talking

	c.advance(pendingTTL/2 + time.Second)
	a.Record(2, Counters{Tx: 1}) // triggers a sweep: 1 is stale, 2 is fresh

	if _, ok := a.pending[1]; ok {
		t.Fatal("stale unclaimed entry was not swept")
	}
	if _, ok := a.pending[2]; !ok {
		t.Fatal("live unclaimed entry was swept")
	}

	// A claim arriving late gets nothing; the traffic is gone, correctly.
	a.Claim("srv", []uint32{1})
	if got := a.Totals("srv"); got != (Counters{}) {
		t.Fatalf("swept traffic resurfaced: %+v", got)
	}
}

func TestSweepIsRateLimited(t *testing.T) {
	// The sweep scans the whole pending map on the packet path, so it must
	// not run on every packet: an entry that goes stale shortly after a sweep
	// survives until the next one is due.
	a, c := newTestAccountant()

	a.Record(1, Counters{Tx: 1})
	c.advance(pendingTTL - 3*time.Second)
	a.Record(2, Counters{Tx: 1}) // sweeps; 1 is not yet stale

	c.advance(4 * time.Second) // 1 is now stale, but the sweep is not due
	a.Record(3, Counters{Tx: 1})
	if _, ok := a.pending[1]; !ok {
		t.Fatal("swept inside the interval")
	}

	c.advance(sweepInterval)
	a.Record(3, Counters{Tx: 1})
	if _, ok := a.pending[1]; ok {
		t.Fatal("not swept once the interval had passed")
	}
}
