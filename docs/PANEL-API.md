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

`working_dir`, `console`, `pre_start_script` and `pre_stop_script` may also be
present. All are for the handful of eggs that need them and are described under
[Consoles that are not stdio](#consoles-that-are-not-stdio),
[Pre-start scripts](#pre-start-scripts) and [Pre-stop scripts](#pre-stop-scripts);
omitting them is the normal case.

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
| `working_dir` | string | Where the server process is started, relative to the server's data directory, and what the startup command's own relative paths resolve against. Empty is the data directory itself. Supports `{{VAR}}` and `${VAR}`. See below. |
| `pseudo_console` | bool | Allocate a ConPTY rather than pipes. Only for processes that detect a non-console stdout — steamcmd being the usual case. |
| `console` | object | Where console output is read from and where commands are written to. Omit for the ordinary arrangement, which is the process's own stdio. |
| `pre_start_script` | string | PowerShell run to completion before every boot. Omit or leave empty for none. |
| `pre_stop_script` | string | PowerShell run to completion when a stop is requested, before `stop` is tried. Omit or leave empty for none. |

Omitting `stop` entirely uses the egg's standard stop configuration.

#### Working directories

Every server is started in its data directory, `<data>/<uuid>/data`, which is the
only part of its tree the server's own account can write. `working_dir` moves the
*server process* into a subdirectory of that, and nothing else with it.

It exists for the games that do not ask where they are. A server that writes its
logs to `..\Logs` is computing a path from the directory it was started in;
started from the data directory that resolves to `<data>/<uuid>`, the server's
root, which the account is denied and must stay denied — `worker.json` lives
there, and a server able to write it can rewrite the command its own supervisor
executes. Naming the subdirectory the game was installed into moves the whole
computation back inside the sandbox, with no permission loosened anywhere.

Three consequences worth knowing:

- The startup command's relative paths resolve against it. With a `working_dir`
  of `ServerFile`, the startup command is `MyServer.exe`, not
  `ServerFile\MyServer.exe`.
- Pre-start commands do not use it. A steamcmd update and the egg's
  `pre_start_script` both run in the data directory, because they are what
  install the content `working_dir` names — on a fresh server it does not exist
  until they have run.
- The console log source does not use it either. `console.source.path` stays
  relative to the data directory, so moving the process does not silently move
  where the daemon looks for the log.

The daemon substitutes variables into it and refuses anything resolving outside
the server's directory, falling back to the data directory and logging the
reason. The worker checks again before launching, and refuses the start if the
directory does not exist by then.

#### Stopping a server

Three mechanisms exist, and they are not equivalent.

| `stop.type` | `stop.value` | What the daemon does |
|---|---|---|
| `command` | the text | Writes it to stdin. The right answer wherever the server has a console command. |
| `signal` | empty | Raises a real `CTRL_C_EVENT` on the server's console — what pressing Ctrl+C in a terminal does. Most console servers shut down cleanly on it. Needs no pseudo console. |
| `signal` | `ctrl_break`, `break`, `sigquit` | Raises `CTRL_BREAK_EVENT` instead. Weaker: fewer programs handle it, and those that do often treat it as "dump state and continue". Only ask for this if Ctrl+C is known not to work. |
| omitted, or unrecognised | — | The server is killed. |

Every attempt escalates on a timeout: the egg's [pre-stop script](#pre-stop-scripts)
if it has one, then the chosen mechanism, then CTRL_BREAK, then terminating the
Job Object. Each step and its outcome is logged, including how long the server
was given.

An unmodified egg carrying a POSIX signal name needs no special handling: any
`signal` stop becomes a Ctrl+C, which is the closest thing Windows has and is
what such an egg meant. Only reach for the break spellings to override that.

#### Consoles that are not stdio

A few games write their log only to a file and take commands only on a TCP port
they open themselves. Their Linux eggs deal with this in the startup line, which
stops being a command and becomes a shell pipeline:

```sh
wine EmpyrionDedicated.exe -logFile ../Logs/server/server.log & PID=$! ;
tail -c0 -F ../Logs/server/server.log &
until nc -z 127.0.0.1 21004; do sleep 1; done ;
telnet -E 127.0.0.1 21004 ;
wait $PID
```

There is no shell here and a startup command is executed directly, so this is
declared rather than run. Both halves are independent and both default to the
process's own stdio:

```json
"console": {
  "source": {
    "type": "file",
    "path": "Logs/server/server.log",
    "encoding": "utf-8"
  },
  "commands": {
    "type": "telnet",
    "host": "127.0.0.1",
    "port": "{{TELNET_PORT}}",
    "password": "{{TELNET_PASSWORD}}",
    "connect_timeout_seconds": 300
  }
}
```

| Field | Type | Meaning |
|---|---|---|
| `source.type` | string | `file` to follow a log file. Omitted or empty reads only the process's own output. |
| `source.path` | string | The log file, **relative to the server's data directory**. Supports `{{VAR}}` and `${VAR}`. Either separator. An absolute path, a UNC path, or one climbing out of the directory is refused. |
| `source.encoding` | string | `utf-8` (the default), `utf-16le` or `utf-16be`. |
| `commands.type` | string | `telnet` for a TCP console. Omitted or empty writes to the process's stdin. |
| `commands.host` | string | Empty means `127.0.0.1`. |
| `commands.port` | string | A **string**, so it can carry `{{TELNET_PORT}}` or whichever variable the egg uses. Substituted and parsed by the node. |
| `commands.password` | string | Sent as the first line after connecting, when set. Substituted. |
| `commands.connect_timeout_seconds` | int | How long the node waits for the port to open. Zero means five minutes. |

Things worth knowing before writing one:

- **The game process is still the process.** Neither of these puts anything
  between the daemon and the server: the exit code, the crash detection and the
  resource limits are the game's, exactly as for any other egg. This is the
  reason it is declared here rather than being a scripted wrapper.
- **The log is followed by name.** A log the game deletes and recreates on boot
  is followed into the new file, and content already in the file when the server
  starts is skipped, so a restart does not replay the previous run.
- **The process's own output is still streamed.** These games usually say nothing
  on stdout, but when they do — a crash before the log is open, a licence
  complaint — that is exactly the output somebody is looking for.
- **Everything the TCP console sends back goes on the console**, its banner
  included. That is how you tell a connected channel from a silent one.
- **A server with a command channel must have a `command` stop.** The stop is
  written to the channel; a `signal` stop has no console to interrupt, so the
  node would kill the server rather than asking it to save.
- **The channel is never mixed with stdin.** A command that cannot be delivered
  is reported on the server console rather than written to a stdin the game is
  not reading, where it would vanish and look successful.
- **Nothing here fails a boot.** A port that cannot be parsed or a log path that
  is refused leaves a server that runs, with the reason in the node's log. A
  server that is up and uncommandable is easier to diagnose than one that will
  not start.

#### Pre-start scripts

`pre_start_script` is PowerShell run to completion before every boot, in the
server's data directory, as the server's own account, with its output on the
console. It is the counterpart to the install script, and it is for preparing a
server rather than running one: writing a configuration file out of the egg's
variables on every boot is the case it exists for.

- It runs **after** any steamcmd update, so it can patch a file the update has
  just replaced.
- The working directory is the server's data directory, also exported as
  `SERVER_DIR`. The egg's variables are in the environment, as they are during an
  install.
- The daemon stages it into the server root, which the server can read and cannot
  write. A script inside the server's own directory would be a server rewriting
  what its next boot executes.
- A non-zero exit is logged and the server starts anyway. A server running with
  a stale configuration is more use than one that refuses to boot, and the
  operator can see both.

It does not replace the startup command, and a script that tries to launch the
game itself will have it killed: the pre-start step is waited for, and the server
is only launched once it exits.

#### Pre-stop scripts

`pre_stop_script` is PowerShell run to completion when the server is asked to
stop, before the profile's `stop` mechanism is tried, in the server's data
directory, as the server's own account, with its output on the console. It is
for the servers whose clean shutdown is neither a line on stdin nor a console
interrupt: an RCON command, a call to a web endpoint, a save that has to be
asked for first. Their Linux eggs did this in a shell `trap` wrapped around the
game; there is no shell around the game here, so the daemon runs the script
instead.

- The server is still running when it starts. Its process id is exported as
  `SERVER_PID`, so the script can wait for it to go away after asking it to.
  A script that returns while the server is still up has not stopped it.
- The environment is the pre-start script's — `SERVER_DIR` and the egg's
  variables, rebuilt at stop time so a variable edited while the server ran is
  the value the script sees.
- If the server has exited by the time the script returns, the stop is done
  and nothing escalates. Otherwise `stop` follows as if the script had not run,
  with the usual escalation after it.
- It is bounded by the stop timeout. A script still running when that elapses
  is killed and the stop carries on without it, so a script that hangs cannot
  make a server impossible to stop. A non-zero exit is logged and treated the
  same way.
- It is staged into the server root beside the pre-start script, for the same
  reason.
- It is **not** run when a server is killed, nor when a stop arrives while the
  pre-start commands are still running: there is no server to talk to yet, and
  the run is simply ended.

A pre-stop script is a better first attempt, not the only one. Keep a `stop`
configured as well: it is what the daemon reaches for when the script fails,
and what a crash-restart cycle or a node shutdown with a shorter deadline gets.

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
