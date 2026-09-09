# Architecture

How win-wings differs from upstream Pterodactyl wings, and why.

## The shape of the change

Upstream runs every server in a Docker container. That single decision supplied
filesystem isolation, a network namespace, resource limits, a clean way to kill a
process tree, and — importantly — console output that survived the daemon
restarting.

None of that exists on Windows without containers. Each capability is replaced
separately:

| Docker provided | Replaced by |
|---|---|
| cgroup resource limits | Windows Job Objects (`internal/jobobject`) |
| kill the container, kill the tree | `TerminateJobObject` |
| filesystem isolation | one local account per server + NTFS ACLs, both daemon-managed |
| network namespace, port publishing | **nothing** — see below |
| console surviving daemon restart | a per-server worker process |
| `/mnt/server` bind mount for installs | `$env:SERVER_DIR` |
| image entrypoint building the command line | `internal/winproc.ParseCommandLine` |
| openat2 path sandbox | `os.Root` (`internal/winfs`) |

## Two binaries

```
wings.exe                    the daemon; one Windows service
└── winwings-worker.exe      one detached process per running server
```

They must be deployed in the same directory; the daemon spawns the worker from
alongside itself.

### Why the worker exists

This is the least obvious decision in the port, and everything else follows from
it.

Under Docker, the daemon could restart and reattach to a running container's
console because the container's stdio belongs to the Docker daemon. Windows has
no equivalent: a process's stdin/stdout handles **cannot be reattached from
outside** once their creator is gone. If the daemon owned the game process's
pipes directly, restarting win-wings would permanently lose console output and
command input for every running server.

So the worker holds them. It owns:

- the Job Object and its limits
- the game process
- the console handles, plus a ring buffer of recent output
- a named pipe at `\\.\pipe\winwings-<uuid>`

The daemon connects to that pipe. When it restarts, it reconnects, learns the
server is still running, and is replayed the console output it missed. This is
covered end to end by `TestWorkerSurvivesDaemonReconnect`.

The protocol is JSON Lines (`internal/wire`), chosen for debuggability — a stuck
server can be diagnosed by attaching to the pipe and reading it. Console payloads
are base64 because game servers emit arbitrary bytes and invalid UTF-8 would
otherwise be silently mangled.

The pipe carries an SDDL granting access only to the daemon's own account plus
SYSTEM, and a per-server token on top of that.

### Lifetime

The job is created with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, so a worker's death
takes its server with it. That is deliberate: the alternative is an orphaned
process with no console, no stdin and no supervisor, which the daemon would then
try to start a second copy of. Two instances sharing one directory and one port
is worse than a stopped server.

This does **not** couple servers to the daemon's lifetime — the worker holds the
handle, not the daemon.

## The filesystem sandbox

`internal/winfs` replaces upstream's `internal/ufs`, built on Go's `os.Root`
rather than hand-rolled path resolution.

That choice is deliberate. Nearly every security advisory in upstream wings'
history has been a path-sandbox escape (CVE-2023-25152, -25168, -32080,
CVE-2024-27102, GHSA-gqmf-jqgv-v8fw, CVE-2026-21696, and the v1.12.2 `fs.Chmod`
symlink bug). Rewriting that layer means owning that entire bug class alone, so
the guiding principle is to minimise how much path resolution we own. `os.Root`
is maintained by the Go team and receives security fixes we inherit.

`os_root_probe_test.go` documents what it actually defends against on Windows,
verified rather than assumed: relative traversal, absolute and drive-relative
paths, the `\\?\` and `\\.\` device namespaces, UNC paths, **junctions** (the
important case, since they need no privilege), rename-out-of-root, and
link creation pointing outside.

What `os.Root` does not give is **canonical naming**. Case, trailing dots and
spaces, and 8.3 short names all alias the same file, which matters because the
egg file denylist is matched as a string. `normalize()` in `winfs` additionally
rejects UNC and device paths outright rather than letting them be mangled into
valid relative paths.

The `dirfd` that upstream threaded through its `*at()` calls is replaced by a
nested `os.Root` handle (`winfs.Dir`), which keeps per-entry `Lstat` a
single-component resolve during large directory walks.

## Startup commands

Upstream never built a command line. It exported the startup string as the
`STARTUP` environment variable and the Docker image's entrypoint did the
`{{VAR}}` substitution and handed the result to a shell.

There is no entrypoint here, so both steps are ours:

1. `{{VAR}}` is normalised to `${VAR}` and expanded from the server environment.
2. The result is split with `windows.DecomposeCommandLine`, which implements the
   real `CommandLineToArgvW` rules.
3. The binary is executed **directly**. No shell is involved.

Routing partly user-controlled text through `cmd.exe` would execute it with the
server account's full host privileges. The consequence is that shell operators
are not interpreted, so eggs relying on `&&` or `>` need a rewritten startup line
in their Windows profile.

## What is genuinely weaker than Docker

Stated plainly, because operators need to know:

**Port enforcement is gone.** Nothing binds ports on a server's behalf. The
allocation is passed through as `{{SERVER_IP}}`/`{{SERVER_PORT}}` and the server
is trusted to honour it. A misconfigured or malicious server can bind any free
port. Constraining that requires per-account firewall rules applied outside the
daemon.

**The daemon is an administrator, by default and by design.** Under
`isolation: managed` it creates a local account per server, grants it the batch
logon right, and rewrites NTFS ownership — all privileged operations. So a
compromise of the daemon is a compromise of the host.

That is a deliberate trade rather than an oversight. The alternative,
`isolation: pool`, keeps the daemon unprivileged but moves account creation onto
the operator, puts the passwords on disk, and caps the node at one server per
pre-created account. Both are supported; managed is the default because the
likely compromise is a *server*, not the daemon, and managed isolation is the one
an operator will actually get right. Every server runs as its own unprivileged
account, denied every logon type but batch, behind an ACL that admits nothing
else.

With `isolation: shared` there is no separation at all: one compromised server
can read every other server's files and the daemon's configuration, which holds
the Panel token controlling every server on the node. The daemon warns loudly
about this at boot.

**No network namespace**, so per-server network statistics are reported as zero.

**Hard links are over-counted** in disk usage. Windows exposes neither the link
count nor the file index through `os.FileInfo`, and obtaining them means opening
every file during a walk. The error is always in the safe direction.

## Package map

| Package | Role |
|---|---|
| `internal/winfs` | sandboxed filesystem on `os.Root` |
| `internal/jobobject` | Job Object limits, stats, limit notifications |
| `internal/winproc` | process creation, ConPTY, argv parsing, logon tokens |
| `internal/wire` | daemon ↔ worker protocol |
| `internal/worker` | the supervisor, and the daemon-side client |
| `environment/windows` | `ProcessEnvironment` implemented over the worker |
| `cmd/winwings-worker` | the worker binary |

## Known gaps

**ConPTY is unverified.** It is implemented and matches Microsoft's documented
sample, but produced no output on the development machine. Pipe mode works fully
and is the default. `internal/winproc/conpty_diag_test.go` records everything
already ruled out; re-run it on a real interactive Windows host before relying on
it. Only steamcmd-class processes need it.

**Cross-platform transfers.** Backups and transfers carry POSIX modes and
symlinks. Moving a server between a Linux node and a Windows node is not
supported.
