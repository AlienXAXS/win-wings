//go:build windows

package hoststats

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes  = kernel32.NewProc("GetSystemTimes")
	procGlobalMemStatus = kernel32.NewProc("GlobalMemoryStatusEx")

	pdh                            = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQuery               = pdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounter       = pdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData        = pdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterValu = pdh.NewProc("PdhGetFormattedCounterValue")
	procPdhCloseQuery              = pdh.NewProc("PdhCloseQuery")
)

func cores() int { return runtime.NumCPU() }

// cpuTimes returns total and busy CPU time since boot, summed across every
// processor, in 100ns units.
//
// GetSystemTimes is the /proc/stat of Windows: idle, kernel and user time,
// where kernel time includes idle. Busy is therefore kernel + user - idle.
// There is no iowait figure to exclude; a thread blocked on disk is simply not
// running, which is the behaviour the Linux agent goes out of its way to get.
func cpuTimes() (total, busy uint64, err error) {
	var idle, kernel, user windows.Filetime
	r, _, e := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return 0, 0, fmt.Errorf("hoststats: GetSystemTimes: %w", e)
	}
	i, k, u := ft(idle), ft(kernel), ft(user)
	total = k + u
	if i > total {
		return 0, 0, errors.New("hoststats: GetSystemTimes returned idle time above total")
	}
	return total, total - i, nil
}

func ft(f windows.Filetime) uint64 {
	return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime)
}

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// memory reads physical memory.
//
// AvailPhys is the memory the system can hand out without paging: free pages
// plus the zeroed and standby lists. That is the same idea as Linux's
// MemAvailable — what could be claimed, not what is currently untouched — and
// is what the Linux agent reports, so a node with a large file cache does not
// read as full.
func memory() Usage {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemStatus.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return Usage{}
	}
	total, avail := toMB(m.TotalPhys), toMB(m.AvailPhys)
	return Usage{TotalMB: total, AvailableMB: avail, UsedMB: total - avail}
}

// disk measures the volume holding the server data directory.
//
// The directory may not exist yet on a freshly configured node, so the path is
// walked upwards to the nearest directory that does; the volume is the same.
// Reports the space available to the daemon's account rather than the raw free
// figure, which is what statfs's bavail gives the Linux agent and what a quota
// on the volume would actually let servers use.
func disk(dataPath string) *Usage {
	if dataPath == "" {
		return nil
	}
	p := filepath.Clean(dataPath)
	for {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}

	dir, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return nil
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &avail, &total, &free); err != nil {
		return nil
	}
	t, a := toMB(total), toMB(avail)
	return &Usage{TotalMB: t, AvailableMB: a, UsedMB: t - a}
}

// queueCounter reads \System\Processor Queue Length through PDH.
//
// The ready queue — threads that could run if a core were free — is the half of
// a load average that utilisation cannot see. Without it a saturated host reads
// as exactly 100% busy with a load of exactly the core count, which hides how
// far over it is. The counter is instantaneous, so one collect per read is
// enough. PDH being unavailable, or the counter missing on a host with a
// damaged performance registry, degrades to a load average built from
// utilisation alone rather than an error.
type queueCounter struct {
	query   uintptr
	counter uintptr
}

// pdhFmtCounterValue mirrors PDH_FMT_COUNTERVALUE with the union read as a
// double (PDH_FMT_DOUBLE).
type pdhFmtCounterValue struct {
	CStatus uint32
	_       uint32
	Double  float64
}

const pdhFmtDouble = 0x00000200

func openQueueCounter() *queueCounter {
	if err := pdh.Load(); err != nil {
		return nil
	}
	var q queueCounter
	if r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q.query))); r != 0 {
		return nil
	}
	path, _ := windows.UTF16PtrFromString(`\System\Processor Queue Length`)
	if r, _, _ := procPdhAddEnglishCounter.Call(q.query, uintptr(unsafe.Pointer(path)), 0,
		uintptr(unsafe.Pointer(&q.counter))); r != 0 {
		procPdhCloseQuery.Call(q.query)
		return nil
	}
	return &q
}

func (q *queueCounter) read() (float64, error) {
	if r, _, _ := procPdhCollectQueryData.Call(q.query); r != 0 {
		return 0, fmt.Errorf("hoststats: PdhCollectQueryData: 0x%x", r)
	}
	var v pdhFmtCounterValue
	if r, _, _ := procPdhGetFormattedCounterValu.Call(q.counter, pdhFmtDouble, 0,
		uintptr(unsafe.Pointer(&v))); r != 0 {
		return 0, fmt.Errorf("hoststats: PdhGetFormattedCounterValue: 0x%x", r)
	}
	if v.Double < 0 {
		return 0, nil
	}
	return v.Double, nil
}

func (q *queueCounter) close() {
	procPdhCloseQuery.Call(q.query)
}
