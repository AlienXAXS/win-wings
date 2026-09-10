# Deploying win-wings

A Windows-native fork of Pterodactyl wings. Servers run as ordinary Windows
processes inside Job Objects — there is no Docker, and no containers.

Read [ARCHITECTURE.md](ARCHITECTURE.md) for how it works and
[PANEL-API.md](PANEL-API.md) for the Panel-side contract you will need before
any egg can be installed.

## Requirements

- Windows Server 2019 / Windows 10 1809 or newer. Server 2025 with **Desktop
  Experience** is the tested target; Server Core is not recommended, as several
  game servers link against GDI and user32 even when headless.
- PowerShell 5.1 (built in) — PowerShell 7 is used in preference if installed
- A Pterodactyl-compatible Panel with the win-wings Blueprint plugin

## Build

Both binaries must be deployed **in the same directory**. The daemon spawns the
worker from alongside itself and refuses to start if it is missing.

```powershell
.\build.ps1 -Release
```

Produces `build\wings.exe` and `build\winwings-worker.exe`. (`make release`
works too if you have make, which Windows does not by default.)

## 0. Provision the host

Installs the runtimes and native dependencies game servers need, and applies the
Defender and performance settings that matter for this workload.

```powershell
# Elevated
.\scripts\provision-host.ps1
```

It installs Visual C++ redistributables (2008 through 2022, x86 and x64), Temurin
JREs 8/11/17/21, .NET 6 and 8 desktop runtimes, 7-Zip, Git, SteamCMD, Node, Python
and PowerShell 7 — then prints the `runtime.runtimes` block to paste into your
config.

Two things worth knowing about what it does:

- **Visual C++ redistributables are the most common cause of a native server
  refusing to start.** Source engine games need the *x86* 2013 runtime even on a
  64-bit host. They are small; the script installs every generation rather than
  leaving you to diagnose a missing DLL.
- **Defender exclusions are scoped to the server data tree only.** Real-time
  scanning of a world directory under continuous write is a large throughput
  cost, and Defender periodically quarantines legitimate server binaries as false
  positives. The rest of the host stays protected.

Use **Desktop Experience**, not Server Core. A number of game servers link
against GDI and user32 even when running headless.

Pass `-Java 17,21` to install fewer JVM versions, or `-SkipDefender` to manage
exclusions yourself.

## 1. Create the directory layout

```powershell
New-Item -ItemType Directory -Force C:\ProgramData\WinWings\logs,C:\ProgramData\WinWings\archives,C:\ProgramData\WinWings\backups,C:\ProgramData\WinWings\tmp
New-Item -ItemType Directory -Force D:\servers
Copy-Item build\wings.exe,build\winwings-worker.exe C:\ProgramData\WinWings\
```

`D:\servers` is `system.data`. Every server gets one directory tree beneath it,
named by its UUID, and deleting a server removes the whole tree. Put it on
whichever volume has the space; it must be **NTFS**, because the separation
between servers is enforced by NTFS ACLs and there is none on FAT32 or exFAT.

You do not need to set permissions on it. The daemon severs inheritance on each
server's directory as it creates it, and grants only that server's own account
access to its files.

## 2. Configure

Either run the Panel's node configuration command:

```powershell
C:\ProgramData\WinWings\wings.exe configure --panel-url https://panel.example.com --token <token> --node <id>
```

…or copy `config.example.yml` to `C:\ProgramData\WinWings\config.yml` and fill it
in.

Three settings the configure command knows nothing about:

- `system.data` — where servers live (`D:\servers` above).
- `system.timezone` — an IANA name (`Europe/London`, not `GMT Standard Time`).
  Windows and IANA name zones differently and Go ships no mapping, so an unset
  value falls back to UTC with a warning. The daemon embeds the IANA database,
  so any valid zone name works despite Windows not shipping one.
- `runtime.runtimes` — paste in the block that step 0 printed. Without it, an
  egg asking for `java-21` gets whichever JRE happens to be first on the host
  PATH.

Leave `system.account.isolation` at `managed` unless you have read the appendix
at the end of this document and decided otherwise.

Protect the file, which holds the Panel token:

```powershell
icacls C:\ProgramData\WinWings\config.yml /inheritance:r /grant:r "SYSTEM:F" "Administrators:F"
```

## 3. Install the service

From an **elevated** prompt:

```powershell
C:\ProgramData\WinWings\wings.exe service install --config C:\ProgramData\WinWings\config.yml
```

That one command creates everything the daemon needs to run:

| | |
|---|---|
| Account | `.\winwings`, created if absent |
| Password | randomly generated, handed to the service control manager, never written down and never shown |
| Groups | Administrators |
| Rights granted | Log on as a service, Replace a process level token, Adjust memory quotas for a process |
| Rights denied | Interactive logon, remote interactive logon |

