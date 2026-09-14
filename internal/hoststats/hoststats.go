//go:build windows

// Package hoststats measures the host's CPU, memory and disk for the node load
// agent.
//
// The figures are the ones the Panel's Free Servers extension already consumes
// from its Linux agent, which reads /proc/stat, /proc/loadavg, /proc/meminfo and
// statfs. Windows has direct equivalents for three of the four:
//
//	/proc/stat       GetSystemTimes — idle, kernel and user time since boot
//	/proc/meminfo    GlobalMemoryStatusEx — total and available physical memory
//	statfs           GetDiskFreeSpaceEx — on the server volumes directory
//
// It has no load average. That is a Linux (and BSD) idea: an exponentially
// damped count of threads that are running or waiting to run, over the last 1,
// 5 and 15 minutes. It is synthesised here from the same inputs the kernel uses
// — the number of CPUs busy, plus the length of the processor ready queue —
// with the same decay constants, so the numbers mean what an operator expects
// them to mean. The Panel only stores load_1m for display; stock is sized from
// utilisation, which is measured directly.
package hoststats

import (
	"context"
	"math"
	"sync"
	"time"
)

// SampleInterval is how often the CPU is read. The Linux agent samples every
// five seconds and reports utilisation over roughly the last minute; the same
// here so the two are comparable on a mixed fleet.
const SampleInterval = 5 * time.Second

// Window is how far back utilisation is measured.
const Window = time.Minute

// Load average decay per sample, as the Linux kernel computes them:
// exp(-interval / period).
var (
	decay1  = math.Exp(-SampleInterval.Seconds() / 60)
	decay5  = math.Exp(-SampleInterval.Seconds() / 300)
	decay15 = math.Exp(-SampleInterval.Seconds() / 900)
)

// CPU is the processor block of a metrics report.
type CPU struct {
	Cores  int     `json:"cores"`
	Load1  float64 `json:"load_1m"`
	Load5  float64 `json:"load_5m"`
	Load15 float64 `json:"load_15m"`
	// Percent is utilisation when two samples exist, otherwise the
	// core-normalised load figure — the same fallback the Linux agent makes for
	// its first few seconds.
	Percent int `json:"percent"`
	// UtilisationPercent is nil until the sampler has run twice.
	UtilisationPercent *int `json:"utilisation_percent"`
	LoadPercent        int  `json:"load_percent"`
}

// Usage is a total/available/used triple in megabytes, used for both memory
// and disk.
type Usage struct {
	TotalMB     int64 `json:"total_mb"`
	AvailableMB int64 `json:"available_mb"`
	UsedMB      int64 `json:"used_mb"`
}

// Report is what /metrics returns. Field names and meanings match version 2 of
// the Linux agent exactly, because the Panel keys on them and resets a node's
// history when the version changes.
type Report struct {
	Version     int       `json:"version"`
	Agent       string    `json:"agent"`
	CollectedAt time.Time `json:"collected_at"`
	CPU         CPU       `json:"cpu"`
	Memory      Usage     `json:"memory"`
	// Disk is nil when the data directory's volume cannot be measured.
	Disk *Usage `json:"disk"`
}

// ReportVersion is the agent protocol version. Bumping it makes the Panel drop
// the node's metrics history, so it changes only when a field changes meaning.
const ReportVersion = 2

type cpuSample struct {
	at    time.Time
	total uint64
	busy  uint64
}

// Collector samples the CPU in the background and produces reports on demand.
type Collector struct {
	// DataPath is the directory whose volume is measured for the disk block;
	// normally system.data. Empty disables the disk block.
	DataPath string
	// Agent is reported verbatim so the Panel side can tell which
	// implementation it is talking to.
	Agent string

	mu      sync.Mutex
	samples []cpuSample
	load1   float64
	load5   float64
	load15  float64
	queue   *queueCounter
}

// New returns a collector that has taken one sample, so that the first report
// after Run has been going for one interval already has utilisation.
func New(dataPath, agent string) *Collector {
	c := &Collector{DataPath: dataPath, Agent: agent}
	c.queue = openQueueCounter()
	c.sample()
	return c
}

// Run samples until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(SampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			if c.queue != nil {
				c.queue.close()
				c.queue = nil
			}
			c.mu.Unlock()
			return
		case <-t.C:
			c.sample()
		}
	}
}

// sample reads the CPU counters, appends to the window and advances the load
// averages.
func (c *Collector) sample() {
	total, busy, err := cpuTimes()
	if err != nil {
		// Keep whatever is already there; a transient failure should not
		// blank the metric.
		return
	}
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Instantaneous "load": busy cores over the last interval plus threads
	// waiting for a core. This is what calc_load feeds its averages on Linux,
	// give or take D-state threads, which Windows has no analogue for.
	if n := len(c.samples); n > 0 {
		prev := c.samples[n-1]
		if d := total - prev.total; d > 0 && busy >= prev.busy {
			busyCores := float64(busy-prev.busy) / float64(d) * float64(cores())
			inst := busyCores + c.readyQueue()
			c.load1 = c.load1*decay1 + inst*(1-decay1)
			c.load5 = c.load5*decay5 + inst*(1-decay5)
			c.load15 = c.load15*decay15 + inst*(1-decay15)
		}
	}

	c.samples = append(c.samples, cpuSample{at: now, total: total, busy: busy})
	cutoff := now.Add(-(Window + SampleInterval))
	i := 0
	for i < len(c.samples) && c.samples[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		c.samples = append(c.samples[:0], c.samples[i:]...)
	}
}

func (c *Collector) readyQueue() float64 {
	if c.queue == nil {
		return 0
	}
	v, err := c.queue.read()
	if err != nil {
		return 0
	}
	return v
}

// utilisation returns busy time as a percentage over the widest interval in
// the window, or false until two samples exist.
func (c *Collector) utilisation() (int, bool) {
	if len(c.samples) < 2 {
		return 0, false
	}
	oldest, newest := c.samples[0], c.samples[len(c.samples)-1]
	d := newest.total - oldest.total
	if d == 0 || newest.busy < oldest.busy {
		return 0, false
	}
	pct := float64(newest.busy-oldest.busy) / float64(d) * 100
	return int(math.Max(0, math.Min(100, math.Round(pct)))), true
}

// Report assembles the current figures.
func (c *Collector) Report() Report {
	c.mu.Lock()
	n := cores()
	util, haveUtil := c.utilisation()
	l1, l5, l15 := c.load1, c.load5, c.load15
	c.mu.Unlock()

	loadPct := int(math.Min(999, math.Round(l1/float64(n)*100)))
	cpu := CPU{
		Cores:       n,
		Load1:       round2(l1),
		Load5:       round2(l5),
		Load15:      round2(l15),
		Percent:     loadPct,
		LoadPercent: loadPct,
	}
	if haveUtil {
		u := util
		cpu.Percent = u
		cpu.UtilisationPercent = &u
	}

	return Report{
		Version:     ReportVersion,
		Agent:       c.Agent,
		CollectedAt: time.Now().UTC(),
		CPU:         cpu,
		Memory:      memory(),
		Disk:        disk(c.DataPath),
	}
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func toMB(b uint64) int64 { return int64(b / (1024 * 1024)) }
