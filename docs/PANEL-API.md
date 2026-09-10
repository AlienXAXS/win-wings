# Panel API contract for the win-wings Blueprint plugin

This is the API the daemon expects a Blueprint plugin to serve. It was derived
from the finished daemon implementation rather than designed up front, so every
field here exists because something in the daemon consumes it.

The Panel itself is **not** forked. Everything below is served by the plugin.

## Why this exists

An egg carries a Linux install script, a Linux startup command, a Docker image,
and a stop configuration that may be a POSIX signal. On Windows:

- The install script must be PowerShell.
- The container image means nothing; what matters is what runtime is on the host.
- The startup command usually needs rewriting — different binary names,
  backslash paths, and no shell, so `&&`, `|` and `>` are not interpreted.
- A signal-based stop cannot work as written. Windows has no signals; the
  nearest equivalent is a console interrupt, which has to be asked for
  explicitly and needs a console to be delivered through.

None of that fits in the existing egg schema, and the daemon cannot guess it.

## Transport and authentication

The daemon builds its base URL as `<panel>/api/remote` and authenticates with the
node's existing credentials:

```
Authorization: Bearer <token_id>.<token>
```

**Register these routes on the Panel's existing node-authentication middleware
group.** Do not invent a separate token: that would add a second credential to
provision and rotate per node, and a new unauthenticated surface if it were ever
misconfigured. The node token the daemon already holds is the right one.

## Endpoints

### `GET /api/remote/windows/ping`

Liveness check for the plugin itself. Called once at daemon boot when
`runtime.require_windows_profile` is enabled.

Any `2xx` response is success; the body is ignored. A `404` makes the daemon
refuse to start, with a message telling the operator to install the plugin.

This is the fail-closed check: without it a node would come up looking healthy
and then fail on every individual server.

### `GET /api/remote/windows/servers/{uuid}/profile`

Returns the Windows profile for the egg the given server uses.

**200 response:**

```json
{
  "runtime": "jdk-21",
  "startup": "java -Xms128M -Xmx{{SERVER_MEMORY}}M -jar server.jar nogui",
  "stop": {
    "type": "command",
    "value": "stop"
  },
  "pseudo_console": false
}
```

**404 response** means no profile is configured for this egg. What the daemon
does then depends on `runtime.require_windows_profile`:

- `true` (production) — the server is not initialised at all, with an error
  naming the egg. Better than a server that appears to install and then fails
  obscurely.
- `false` (bring-up) — the daemon falls back to the egg's standard fields and
  logs a debug note. Useful only for testing a node against an unmodified Panel.

A transport error is never fatal: the daemon logs a warning and continues, so a
plugin outage cannot stop a node from booting servers it already knows about.

#### Fields

| Field | Type | Meaning |
|---|---|---|
| `runtime` | string | Which runtime this egg needs — see below. Empty falls back to the egg's `container_image`. |
| `startup` | string | Windows startup command. Empty uses the Panel's standard startup value. Supports `{{VAR}}` and `${VAR}`. |
| `stop.type` | string | `command` or `signal`. |
| `stop.value` | string | For `command`, the text written to stdin (`stop`, `end`, `quit`). For `signal`, leave empty to get a Ctrl+C interrupt, or name a break (`ctrl_break`, `break`, `sigquit`) to get CTRL_BREAK instead. |
| `pseudo_console` | bool | Allocate a ConPTY rather than pipes. Only for processes that detect a non-console stdout — steamcmd being the usual case. |

Omitting `stop` entirely uses the egg's standard stop configuration.

#### Stopping a server

Three mechanisms exist, and they are not equivalent.

| `stop.type` | `stop.value` | What the daemon does |
|---|---|---|
| `command` | the text | Writes it to stdin. The right answer wherever the server has a console command. |
| `signal` | empty | Raises a real `CTRL_C_EVENT` on the server's console — what pressing Ctrl+C in a terminal does. Most console servers shut down cleanly on it. Needs no pseudo console. |
| `signal` | `ctrl_break`, `break`, `sigquit` | Raises `CTRL_BREAK_EVENT` instead. Weaker: fewer programs handle it, and those that do often treat it as "dump state and continue". Only ask for this if Ctrl+C is known not to work. |
| omitted, or unrecognised | — | The server is killed. |

