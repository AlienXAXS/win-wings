//go:build windows

package config

import (
	"github.com/apex/log"
	"golang.org/x/sys/windows/registry"
)

const longPathsKey = `SYSTEM\CurrentControlSet\Control\FileSystem`

// enableLongPaths turns on the machine-wide long path policy if it is off.
//
// The daemon's own sandbox does not need this: os.Root resolves paths one
// component at a time against a directory handle, and MAX_PATH is a limit of the
// Win32 path-string layer rather than the kernel. Server files nested arbitrarily
// deep are reachable either way.
//
// Game servers are a different matter. A JVM writing a modpack's config tree uses
// ordinary Win32 paths and hits the 260 character limit, which surfaces as a mod
// failing to write its own configuration — an error nobody attributes to the
// host. Windows Server still ships with this disabled.
//
// Failure is not fatal. The daemon may be running as an account without
// HKLM write access, and every server that stays under the limit is unaffected.
func enableLongPaths() {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, longPathsKey, registry.QUERY_VALUE)
	if err != nil {
		log.WithField("error", err).Debug("could not read the long path policy")
		return
	}

	v, _, err := k.GetIntegerValue("LongPathsEnabled")
	_ = k.Close()
	if err == nil && v == 1 {
		log.Debug("long path support is already enabled")
		return
	}

	w, err := registry.OpenKey(registry.LOCAL_MACHINE, longPathsKey, registry.SET_VALUE)
	if err != nil {
		log.WithField("error", err).Warn(
			"long path support is disabled and could not be enabled: game servers writing " +
				"deeply nested files, such as Minecraft modpack configuration, may fail at 260 " +
				"characters. Set LongPathsEnabled to 1 under HKLM\\" + longPathsKey)
		return
	}
	defer w.Close()

	if err := w.SetDWordValue("LongPathsEnabled", 1); err != nil {
		log.WithField("error", err).Warn("failed to enable long path support")
		return
	}

	// The policy is read when a process starts, so servers launched from now on
	// pick it up without a reboot.
	//
	// It is necessary but not always sufficient: an executable must also declare
	// longPathAware in its manifest for the Win32 layer to honour it. Runtimes
	// vary, so this removes one cause of deep-path failures rather than all of
	// them.
	log.Info("enabled machine-wide long path support; servers started from now on will inherit it")
}
