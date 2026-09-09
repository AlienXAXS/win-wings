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
.uild.ps1 -Release
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
New-Item -ItemType Directory -Force C:\ProgramData\WinWings\{logs,volumes,instances,archives,backups,tmp,secrets}
Copy-Item build\wings.exe, build\winwings-worker.exe C:\ProgramData\WinWings\
```

`instances` must not sit inside `volumes`. It holds each server's resolved
startup command, and a server able to write there could rewrite what its own
worker executes. The daemon logs an error at boot if the two overlap.

## 2. Create the server accounts

This is the step that determines whether servers are isolated from each other.
Skipping it means every server runs as the daemon's account and can read every
other server's files *and* your Panel token.

Create one account per concurrent server you intend to run:

```powershell
# Repeat for srv02, srv03, ...
$pw = [System.Web.Security.Membership]::GeneratePassword(32, 8)
New-LocalUser -Name winwings-srv01 -Password (ConvertTo-SecureString $pw -AsPlainText -Force) `
  -PasswordNeverExpires -UserMayNotChangePassword `
  -Description "win-wings server account"
Set-Content -Path C:\ProgramData\WinWings\secrets\srv01 -Value $pw -NoNewline
```

Remove them from `Users` so they cannot read the rest of the system:

```powershell
Remove-LocalGroupMember -Group Users -Member winwings-srv01
```

Grant each account the **Log on as a batch job** right (`SeBatchLogonRight`).
There is no PowerShell cmdlet for this; use `secpol.msc` → Local Policies → User
Rights Assignment, or export and edit with `secedit`.

Restrict the secrets directory to the daemon's account only:

```powershell
icacls C:\ProgramData\WinWings\secrets /inheritance:r /grant:r "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F"
```

## 3. Lock down the servers root

Set the top of the tree so server accounts cannot roam it:

```powershell
icacls D:\servers /inheritance:r `
  /grant:r "SYSTEM:(OI)(CI)F" "Administrators:(OI)(CI)F"
```

**Per-server permissions are applied by the daemon**, not by you. When a server
is created it severs inheritance on `<uuid>\` and grants that server's assigned
account exclusive write access to `<uuid>\data` only — so its `worker.json`,
which holds the command its worker executes, stays out of its own reach.

If the daemon lacks the rights to do that it logs a warning per server and
carries on; the servers run, but they are not isolated from one another.

Also protect the configuration file, which holds the Panel token:

```powershell
icacls C:\ProgramData\WinWings\config.yml /inheritance:r `
  /grant:r "SYSTEM:F" "Administrators:F"
```

## 4. Configure

Either run the Panel's node configuration command:

```powershell
C:\ProgramData\WinWings\wings.exe configure --panel-url https://panel.example.com --token <token> --node <id>
```

…or copy `config.example.yml` to `C:\ProgramData\WinWings\config.yml` and fill it
in. Either way you must then add the `system.account` block by hand — the
configure command knows nothing about Windows accounts.

Set `system.timezone` to an IANA name (`Europe/London`, not `GMT Standard Time`).
Windows and IANA name zones differently and Go ships no mapping, so an unset
value falls back to UTC with a warning. The daemon embeds the IANA database, so
any valid zone name works despite Windows not shipping one.

Paste in the `runtime.runtimes` block that step 0 printed. Without it, an egg
asking for `java-21` gets whichever JRE happens to be first on the host PATH.

## 5. Create the service account

**The daemon must not run as LocalSystem.** It executes egg install scripts and
supervises game servers, both third-party code; a compromise of either should not
yield the host. `wings.exe service install` refuses LocalSystem, and the daemon
refuses to start elevated.

There is a genuine tension to understand here. Launching a process as another
local account — the mechanism that isolates servers from one another — requires
two privileges an ordinary user does not hold:

| Privilege | Name in secpol.msc |
|---|---|
| `SeAssignPrimaryTokenPrivilege` | Replace a process level token |
| `SeIncreaseQuotaPrivilege` | Adjust memory quotas for a process |

So a *completely* unprivileged daemon cannot isolate servers at all. The correct
posture is a dedicated account holding exactly those two and nothing else — far
less dangerous than SYSTEM, and sufficient.

```powershell
$pw = [System.Web.Security.Membership]::GeneratePassword(32, 8)
New-LocalUser -Name winwings -Password (ConvertTo-SecureString $pw -AsPlainText -Force) `
  -PasswordNeverExpires -UserMayNotChangePassword -Description "win-wings daemon"
Remove-LocalGroupMember -Group Users -Member winwings
```

Grant it, in `secpol.msc` under Local Policies → User Rights Assignment:

- **Replace a process level token**
- **Adjust memory quotas for a process**
- **Log on as a service**

Then give it ownership of its own directories:

```powershell
icacls C:\ProgramData\WinWings /grant "winwings:(OI)(CI)M"
icacls D:\servers /grant "winwings:(OI)(CI)F"
```

The daemon reports what it ended up with at boot:

```
INFO  daemon security context  privileges=account=HOST\winwings can_launch_as_user=true
INFO  running unprivileged with the token-assignment rights needed to isolate servers
```

If those privileges are missing and `isolation: pool` is configured, it refuses
to start and names exactly what to grant, rather than failing at the first server.

## 6. Install the service

From an **elevated** prompt — the install needs elevation, the service itself
must not have it:

```powershell
C:\ProgramData\WinWings\wings.exe service install `
  --config C:\ProgramData\WinWings\config.yml `
  --account .\winwings --password <password>
sc start winwings
```

Check it (no elevation needed):

```powershell
C:\ProgramData\WinWings\wings.exe service status
```

`--allow-system` exists to override the refusal, and requires
`system.account.allow_elevated: true` in the config to match. Only for a
single-tenant node where every server is already trusted.

## 7. Verify

```powershell
C:\ProgramData\WinWings\wings.exe diagnostics
```

Reports the Windows build, the runtime configuration, whether the worker binary
was found, and which servers currently have a live worker.

Watch the log while starting a server:

```powershell
Get-Content C:\ProgramData\WinWings\logs\wings.log -Wait -Tail 50
```

## Firewall

Nothing publishes ports on a server's behalf. Under Docker, only the ports the
Panel allocated were reachable; here a server binds whatever it asks for.

Constrain that per account:

```powershell
New-NetFirewallRule -DisplayName "win-wings srv01" -Direction Inbound `
  -Program C:\ProgramData\WinWings\volumes\<uuid>\server.exe -Action Allow
```

Or set a default-deny inbound policy and allow only the allocated ports.

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

**`system.account.isolation is "pool" but no accounts are configured`** — see
step 2, or set `isolation: shared` and accept that servers are not isolated.

**`refusing to run as ...`** — the daemon is LocalSystem, an Administrators
member, or elevated. See step 5. Override with `system.account.allow_elevated`
only if you accept the consequence.

**`isolation is "pool" but this account is missing ...`** — grant *Replace a
process level token* and *Adjust memory quotas for a process* in `secpol.msc`,
then restart the service.

**Server fails to start with a logon error** — the *server* account lacks *Log on
as a batch job* (step 2), or the *daemon* account lacks the two privileges above
(step 5).

**Console is empty for a steamcmd-based server** — that class of process detects
a non-console stdout and drops output. Set `pseudo_console` on the egg's Windows
profile. Note that ConPTY is currently unverified; see
`internal/winproc/conpty_diag_test.go`.

**Server starts but ignores its port** — expected. Nothing enforces the
allocation; the egg's startup command must pass `{{SERVER_IP}}` and
`{{SERVER_PORT}}` through.
