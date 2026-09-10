# win-wings

A Windows-native fork of [Pterodactyl wings](https://github.com/pterodactyl/wings).

Servers run as ordinary Windows processes inside Job Objects. There is no Docker,
no containers, and no WINE. Some game servers simply run badly under emulation;
this exists so they can run natively.

> **Status: ready for first deployment and testing.** The full test suite passes
> on Windows, including tests that drive the real kernel APIs. It has not yet run
> a production workload. See [Known gaps](#known-gaps).

## What is different from upstream

| Docker provided | Replaced by |
|---|---|
| cgroup resource limits | Windows Job Objects |
| kill the container, kill the tree | `TerminateJobObject` |
| filesystem isolation | one local account per server + NTFS ACLs, both daemon-managed |
| port publishing | **nothing** — servers are trusted to honour their allocation |
| console surviving daemon restart | a per-server worker process |
| `openat2` path sandbox | Go's `os.Root` |
| bash install scripts | PowerShell |

Two binaries are produced and must be deployed together:

```
wings.exe                 the daemon; runs as a Windows service
winwings-worker.exe       one detached supervisor per running server
```

The worker exists because Windows cannot reattach to another process's stdio.
Without it, restarting the daemon would permanently lose the console for every
running server. With it, the daemon restarts freely and reconnects — replaying
the output it missed. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## The Panel is not forked

This runs against an unmodified Pterodactyl Panel plus a Blueprint plugin that
serves per-egg Windows profiles: a PowerShell install script, a Windows startup
command, a stop configuration, and a runtime selector.

The contract that plugin must implement is in
[docs/PANEL-API.md](docs/PANEL-API.md).

For bring-up you can run without it — set `runtime.require_windows_profile: false`
and the daemon falls back to each egg's standard fields. That is only useful for
testing that a node comes up; most eggs will not actually install or start,
because their scripts are bash.

## Build

```bash
make release
```

Requires Go 1.25 or newer (the sandbox is built on `os.Root`).

## Install

See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md). In short:

```powershell
# Elevated
C:\ProgramData\WinWings\wings.exe service install --config C:\ProgramData\WinWings\config.yml
sc start winwings

# Elevated: prove the host actually works before pointing the Panel at it
C:\ProgramData\WinWings\wings.exe selftest --config C:\ProgramData\WinWings\config.yml

# Unelevated
C:\ProgramData\WinWings\wings.exe service status
C:\ProgramData\WinWings\wings.exe diagnostics
```

`service install` creates the daemon's own account, gives it a random password
nobody has to know, and grants it exactly the rights it needs. Nothing has to be
prepared in `secpol.msc` first.

Each server then gets its own local account, created and deleted with the server
and named after its UUID, with a random password held only in memory. That
account plus an NTFS ACL is what keeps one server out of another's files, so it
is worth running `selftest` once on a new host: it creates two throwaway servers
and checks that neither can read the other.

## Testing

```bash
make test               # everything
make test-integration   # the tests that drive real kernel APIs
```

The Job Object, process and worker tests are only meaningful on Windows. They
verify that a job captures a process *and* its children, that terminating the job
kills the whole tree, that console output and stdin work, and that a server keeps
running across a daemon disconnect and reconnect.

## Known gaps

- **ConPTY is unverified.** Implemented and matching Microsoft's documented
  sample, but it produced no output on the development machine. Plain pipes work
  fully and are the default; only steamcmd-class processes need ConPTY. Run
  `go test ./internal/winproc -run TestConPTY -v` on a real interactive Windows
  host — the diagnostic records everything already ruled out.
- **Port allocations are not enforced.** Nothing binds on a server's behalf.
  Use per-account firewall rules.
- **Network statistics need an administrative daemon, or a one-time grant.**
  Per-server byte counts come from a kernel trace rather than a network
  namespace. Under the default managed isolation the daemon can start one; an
  unprivileged daemon needs `wings service allow-network-stats` run once,
  elevated. The counts are transport payload, a few percent under what an
  interface counter would show.
- **Cross-platform transfers are unsupported.** Backups carry POSIX modes and
  symlinks; Linux ↔ Windows node migration will not work.
- **Every egg needs porting.** No upstream egg works unchanged.

## Licence

MIT, as upstream. See [LICENSE](LICENSE).

This is an unofficial fork and is not affiliated with or endorsed by the
Pterodactyl project.
