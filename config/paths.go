package config

import "path/filepath"

// Per-server directory layout.
//
// Every server owns one directory tree, named by its UUID, beneath System.Data:
//
//	<data>\<uuid>\                 server root, owned by the daemon
//	<data>\<uuid>\data\            server files — the sandbox and SFTP root
//	<data>\<uuid>\runtime\         per-server runtime, when one is provisioned
//	<data>\<uuid>\worker.json      worker configuration
//	<data>\<uuid>\console.log      console history
//
// The server's own files sit one level *below* its root, and that nesting is
// load-bearing rather than cosmetic:
//
//   - worker.json holds the resolved startup command. A server able to write it
//     could rewrite what its own worker executes, which is arbitrary code
//     execution as its account. Because the sandbox is rooted at data\, the
//     server cannot address its parent at all — os.Root refuses "..".
//
//   - Disk usage and backups both walk the sandbox root. A per-server runtime
//     inside it would be charged against the user's disk quota and copied into
//     every backup. Outside it, a 200MB JDK costs neither.
//
// Deleting a server is one RemoveAll of its root.

// ServerRoot returns a server's top-level directory.
//
// The daemon owns this. The account the server runs as must not have write
// access here, only to ServerData below it.
func (sc *SystemConfiguration) ServerRoot(uuid string) string {
	return filepath.Join(sc.Data, uuid)
}

// ServerData returns the directory holding a server's own files.
//
// This is the sandbox root, the SFTP root, and what the Panel thinks of as
// /home/container. It is the only part of the tree the server can write.
func (sc *SystemConfiguration) ServerData(uuid string) string {
	return filepath.Join(sc.Data, uuid, "data")
}

// ServerRuntime returns a server's private runtime directory.
//
// Optional. When present it takes precedence over the host-wide
// runtime.runtimes map, for eggs needing a specific JVM build. Populated by the
// daemon rather than the install script, which cannot write outside ServerData.
func (sc *SystemConfiguration) ServerRuntime(uuid string) string {
	return filepath.Join(sc.Data, uuid, "runtime")
}

// ServerWorkerConfig returns the path of a server's worker configuration.
func (sc *SystemConfiguration) ServerWorkerConfig(uuid string) string {
	return filepath.Join(sc.Data, uuid, WorkerConfigFileName)
}

// ServerConsoleLog returns the path of a server's console log.
func (sc *SystemConfiguration) ServerConsoleLog(uuid string) string {
	return filepath.Join(sc.Data, uuid, "console.log")
}

// WorkerConfigFileName is the worker's configuration within a server's root.
const WorkerConfigFileName = "worker.json"
