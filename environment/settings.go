package environment

import (
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"

	"github.com/apex/log"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/wire"
)

type Mount struct {
	// In Docker environments this makes no difference, however in a non-Docker environment you
	// should treat the "Default" mount as the root directory for the server. All other mounts
	// are just in addition to that one, and generally things like shared maps or timezone data.
	Default bool `json:"-"`

	// The target path on the system. This is "/home/container" for all server's Default mount
	// but in non-container environments you can likely ignore the target and just work with the
	// source.
	Target string `json:"target"`

	// The directory from which the files will be read. In Docker environments this is the directory
	// that we're mounting into the container at the Target location.
	Source string `json:"source"`

	// Whether the directory is being mounted as read-only. It is up to the environment to
	// handle this value correctly and ensure security expectations are met with its usage.
	ReadOnly bool `json:"read_only"`
}

// Limits is the build settings for a given server that impact docker container
// creation and resource limits for a server instance.
type Limits struct {
	// The total amount of memory in mebibytes that this server is allowed to
	// use on the host system.
	MemoryLimit int64 `json:"memory_limit"`

	// Swap is accepted from the Panel and ignored. Windows manages its page file
	// globally and offers no per-process swap allocation.
	Swap int64 `json:"swap"`

	// IoWeight is accepted from the Panel and ignored. Job Objects have no
	// equivalent of Docker's block IO weight.
	IoWeight uint16 `json:"io_weight"`

	// The percentage of CPU that this instance is allowed to consume relative to
	// the host. A value of 200% represents complete utilization of two cores. This
	// should be a value between 1 and THREAD_COUNT * 100.
	CpuLimit int64 `json:"cpu_limit"`

	// The amount of disk space in megabytes that a server is allowed to use.
	DiskSpace int64 `json:"disk_space"`

	// Threads selects which processors the server may run on, in cpuset syntax.
	Threads string `json:"threads"`

	// OOMDisabled is accepted from the Panel and ignored. A Job Object memory cap
	// causes allocation failures rather than an OOM kill, so there is nothing to
	// disable.
	OOMDisabled bool `json:"oom_disabled"`
}

// MemoryOverheadMultiplier sets the hard limit for memory usage above the
// amount assigned to the server. This matters more under a Job Object than it
// did under Docker: JOB_OBJECT_LIMIT_JOB_MEMORY makes allocations fail rather
// than invoking an OOM killer, so a runtime sitting fractionally over its limit
// dies with an allocation error instead of being reaped.
func (l Limits) MemoryOverheadMultiplier() float64 {
	return config.Get().Runtime.Overhead.GetMultiplier(l.MemoryLimit)
}

// BoundedMemoryLimit returns the memory limit in bytes, including overhead.
func (l Limits) BoundedMemoryLimit() int64 {
	return int64(math.Round(float64(l.MemoryLimit) * l.MemoryOverheadMultiplier() * 1024 * 1024))
}

// ProcessLimit returns the maximum number of concurrently active processes.
func (l Limits) ProcessLimit() uint32 {
	return config.Get().Runtime.ProcessLimit
}

// AsJobLimits converts the server's build settings into Job Object limits.
func (l Limits) AsJobLimits() wire.Limits {
	return wire.Limits{
		MemoryBytes:  l.BoundedMemoryLimit(),
		CpuRate:      l.JobCpuRate(),
		CpuHardCap:   config.Get().Runtime.CpuHardCap,
		AffinityMask: l.AffinityMask(),
		ProcessLimit: l.ProcessLimit(),
	}
}

// JobCpuRate converts the Panel's CPU limit into the units a Job Object uses.
//
// The two express different things. The Panel's CpuLimit is a percentage of one
// processor, so 200 means two cores fully used. A Job Object's CpuRate is a
// share of *all* processors expressed in 1/100ths of a percent, where 10000 is
// the whole machine. On an 8-core host, the Panel's 200 is therefore 2500.
//
// Returns 0 when no limit is set, which disables CPU rate control entirely.
func (l Limits) JobCpuRate() uint32 {
	if l.CpuLimit <= 0 {
		return 0
	}

	cpus := int64(runtime.NumCPU())
	if cpus < 1 {
		cpus = 1
	}

	rate := l.CpuLimit * 100 / cpus
	if rate < 1 {
		// Never round a real limit down to "unlimited".
		rate = 1
	}
	if rate > 10000 {
		rate = 10000
	}
	return uint32(rate)
}

// AffinityMask converts the Panel's thread pinning string into a processor mask.
//
// The Panel sends a cpuset-style list such as "0-2,4". Anything unparseable
// yields 0, which leaves the server free to use every processor rather than
// pinning it somewhere arbitrary.
func (l Limits) AffinityMask() uint64 {
	if l.Threads == "" {
		return 0
	}

	var mask uint64
	for _, part := range strings.Split(l.Threads, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, found := strings.Cut(part, "-")
		start, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || start < 0 || start > 63 {
			continue
		}
		end := start
		if found {
			end, err = strconv.Atoi(strings.TrimSpace(hi))
			if err != nil || end < start || end > 63 {
				continue
			}
		}
		for i := start; i <= end; i++ {
			mask |= 1 << uint(i)
		}
	}
	return mask
}

type Variables map[string]interface{}

// Get is an ugly hacky function to handle environment variables that get passed
// through as not-a-string from the Panel. Ideally we'd just say only pass
// strings, but that is a fragile idea and if a string wasn't passed through
// you'd cause a crash or the server to become unavailable. For now try to
// handle the most likely values from the JSON and hope for the best.
func (v Variables) Get(key string) string {
	val, ok := v[key]
	if !ok {
		return ""
	}

	switch val.(type) {
	case int:
		return strconv.Itoa(val.(int))
	case int32:
		return strconv.FormatInt(val.(int64), 10)
	case int64:
		return strconv.FormatInt(val.(int64), 10)
	case float32:
		return fmt.Sprintf("%f", val.(float32))
	case float64:
		return fmt.Sprintf("%f", val.(float64))
	case bool:
		return strconv.FormatBool(val.(bool))
	case string:
		return val.(string)
	}

	// TODO: I think we can add a check for val == nil and return an empty string for those
	//  and this warning should theoretically never happen?
	log.Warn(fmt.Sprintf("failed to marshal environment variable \"%s\" of type %+v into string", key, val))

	return ""
}
