//go:build windows

// Command winwings-worker supervises a single game server process.
//
// It is spawned detached by the win-wings daemon, one per running server, and
// given the path to an instance directory containing its configuration:
//
//	winwings-worker.exe C:\ProgramData\WinWings\instances\<uuid>
//
// The worker owns the Job Object, the process, and its console handles, and
// serves control over a named pipe at \\.\pipe\winwings-<uuid>. It outlives
// daemon restarts by design — see internal/wire for why that matters.
//
// This binary is not intended to be run by hand.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pterodactyl/wings/internal/winproc"
	"github.com/pterodactyl/wings/internal/wire"
	"github.com/pterodactyl/wings/internal/worker"
)

// ConfigFileName is the worker configuration within an instance directory.
const ConfigFileName = "worker.json"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <instance-directory>\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}

	instanceDir := os.Args[1]
	cfg, err := worker.LoadConfig(filepath.Join(instanceDir, ConfigFileName))
	if err != nil {
		fmt.Fprintf(os.Stderr, "winwings-worker: %v\n", err)
		os.Exit(1)
	}

	// Default the console log into the instance directory rather than the
	// server's own data directory. The server must not be able to rewrite or
	// delete its own console history, and its data directory is writable by it.
	if cfg.LogPath == "" {
		cfg.LogPath = filepath.Join(instanceDir, "console.log")
	}

	// Diagnostics land beside worker.json rather than in the server's data
	// directory: the server must not be able to rewrite the record of what its
	// own worker did.
	worker.SetDiagnosticLog(filepath.Join(instanceDir, "worker.log"))

	w := worker.New(*cfg)

	// The worker launches the game server under its own account, which is where
	// a desktop grant can fail. Relayed up the control pipe and written to the
	// worker's log, because its stderr goes to a file nobody thinks to look in.
	winproc.Warn = func(msg string) {
		w.Log(wire.LogWarn, msg)
	}

	w.Log(wire.LogInfo, "worker starting",
		"uuid", cfg.UUID, "pid", os.Getpid(), "instance", instanceDir)

	if err := w.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "winwings-worker: %v\n", err)
		os.Exit(1)
	}
}
