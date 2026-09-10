# win-wings

A Windows-native fork of Pterodactyl wings. No Docker: servers run as real
Windows processes under Job Objects, isolated by separate accounts and NTFS
ACLs. The Panel is unmodified — see `docs/PANEL-API.md` for the contract.

## Converting egg install scripts to PowerShell

**Read `docs/eggs/TEMPLATE.install.ps1` before writing or converting any egg
install script.** Every time — not just the first. It is a working skeleton
distilled from a script that took several rounds of real debugging to get right,
and its header lists the rules that matter. Most of them fail *silently* when
broken, so they are not recoverable by reading the converted script back.

The one that bites hardest: never pipe, capture or redirect a native command
whose output should be visible. The daemon hands the script a pseudo console so
installers stream live; a pipeline stops it dead at PowerShell and a long
download goes silent for its entire duration.

`docs/eggs/starrupture/install.ps1` is the reference implementation — a complete
egg built on the template, worth reading alongside it.

## Build and test

```powershell
.\build.ps1            # both binaries into .\build
.\build.ps1 -Release   # stripped and trimmed
.\build.ps1 -Test      # go test ./...
```

There is no `make` on Windows. `wings.exe` and `winwings-worker.exe` must be
deployed to the same directory; the daemon spawns the worker from alongside
itself and refuses to start without it.

Tests in `internal/` drive real kernel APIs — Job Objects, process creation,
pseudo consoles, ETW — and are the regression net for the whole port. Run
`go test ./internal/...` before changing anything under it.
