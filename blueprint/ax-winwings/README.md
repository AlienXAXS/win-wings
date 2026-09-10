# Ax Win-Wings

Serves the Panel API that [win-wings](https://github.com/AlienXAXS/win-wings) —
the Windows-native fork of wings — needs in order to run against an unmodified
Pterodactyl Panel.

An egg carries a bash install script, a Linux startup command, a Docker image
and, often, a signal-based stop. On a Windows host none of those mean anything:
there is no container, no shell interpreting `&&`, and no signals to send. This
extension holds the missing half — a **Windows profile** per egg — and serves it
to nodes over the Panel's existing node-authenticated remote API.

The contract implemented here is `docs/PANEL-API.md` in the win-wings repository.

## What it serves

| Route | Purpose |
|---|---|
| `GET /api/remote/windows/ping` | Liveness. A node with `runtime.require_windows_profile: true` refuses to boot on a 404, so a node can never come up looking healthy and then fail server by server. |
| `GET /api/remote/windows/servers/{uuid}/profile` | The egg's Windows profile. A 404 is a real answer: it tells the node to refuse the server rather than guess at Linux defaults. |
| `GET /api/remote/windows/servers/{uuid}/install` | The optional install endpoint from the contract. The shipping daemon does not use it; it is served anyway. |
| `GET /api/remote/servers/{uuid}/install` | **The Panel's own route, replaced.** This is the one the daemon actually reads, so it is where a Windows node gets PowerShell instead of bash. Non-Windows nodes get byte-for-byte what the core controller returns. |

All four sit on the Panel's existing `daemon` middleware group and authenticate
with the node token the daemon already holds. There is deliberately no second
credential: one more secret per node is one more thing to provision, rotate, and
eventually misconfigure into an unauthenticated surface.

## A profile

Per egg, and every field falls back to the egg on its own:

- **Runtime** — names an entry in the node's `runtime.runtimes` map in
  `config.yml`. The daemon resolves it to a directory, puts its `bin` first on
  the server's PATH and exports `RUNTIME_PATH`. This is how several Java versions
  coexist on one host where a Linux node would have used a different container
  image per egg. Empty means the egg needs nothing on PATH.
- **Startup** — the Windows command line. Executed directly, not through a
  shell, so `&&`, `|` and `>` are not interpreted. Both `{{VAR}}` and `${VAR}`
  are substituted. Empty uses the egg's own line.
- **Stop** — `command` (text written to stdin) or `signal`. Windows has no
  signals, but the daemon can deliver one interrupt: a `signal` stop with the
  value `ctrl_c` writes `0x03` to the server's console input and the console
  driver raises a real `CTRL_C_EVENT`. That **requires the pseudo console**, so
  the editor refuses the combination without it — with no console there is
  nothing to translate the byte and the stop silently degrades to a kill.
  A stop command is preferred wherever the server has one, because it needs no
  pseudo console. Omitted entirely means inherit the egg's stop configuration,
  which for a POSIX-signal egg means the server is killed.
- **ConPTY** — allocate a pseudo-console rather than pipes. Only for a process
  that inspects its own stdout and behaves differently when it is not a console;
  steamcmd is the usual one. It costs a stream of VT escapes in place of clean
  lines, so it is off by default.
- **Install script** — PowerShell, served in place of the egg's own script. The
  egg is never modified, so it keeps working on Linux nodes.

A profile can be turned off. A disabled profile is deliberately indistinguishable
from a missing one — "off" has to mean the node refuses the server, not that it
runs half-configured.

## Install-script environment

The daemon writes the script to a private temp directory and runs it as:

```
pwsh.exe -NoProfile -NonInteractive -NoLogo -ExecutionPolicy Bypass -File install.ps1
```

inside a Job Object with the installer resource limits applied, as the server's
own account. It is not a container and it is not privileged.

| Linux egg | Here |
|---|---|
| `/mnt/server` bind mount | `$env:SERVER_DIR`, also the working directory |
| `apt-get`, `curl`, `wget`, `tar` | none of these exist — `Invoke-WebRequest`, `Expand-Archive` |
| runs as root in a container | runs as the server's unprivileged account |
| the image provides the runtime | `$env:INSTALL_RUNTIME` names what is expected; `$env:RUNTIME_PATH` points at it |

Every standard egg variable is present. A non-zero exit fails the installation.

The profile editor ships starter scripts for the shapes that come up repeatedly
(single download, archive, Java, SteamCMD, .NET) with the environment differences
already handled.

## Windows nodes

Nothing but win-wings ever calls `/api/remote/windows/`, so the first such call
from a node is proof of what it runs, and the node is flagged automatically.
A node you have switched off by hand is never switched back on. The flag lives in
this extension's own table — no column is added to `nodes`, so removal is clean
and a panel upgrade cannot collide with it.

## The egg gate

Optional, and on by default. Creating a server on a Windows node with an egg that
has no enabled profile is refused before anything is written. The daemon refuses
it in any case, but by then the record exists and the person who asked for it is
watching a server that says "installing" and never will.

## What it patches

Blueprint can only give an extension routes under `/extensions/<id>`, and the
daemon's URLs are fixed. So `data/install.sh` makes two edits, and `remove.sh`
takes both back out:

1. **`routes/api-remote.php`** — one appended line loading this extension's route
   file. Required. Being appended is load-bearing: the install-endpoint override
   depends on being registered after the core route.
2. **`app/Services/Servers/ServerCreationService.php`** — one line calling the
   egg gate. Optional; the installer warns and carries on if the hook point has
   moved.

**A panel upgrade will revert both.** The extension's admin page checks all three
things — the routes, the override, and the gate — and says which are missing.
Re-run `blueprint -install axwinwings` to restore them.

If the panel has a compiled route cache, both scripts rebuild it. If it does not,
they do not create one.

## Coexistence

The `ServerCreationService.php` patch is a different line from the one Ax Port
Manager uses, so the two coexist. Auto Allocation Manager rewrites that file more
aggressively; it has not been tested alongside this.

## Requirements

- Pterodactyl Panel with Blueprint (`beta-2026-01`)
- At least one node running win-wings

## Known limits

- **The gate only covers creation.** Changing an existing server's egg to an
  unprofiled one, on a Windows node, is not blocked — the daemon refuses the
  server at its next boot instead. The admin page's "Servers with no profile"
  check finds these.
- **Profiles are per egg, not per node.** A profile describes how the software
  runs on Windows, which does not vary between two Windows nodes. If it ever
  needs to, that is a second table rather than a wider key.
- **Backups and transfers between Linux and Windows nodes do not work**, which is
  a daemon-side limitation and nothing this extension can address.
