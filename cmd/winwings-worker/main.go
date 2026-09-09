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

	w := worker.New(*cfg)

	if err := w.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "winwings-worker: %v\n", err)
		os.Exit(1)
	}
}
