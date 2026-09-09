//go:build windows

package system

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Information is what the Panel renders on a node's detail page.
//
// The Docker block is retained with its original JSON shape so an unmodified
// Panel still renders the page. The values describe the Windows equivalents
// rather than being blanked out, which would leave the UI looking broken.
type Information struct {
	Version string            `json:"version"`
	Docker  DockerInformation `json:"docker"`
	System  System            `json:"system"`
}

type DockerInformation struct {
	Version    string           `json:"version"`
	Cgroups    DockerCgroups    `json:"cgroups"`
	Containers DockerContainers `json:"containers"`
	Storage    DockerStorage    `json:"storage"`
	Runc       DockerRunc       `json:"runc"`
}

type DockerCgroups struct {
	Driver  string `json:"driver"`
	Version string `json:"version"`
}

type DockerContainers struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Paused  int `json:"paused"`
	Stopped int `json:"stopped"`
}

type DockerStorage struct {
	Driver     string `json:"driver"`
	Filesystem string `json:"filesystem"`
}

type DockerRunc struct {
	Version string `json:"version"`
}

type System struct {
	Architecture  string `json:"architecture"`
	CPUThreads    int    `json:"cpu_threads"`
	MemoryBytes   int64  `json:"memory_bytes"`
	KernelVersion string `json:"kernel_version"`
	OS            string `json:"os"`
	OSType        string `json:"os_type"`
}

// InstanceCounter reports how many servers exist and how many are running.
//
// Set by the daemon at startup. Without it the container counts read zero, which
// is accurate rather than misleading.
var InstanceCounter func() (total int, running int)

func GetSystemInformation() (*Information, error) {
	product, build := windowsRelease()

	total, running := 0, 0
	if InstanceCounter != nil {
		total, running = InstanceCounter()
	}

	return &Information{
		Version: Version,
		Docker: DockerInformation{
			// There is no container engine. Reporting the daemon's own identity
			// here is more useful to an operator reading the node page than an
			// empty field.
			Version: "win-wings (no container engine)",
			Cgroups: DockerCgroups{
				// Job Objects are what provides the resource limits cgroups did.
				Driver:  "job-object",
				Version: "1",
			},
			Containers: DockerContainers{
				Total:   total,
				Running: running,
				Paused:  0,
				Stopped: total - running,
			},
			Storage: DockerStorage{
				Driver:     "ntfs",
				Filesystem: volumeFilesystem(),
			},
			Runc: DockerRunc{Version: ""},
		},
		System: System{
			Architecture:  runtime.GOARCH,
			CPUThreads:    runtime.NumCPU(),
			MemoryBytes:   physicalMemoryBytes(),
			KernelVersion: build,
			OS:            product,
			OSType:        runtime.GOOS,
		},
	}, nil
}

// windowsRelease returns the product name and build string.
func windowsRelease() (product string, build string) {
	product, build = "Windows", ""

	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return product, build
	}
	defer k.Close()

	if v, _, err := k.GetStringValue("ProductName"); err == nil && v != "" {
		product = v
	}
	if v, _, err := k.GetStringValue("CurrentBuildNumber"); err == nil && v != "" {
		build = v
		if ubr, _, err := k.GetIntegerValue("UBR"); err == nil {
			build = fmt.Sprintf("%s.%d", v, ubr)
		}
	}
	if v, _, err := k.GetStringValue("DisplayVersion"); err == nil && v != "" {
		product = fmt.Sprintf("%s %s", product, v)
	}
	return product, build
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

// physicalMemoryBytes reports installed physical memory.
func physicalMemoryBytes() int64 {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))

	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	if r, _, _ := proc.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return int64(m.TotalPhys)
}

// volumeFilesystem reports the filesystem backing the daemon's data volume.
func volumeFilesystem() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	vol := filepath.VolumeName(exe)
	if vol == "" {
		return ""
	}

	root, err := windows.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return ""
	}

	nameBuf := make([]uint16, 261)
	fsBuf := make([]uint16, 261)
	if err := windows.GetVolumeInformation(
		root,
		&nameBuf[0], uint32(len(nameBuf)),
		nil, nil, nil,
		&fsBuf[0], uint32(len(fsBuf)),
	); err != nil {
		return ""
	}
	return strings.ToLower(windows.UTF16ToString(fsBuf))
}
