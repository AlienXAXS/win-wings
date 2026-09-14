//go:build windows

package hoststats

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCPUTimesAreMonotonic(t *testing.T) {
	t1, b1, err := cpuTimes()
	if err != nil {
		t.Fatal(err)
	}
	if t1 == 0 || b1 > t1 {
		t.Fatalf("implausible counters: total=%d busy=%d", t1, b1)
	}
	// Burn a little CPU so the second read is strictly later.
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
	}
	t2, b2, err := cpuTimes()
	if err != nil {
		t.Fatal(err)
	}
	if t2 < t1 || b2 < b1 {
		t.Fatalf("counters went backwards: %d->%d busy %d->%d", t1, t2, b1, b2)
	}
}

func TestMemoryIsPlausible(t *testing.T) {
	m := memory()
	if m.TotalMB < 256 {
		t.Fatalf("total memory %d MB is implausible", m.TotalMB)
	}
	if m.AvailableMB <= 0 || m.AvailableMB > m.TotalMB {
		t.Fatalf("available %d MB out of range for total %d MB", m.AvailableMB, m.TotalMB)
	}
	if m.UsedMB != m.TotalMB-m.AvailableMB {
		t.Fatalf("used %d != total %d - available %d", m.UsedMB, m.TotalMB, m.AvailableMB)
	}
}

func TestDiskWalksUpToAnExistingDirectory(t *testing.T) {
	// A directory that does not exist yet, beneath one that does: the freshly
	// configured node case.
	missing := filepath.Join(t.TempDir(), "servers", "not-yet")
	d := disk(missing)
	if d == nil {
		t.Fatal("expected the parent volume to be measured")
	}
	if d.TotalMB <= 0 || d.AvailableMB < 0 || d.AvailableMB > d.TotalMB {
		t.Fatalf("implausible disk figures: %+v", *d)
	}

	if disk("") != nil {
		t.Fatal("empty path must disable the disk block")
	}
	// A volume letter nothing has.
	if got := disk(`Q:\nope\nope`); got != nil {
		// Only a failure if Q: genuinely does not exist on this host.
		if _, err := os.Stat(`Q:\`); os.IsNotExist(err) {
			t.Fatalf("expected nil for a non-existent volume, got %+v", *got)
		}
	}
}

func TestReportBeforeAndAfterSecondSample(t *testing.T) {
	c := New(os.TempDir(), "test")
	r := c.Report()
	if r.Version != ReportVersion || r.Agent != "test" {
		t.Fatalf("bad header: %+v", r)
	}
	if r.CPU.Cores != runtime.NumCPU() {
		t.Fatalf("cores %d != %d", r.CPU.Cores, runtime.NumCPU())
	}
	if r.CPU.UtilisationPercent != nil {
		t.Fatal("utilisation must be nil with a single sample")
	}
	if r.CPU.Percent != r.CPU.LoadPercent {
		t.Fatal("percent must fall back to the load figure before utilisation exists")
	}
	if r.Disk == nil {
		t.Fatal("expected a disk block for the temp directory")
	}

	// Force a second sample without waiting out the interval.
	time.Sleep(20 * time.Millisecond)
	c.sample()
	r = c.Report()
	if r.CPU.UtilisationPercent == nil {
		t.Fatal("utilisation must be present after two samples")
	}
	if u := *r.CPU.UtilisationPercent; u < 0 || u > 100 || r.CPU.Percent != u {
		t.Fatalf("utilisation %d out of range or not authoritative (percent=%d)", u, r.CPU.Percent)
	}
	if r.CPU.Load1 < 0 || r.CPU.Load5 < 0 || r.CPU.Load15 < 0 {
		t.Fatalf("negative load: %+v", r.CPU)
	}
}

func TestWindowIsTrimmed(t *testing.T) {
	c := New("", "test")
	old := time.Now().Add(-(Window + 2*SampleInterval))
	c.mu.Lock()
	c.samples = []cpuSample{{at: old, total: 1, busy: 1}}
	c.mu.Unlock()
	c.sample()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.samples) != 1 || c.samples[0].at.Equal(old) {
		t.Fatalf("stale sample not trimmed: %+v", c.samples)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	c := New("", "test")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queue != nil {
		t.Fatal("PDH query not closed on shutdown")
	}
}

// The JSON shape is the contract with the Panel; a renamed field breaks stock
// calculation silently, so it is pinned here.
func TestReportJSONShape(t *testing.T) {
	c := New(os.TempDir(), "test")
	b, err := json.Marshal(c.Report())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "collected_at", "cpu", "memory", "disk"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing top-level key %q in %s", k, b)
		}
	}
	cpu := m["cpu"].(map[string]any)
	for _, k := range []string{"cores", "load_1m", "load_5m", "load_15m", "percent", "utilisation_percent", "load_percent"} {
		if _, ok := cpu[k]; !ok {
			t.Fatalf("missing cpu key %q in %s", k, b)
		}
	}
	for _, blk := range []string{"memory", "disk"} {
		u := m[blk].(map[string]any)
		for _, k := range []string{"total_mb", "available_mb", "used_mb"} {
			if _, ok := u[k]; !ok {
				t.Fatalf("missing %s key %q in %s", blk, k, b)
			}
		}
	}
}

func TestQueueCounterReads(t *testing.T) {
	q := openQueueCounter()
	if q == nil {
		t.Skip("PDH processor queue counter unavailable on this host")
	}
	defer q.close()
	v, err := q.read()
	if err != nil {
		t.Fatal(err)
	}
	if v < 0 || v > 100000 {
		t.Fatalf("implausible queue length %v", v)
	}
}