Every attempt escalates on a timeout: the chosen mechanism, then CTRL_BREAK,
then terminating the Job Object. Each step and its outcome is logged, including
how long the server was given.

An unmodified egg carrying a POSIX signal name needs no special handling: any
`signal` stop becomes a Ctrl+C, which is the closest thing Windows has and is
what such an egg meant. Only reach for the break spellings to override that.

#### Runtime names

`runtime` is the Windows counterpart to an egg's container image, and it is
resolved rather than merely passed through. The node's `config.yml` maps names to
directories:

```yaml
runtime:
  runtimes:
    java-8:  'C:\Program Files\Eclipse Adoptium\jre-8.0.504.1-hotspot'
    java-17: 'C:\Program Files\Eclipse Adoptium\jre-17.0.20.101-hotspot'
    java-21: 'C:\Program Files\Eclipse Adoptium\jre-21.0.12.101-hotspot'
```

When a server starts, the matching `bin` directory is **prepended to its PATH**
and exported as `RUNTIME_PATH`, for both the server process and its install
script. A startup command saying `java` therefore gets the version the egg asked
for, not whichever JRE was installed last. This is necessary because several Java
versions coexist on one host, where a Linux node would have used a different
container image per egg.

The names are a convention between the plugin and the node operator, not
something the daemon defines. `java-8`, `java-11`, `java-17`, `java-21`,
`dotnet-8` are what `scripts/provision-host.ps1` emits. A name the node does not
recognise is not an error — the server simply inherits the host PATH, which is
correct for eggs needing no runtime.

Matching is case-insensitive.

The plugin should offer this as a dropdown rather than free text, since a typo
silently produces a server that inherits the wrong Java.

### `GET /api/remote/windows/servers/{uuid}/install`

Optional. If you do not implement it, the daemon uses the standard
`/servers/{uuid}/install` endpoint and the plugin must instead ensure the
script served there is PowerShell for Windows nodes.

Same response shape as the standard endpoint:

```json
{
  "container_image": "jdk-21",
  "entrypoint": "powershell",
  "script": "# PowerShell...\n"
}
```

`entrypoint` is ignored — the daemon always runs PowerShell. `container_image`
is surfaced to the script as `INSTALL_RUNTIME`.

## How install scripts differ

The daemon writes the script to a private temp directory and runs it as:

```
pwsh.exe -NoProfile -NonInteractive -NoLogo -ExecutionPolicy Bypass -File install.ps1
```

PowerShell 7 is used when present, falling back to Windows PowerShell 5.1.

The script runs **inside a Job Object** with the installer resource limits
applied, and **as the server's own account** when account isolation is
configured. It is not a container, and it is not privileged.

Environment differences from a Linux egg:

| Linux | Windows |
|---|---|
| `/mnt/server` bind mount | `$env:SERVER_DIR`, which is also the working directory |
| `apt-get`, `curl`, `wget`, `tar` | none of these exist; use `Invoke-WebRequest`, `Expand-Archive` |
| runs as root in a container | runs as the server's unprivileged account |
| container image provides the runtime | `$env:INSTALL_RUNTIME` names what is expected; the script must verify it |

Every standard egg variable is present, plus `SERVER_DIR`, `INSTALL_RUNTIME`
and `STEAMCMD_DIR`. A non-zero exit fails the installation and the output is written to
`<log_directory>\install\<uuid>.log` as well as streamed to the Panel.

`docs\eggs\TEMPLATE.install.ps1` is a working skeleton to start from, and
`docs\eggs\starrupture\install.ps1` a complete egg built on it.

**Do not pipe a native command's output through PowerShell.** The script is given
a pseudo console so that installers stream their progress live
(`console.install_pseudo_console`, on by default). Writing

```powershell
& $exe @args | ForEach-Object { Write-Host $_ }   # and likewise 2>&1 |, *> file
```