Then:

```powershell
sc start winwings
C:\ProgramData\WinWings\wings.exe service status   # no elevation needed
```

`service uninstall` leaves the account in place, because removing the service is
usually a step in reinstalling it and deleting the account in between would
strip the ACLs naming it — every server's data directory would be left granting
access to a SID that no longer resolves. Pass `--remove-account` when you mean
it.

### Why the daemon is an administrator

Worth being explicit about rather than discovering later.

Under `managed` isolation the daemon creates a local account per server, grants
it the batch logon right, and rewrites NTFS ownership on that server's
directory. All three are privileged operations. So **a compromise of the daemon
is a compromise of the host**, and no amount of configuration changes that.

What the design buys is the case that actually happens. A game server runs
third-party code, faces the internet, and is exposed by whatever an egg's author
wrote; it is far likelier to be compromised than the daemon is. Every server
runs as its own unprivileged account, denied every form of logon except batch,
with an ACL on its directory that admits nothing else. A compromised server
reaches its own files and stops there.

It is still a dedicated account rather than LocalSystem, which is not cosmetic:
it can be audited in the security log, its rights can be listed and revoked, and
it is denied interactive logon. `wings service install` refuses LocalSystem, and
the daemon refuses to start as LocalSystem unless `system.account.allow_elevated`
is set.

If an administrative daemon is unacceptable on your host, see the appendix.

## 4. Verify

```powershell
C:\ProgramData\WinWings\wings.exe selftest --config C:\ProgramData\WinWings\config.yml
```

This does the real thing rather than inspecting configuration. It creates two
throwaway accounts, gives each a directory with the permissions a real server
gets, launches a process as each, and checks that one can write its own files,
cannot read the other's, and cannot rewrite its own `worker.json`. It also
exercises the Job Object memory and process limits, long path support and the
worker binary, then removes everything it created.

Run it elevated — unelevated, the account checks are skipped rather than failed.
Every check runs regardless of whether earlier ones failed, so one run tells you
everything that is wrong with the host:

```
Accounts and isolation
  PASS   account lifecycle                      created wwt-000000000000se01, wwt-000000000000se02
  PASS   NTFS permissions applied               each server's data directory admits only its own account
  PASS   logon as a server account              both accounts obtained a batch logon token
  PASS   run a process as a server account      a process launched as wwt-... reported itself as wwt-...
  PASS   server can write its own files         wrote and read back a file in its data directory
  PASS   server cannot read another server      wwt-... was denied D:\servers\...\data\secret.txt
  PASS   server cannot write its worker config  the server was denied write access to its own worker.json
  PASS   job object process limit               an active process limit stopped a second process from starting
  PASS   job object memory limit                a 256MB allocation was refused inside a 64MB job
```

`--keep` leaves the accounts and directories in place if you want to inspect
them; without it they are removed even when checks fail. A failed run exits
non-zero, so it can gate a provisioning script.

Then the general report, and the log:

```powershell
C:\ProgramData\WinWings\wings.exe diagnostics
Get-Content C:\ProgramData\WinWings\logs\wings.log -Wait -Tail 50
```

## Appendix: running without administrator rights

`managed` isolation trades an administrative daemon for automatic account
management. If that trade is wrong for your host, `pool` isolation reverses it:
you create the accounts and the daemon stays unprivileged.

There is still a floor. Launching a process as another local account — the
mechanism that isolates servers at all — requires two privileges an ordinary
user does not hold:

| Privilege | Name in secpol.msc |
|---|---|
| `SeAssignPrimaryTokenPrivilege` | Replace a process level token |
| `SeIncreaseQuotaPrivilege` | Adjust memory quotas for a process |

So a *completely* unprivileged daemon cannot isolate servers. The posture is a
dedicated account holding exactly those two and nothing else.

Create the daemon's account:

```powershell
$pw = [System.Web.Security.Membership]::GeneratePassword(32, 8)
New-LocalUser -Name winwings -Password (ConvertTo-SecureString $pw -AsPlainText -Force) `
  -PasswordNeverExpires -UserMayNotChangePassword -Description "win-wings daemon"
```

Grant it, in `secpol.msc` under Local Policies → User Rights Assignment:
**Replace a process level token**, **Adjust memory quotas for a process**, and
**Log on as a service**. Then give it its directories:

```powershell
icacls C:\ProgramData\WinWings /grant "winwings:(OI)(CI)M"
icacls D:\servers /grant "winwings:(OI)(CI)F"
```

Create one account per concurrent server, each with the **Log on as a batch job**
right, and list them under `system.account.accounts` with `isolation: pool`. A
node can then run at most as many servers as you created accounts; assignment is
by hash of the server UUID, so size the pool comfortably above your server count
to avoid two servers sharing an account.

Install the service against the account you made:

```powershell
C:\ProgramData\WinWings\wings.exe service install `
  --config C:\ProgramData\WinWings\config.yml `
  --account .\winwings --password <password>
