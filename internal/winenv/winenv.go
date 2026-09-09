//go:build windows

// Package winenv builds the environment block handed to server processes and
// install scripts.
//
// This exists because of an asymmetry between Docker and CreateProcess that is
// easy to miss. Under Docker, wings passed a handful of variables and the
// container image supplied everything else — PATH, HOME, TMPDIR, the lot.
// CreateProcess has no image: when lpEnvironment is non-NULL it *replaces* the
// environment entirely, so a process launched with only the Panel's variables
// gets an environment with no SystemRoot, no TEMP and no PATH.
//
// Windows does not degrade gracefully from that. SystemRoot in particular is
// read during process startup to locate system DLLs, and Winsock initialisation
// fails without it — which is how it presents: not as "SystemRoot is unset" but
// as a networked installer that cannot resolve anything.
//
// So this package supplies the floor, and the Panel's variables are layered on
// top where they win.
package winenv

import (
	"os"
	"path/filepath"
	"strings"
)

// Paths locates the per-server directories that the environment points at.
type Paths struct {
	// Data is the server's own files: its working directory, its sandbox root,
	// and what the Panel thinks of as /home/container.
	Data string

	// Temp is a scratch directory for the server, outside the sandbox.
	//
	// Outside deliberately. Windows installers write freely to TEMP — an
	// unpacked game depot can be several gigabytes — and inside the sandbox that
	// would be charged against the user's disk quota and copied into every
	// backup, both of which walk the sandbox root.
	Temp string
}

// passthrough are host facts rather than policy: the same value the daemon sees
// is the right value for the server.
//
// PROCESSOR_* and NUMBER_OF_PROCESSORS are included because build tooling
// invoked by install scripts reads them to size its own parallelism, and a
// missing value tends to mean "assume one".
var passthrough = []string{
	"SystemRoot",
	"SystemDrive",
	"windir",
	"NUMBER_OF_PROCESSORS",
	"PROCESSOR_ARCHITECTURE",
	"PROCESSOR_IDENTIFIER",
	"PROCESSOR_LEVEL",
	"PROCESSOR_REVISION",
	"COMPUTERNAME",
}

// Base returns the environment every server process starts from.
//
// Deliberately not the daemon's own environment. That carries whatever the
// service account was configured with — proxy settings, credentials helpers,
// anything an operator exported — and none of it is a server's business. Only
// the variables named here cross over.
func Base(p Paths) []string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		// Only reachable if the daemon itself was launched without one. Guessing
		// beats propagating the emptiness, because everything downstream fails
		// in ways that do not name the cause.
		root = `C:\Windows`
	}
	system32 := filepath.Join(root, "System32")

	env := []string{
		"ComSpec=" + filepath.Join(system32, "cmd.exe"),
		"PATHEXT=.COM;.EXE;.BAT;.CMD;.VBS;.JS;.WS;.MSC",
		"PATH=" + strings.Join([]string{
			system32,
			root,
			filepath.Join(system32, "Wbem"),
			filepath.Join(system32, "WindowsPowerShell", "v1.0"),
		}, string(os.PathListSeparator)),
	}

	for _, k := range passthrough {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}

	if p.Temp != "" {
		env = append(env, "TEMP="+p.Temp, "TMP="+p.Temp)
	}

	if p.Data != "" {
		// The Linux eggs this port inherits set HOME to the server directory so
		// that anything writing dotfiles — steamcmd being the usual culprit —
		// lands somewhere the user can see and the daemon can back up. The
		// Windows equivalent is these four, and the reasoning carries over: the
		// server account has no loaded user profile, so without them a process
		// asking for its profile directory gets a path it cannot write.
		env = append(env,
			"USERPROFILE="+p.Data,
			"HOMEDRIVE="+filepath.VolumeName(p.Data),
			"HOMEPATH="+strings.TrimPrefix(p.Data, filepath.VolumeName(p.Data)),
			"APPDATA="+filepath.Join(p.Data, "AppData", "Roaming"),
			"LOCALAPPDATA="+filepath.Join(p.Data, "AppData", "Local"),
			// Not a Windows convention, but enough ported tooling reads it that
			// setting it costs nothing and omitting it costs a support ticket.
			"HOME="+p.Data,
		)
	}

	return env
}

// Merge overlays vars onto base, with vars winning.
//
// Windows environment variable names are case-insensitive, so a Panel variable
// named "path" has to replace "PATH" rather than sit beside it — CreateProcess
// accepts a block containing both and the process then sees whichever it happens
// to look up first.
func Merge(base, vars []string) []string {
	index := make(map[string]int, len(base)+len(vars))
	out := make([]string, 0, len(base)+len(vars))

	add := func(e string) {
		k, _, ok := strings.Cut(e, "=")
		if !ok {
			return
		}
		key := strings.ToUpper(k)
		if i, seen := index[key]; seen {
			out[i] = e
			return
		}
		index[key] = len(out)
		out = append(out, e)
	}

	for _, e := range base {
		add(e)
	}
	for _, e := range vars {
		add(e)
	}
	return out
}