hands the child a pipe instead, and a C runtime that finds stdout is not a
character device switches from line buffering to full buffering. Nothing is
lost, but a twenty-minute steamcmd download shows an empty console and then
every progress line at once at the end — which is indistinguishable from an
install that has hung. Call the executable and let it write to the console:

```powershell
& $exe @args
$rc = $LASTEXITCODE
```

If the script needs to react to something the installer said, prefer the state it
leaves on disk over matching its output; that is what
`docs\eggs\starrupture\install.ps1` does for steamcmd's install-path refusal.

## Steam games: updating before start

The Linux steamcmd images did one more thing in their entrypoint: when
`AUTO_UPDATE` was set they ran `steamcmd +app_update` before handing over to
the startup command. The daemon does the same, without any profile field.

A server is treated as a Steam game when `steamcmd.exe` exists in
`$env:STEAMCMD_DIR`, a per-server directory the daemon creates beside the
server's files and hands to both the install script and the server. The
install script should download and extract steamcmd there:

```powershell
Invoke-WebRequest https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip -OutFile "$env:TEMP\steamcmd.zip"
Expand-Archive "$env:TEMP\steamcmd.zip" -DestinationPath $env:STEAMCMD_DIR -Force
& "$env:STEAMCMD_DIR\steamcmd.exe" +force_install_dir $env:SERVER_DIR +login anonymous +app_update $env:SRCDS_APPID validate +quit
```

The Linux layout, steamcmd inside the server directory updating that same
directory, does **not** work on Windows. Steamcmd refuses to install into its
own folder or any folder above it, prints "Please set the game install path to
something other than the Steam install folder", ignores the directive and
installs into its own folder instead. A steamcmd found at `steamcmd\steamcmd.exe`
inside the server directory is therefore ignored, with a warning in the daemon
log saying where it should be.

For a Steam server, every start first runs an update when `AUTO_UPDATE` is
`1`, `true`, `yes` or `on`. The update runs under the server's account, inside its job, with
its output on the console, and the server starts once it finishes. A stop
request during the update kills the update and the server does not start.

The command is built from the variables the standard steamcmd eggs already
define:

| Variable | Use |
|---|---|
| `AUTO_UPDATE` | Whether to update at all. Anything but a positive value skips it. |
| `SRCDS_APPID`, or `STEAM_APPID` | The app to update. Required; without one the update is skipped and logged. |
| `STEAM_USER`, `STEAM_PASS`, `STEAM_AUTH` | Login. Empty or `anonymous` logs in anonymously. |
| `SRCDS_BETAID`, `SRCDS_BETAPASS` | Beta branch and its password. |
| `INSTALL_FLAGS` | Extra `app_update` arguments, split with command-line rules. |
| `VALIDATE` | Adds `validate` when positive. |

The install directory is always the server's own directory. `WINDOWS_INSTALL`
is ignored: steamcmd on Windows fetches the Windows build by default.

Passwords are masked in the daemon's log and the worker does not log the
command at all, but steamcmd itself may echo what it is given.

## Suggested plugin behaviour

**Gate server creation.** The daemon refuses an egg with no profile, but by then
the Panel has already created the record and the user sees a server stuck in
"installing". Blocking the egg at selection time on a Windows node turns that
into a comprehensible "this egg is not available on Windows nodes". This is the
highest-value UI injection.

**Mark nodes as Windows.** The plugin needs to know which nodes are Windows to
apply the gate. A node-level flag is the simplest approach.

**Hide meaningless fields.** Swap, OOM-killer toggle, CPU pinning and the Docker
image selector map to nothing here. Cosmetic, and can come later.

## Things the daemon does not ask for

Deliberately, so the plugin stays small:

- **Runtime installation.** The daemon does not download or manage JREs. The
  install script owns that.
- **Port allocation enforcement.** Nothing binds ports on a server's behalf on
  Windows; allocations are passed through as `{{SERVER_IP}}`/`{{SERVER_PORT}}`
  and the server is trusted to honour them.
- **Account management.** Windows accounts are host configuration, created at
  install time. See [DEPLOYMENT.md](DEPLOYMENT.md).