```

Per-server network statistics need two further rights an ordinary account lacks:
membership of Performance Log Users, to start a trace session, and permission to
enable the kernel network provider, which by default only administrators hold.
Neither is needed to run servers; without them the daemon warns at boot and the
Panel's network graphs read zero. Grant both, elevated, with:

```powershell
C:\ProgramData\WinWings\wings.exe service allow-network-stats --account .\winwings
```

The daemon reports what it ended up with at boot, and refuses to start if
`isolation: pool` is set without those two privileges, rather than failing at the
first server start:

```
INFO  daemon security context  privileges=account=HOST\winwings can_launch_as_user=true
INFO  running unprivileged with the token-assignment rights needed to isolate servers
```

## Firewall

**The daemon does this for you.** When a server is created or started it writes
two inbound allow rules — TCP and UDP — covering exactly the ports the Panel
allocated it, and removes them when the server is deleted. Rules belonging to
servers this node no longer has are pruned at boot, which catches servers deleted
while the node was stopped.

The rules are named `win-wings-<uuid>-tcp` / `-udp`, and their description names
the server, so an unexplained open port in `wf.msc` can be traced without opening
the Panel:

```powershell
Get-NetFirewallRule -DisplayName 'win-wings-*' | Select-Object DisplayName, Enabled
```

Both protocols are opened for every allocation because a Pterodactyl allocation
does not record which one it is — a Source engine server wants UDP for game
traffic and TCP for RCON on the same number, and a Minecraft server wants TCP for
the game and UDP for query. Opening one and guessing wrong produces a server that
half works.

This needs administrator rights, which managed isolation already requires. Under
`pool` or `shared` isolation the daemon is unprivileged by design, so it reports
at boot that the firewall cannot be managed and you open the ports yourself.

To manage the rules elsewhere — group policy, or a configuration management tool
that would fight the daemon over them — turn it off:

```yaml
system:
  firewall:
    manage: false
    prune: true
```

Servers are then unreachable until something else opens their ports.

Note that the daemon opens the allocated ports; it does not stop a server binding
a different one. Nothing on Windows can, short of per-account outbound filtering.
A server that binds an unallocated port will find it unreachable from outside,
which is usually enough.

## Upgrading

Stop the service, replace both binaries, start it again. **Running servers are
not affected** — each is supervised by its own worker process, and the daemon
reconnects to them on startup, replaying the console output produced while it was
away.

The worker speaks a versioned protocol. A daemon that finds a worker speaking a
different version refuses to drive it rather than misinterpreting its messages,
so restart servers after a protocol change.

## Troubleshooting

**`winwings-worker.exe was not found next to the daemon`** — both binaries must
sit in the same directory.

**Anything at all, before you read further** — run `wings.exe selftest` from an
elevated prompt. It reproduces most of what follows and names the cause.

**`isolation is "managed" ... does not have administrator rights`** — the
service is running as an account that cannot create the per-server accounts.
Reinstall it with `wings.exe service install`, or switch to `pool` isolation and
follow the appendix.

**`was missing SeAssignPrimaryTokenPrivilege ... restarted`** — the daemon
granted itself the rights it needed. Windows applies account rights at logon, so
`sc stop winwings && sc start winwings` and it will come up.

**`refusing to run as LocalSystem`** — reinstall the service with an account of
its own. Override with `system.account.allow_elevated` only if you accept that a
compromised daemon owns the host outright.

**`the account "ww-..." already exists but was not created by this daemon`** — a
name collision with an account of yours. Rename yours, or set
`system.account.prefix` to something that does not collide.

**`system.account.isolation is "pool" but no accounts are configured`** — see the
appendix, or set `isolation: managed` and let the daemon create them.

**Server fails to start with a logon error** — under `pool` isolation, the
*server* account lacks *Log on as a batch job*; under `managed`, run `selftest`,
which exercises exactly this.

**Console is empty for a steamcmd-based server** — that class of process detects
a non-console stdout and drops output. Set `pseudo_console` on the egg's Windows
profile. Note that ConPTY is currently unverified; see
`internal/winproc/conpty_diag_test.go`.

**Install output arrives all at once at the end** — the installer's C runtime
switches stdout from line to full buffering when it is not a console, so the
output is not lost, just delivered in blocks. Setting
`console.install_pseudo_console` gives live output instead, at the cost of
ConPTY, which is not yet dependable: on some hosts the child dies in the loader
with `0xC0000142` having run nothing. It is off by default for that reason.

**Server starts but ignores its port** — expected. Nothing enforces the
allocation; the egg's startup command must pass `{{SERVER_IP}}` and
`{{SERVER_PORT}}` through.
