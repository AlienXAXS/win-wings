//go:build windows

package netstat

import (
	"sync"
	"time"
)

// Accountant attributes per-process byte counts to the servers that own the
// processes.
//
// Ownership is declared by Claim, which each server's environment calls with
// the process list its worker reports. Bytes are attributed at the moment they
// are observed, so a process that leaves a server's job takes nothing with it
// and a PID later reused by an unrelated process is not charged to the server.
//
// Traffic seen for a process nobody has claimed yet is held briefly. A server's
// first packets are sent before its worker has reported the process list, and
// dropping them would make every graph start with a hole. Unclaimed entries are
// swept once they go quiet; the whole host's traffic passes through here, and
// nothing must accumulate for processes that never turn out to be servers.
type Accountant struct {
	mu sync.Mutex

	// owner maps a PID to the server that currently claims it.
	owner map[uint32]string
	// claimed is the set of PIDs each server holds, so a Claim can release the
	// ones no longer reported without scanning owner.
	claimed map[string]map[uint32]struct{}
	// totals is what each server has been charged.
	totals map[string]*Counters

	pending   map[uint32]*pendingEntry
	lastSweep time.Time

	now func() time.Time
}

// Counters is a cumulative byte count in each direction.
type Counters struct {
	Rx uint64
	Tx uint64
}

func (c *Counters) add(o Counters) {
	c.Rx += o.Rx
	c.Tx += o.Tx
}

type pendingEntry struct {
	Counters
	seen time.Time
}

const (
	// pendingTTL is how long unclaimed traffic is remembered. It needs to cover
	// the gap between a server's first packet and its worker's first stats
	// report, which is one stats interval plus pipe latency; anything longer
	// only retains traffic for processes that are not servers.
	pendingTTL = 15 * time.Second
	// sweepInterval bounds how often the pending map is scanned. The scan runs
	// on the event path, so it must be rare relative to packet rate.
	sweepInterval = 5 * time.Second
)

// NewAccountant returns an empty accountant.
func NewAccountant() *Accountant {
	return &Accountant{
		owner:   make(map[uint32]string),
		claimed: make(map[string]map[uint32]struct{}),
		totals:  make(map[string]*Counters),
		pending: make(map[uint32]*pendingEntry),
		now:     time.Now,
	}
}

// Record charges bytes to whichever server owns pid, or holds them for a short
// while if none does yet. It is called once per packet, from the trace
// callback, and does nothing that could block.
func (a *Accountant) Record(pid uint32, c Counters) {
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()

	if owner, ok := a.owner[pid]; ok {
		a.totals[owner].add(c)
		return
	}

	p := a.pending[pid]
	if p == nil {
		p = &pendingEntry{}
		a.pending[pid] = p
	}
	p.add(c)
	p.seen = now

	if now.Sub(a.lastSweep) >= sweepInterval {
		a.lastSweep = now
		for pid, p := range a.pending {
			if now.Sub(p.seen) > pendingTTL {
				delete(a.pending, pid)
			}
		}
	}
}

// Claim declares that owner's job currently consists of exactly pids. PIDs it
// held before and no longer reports are released; ones it gains inherit any
// traffic seen for them while unclaimed.
//
// A PID already claimed by another owner is reassigned. Only one worker can
// hold a process in its job, so the older claim is stale.
func (a *Accountant) Claim(owner string, pids []uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.totals[owner]; !ok {
		a.totals[owner] = &Counters{}
	}
	held := a.claimed[owner]
	if held == nil {
		held = make(map[uint32]struct{})
		a.claimed[owner] = held
	}

	next := make(map[uint32]struct{}, len(pids))
	for _, pid := range pids {
		next[pid] = struct{}{}
		if a.owner[pid] == owner {
			continue
		}
		if prev, ok := a.owner[pid]; ok {
			delete(a.claimed[prev], pid)
		}
		a.owner[pid] = owner
		held[pid] = struct{}{}
		if p, ok := a.pending[pid]; ok {
			a.totals[owner].add(p.Counters)
			delete(a.pending, pid)
		}
	}
	for pid := range held {
		if _, ok := next[pid]; !ok {
			delete(held, pid)
			delete(a.owner, pid)
		}
	}
}

// Totals reports what owner has been charged so far.
func (a *Accountant) Totals(owner string) Counters {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.totals[owner]; ok {
		return *t
	}
	return Counters{}
}

// Reset zeroes owner's counters without releasing its processes. Called when
// a server starts, so the figures the Panel shows are for the current run, as
// a container's were.
func (a *Accountant) Reset(owner string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.totals[owner]; ok {
		*t = Counters{}
	}
}

// Release forgets owner entirely: its processes, its counters, everything.
func (a *Accountant) Release(owner string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for pid := range a.claimed[owner] {
		delete(a.owner, pid)
	}
	delete(a.claimed, owner)
	delete(a.totals, owner)
}
