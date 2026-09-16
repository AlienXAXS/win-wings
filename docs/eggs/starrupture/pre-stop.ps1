# StarRupture - win-wings pre-stop script
#
# Windows port of the Linux egg's shutdown trap. Runs to completion when the
# server is asked to stop, before the profile's stop method, in $env:SERVER_DIR,
# as the server's own account. See "Pre-stop scripts" in docs\PANEL-API.md.
#
# The server does not read stdin and does not handle a console interrupt: the
# only way to make it save and exit cleanly is the RCON "exit" command. The
# install script put rcon-cli beside the server files as rcon.exe for exactly
# this. If the server is still up when this script returns, the daemon carries
# on with the profile's stop method and then kills it, so the wait here is what
# turns a clean exit into a stop the daemon recognises as one.
#
##
#
# Variables
# RCON_PORT     - RCON port the server was started with. Required.
# RCON_PASSWORD - RCON password the server was started with. Required.
#
# Supplied by the daemon rather than the egg:
#   SERVER_DIR  the server's files, and this script's working directory
#   SERVER_PID  the process id of the server being stopped
#
##

$ErrorActionPreference = 'Stop'

$ServerDir = $env:SERVER_DIR
$RconExe   = Join-Path $ServerDir 'rcon.exe'

# How long the server is given to exit after being told to. The Linux trap
# used 15 seconds; the daemon's own stop timeout is 30, and this has to fit
# inside it or the daemon kills this script before it has reported anything.
$GraceSeconds = 15

if (-not $env:SERVER_PID) {
    Write-Host 'SERVER_PID is not set. This script must be run by win-wings as a pre-stop script.'
    exit 1
}
$serverPid = [int]$env:SERVER_PID

if ([string]::IsNullOrEmpty($env:RCON_PORT) -or [string]::IsNullOrEmpty($env:RCON_PASSWORD)) {
    Write-Host 'RCON_PORT or RCON_PASSWORD is not set; the server cannot be asked to exit over RCON.'
    Write-Host 'The daemon will fall back to its own stop method.'
    exit 1
}
if (-not (Test-Path $RconExe)) {
    Write-Host "rcon.exe not found at $RconExe; the server cannot be asked to exit over RCON."
    Write-Host 'Reinstall the server to restore it. The daemon will fall back to its own stop method.'
    exit 1
}

Write-Host "Shutdown requested, sending RCON exit command to 127.0.0.1:$($env:RCON_PORT)..."

# Called as a statement so whatever rcon.exe says is visible. Its failure is
# only in $LASTEXITCODE: a wrong password or a server that has not opened the
# port yet both land here.
$previousEAP = $ErrorActionPreference
$ErrorActionPreference = 'Continue'
try {
    & $RconExe -a "127.0.0.1:$($env:RCON_PORT)" -p $env:RCON_PASSWORD 'exit'
    $rconRc = $LASTEXITCODE
} finally {
    $ErrorActionPreference = $previousEAP
}
if ($rconRc -ne 0) {
    Write-Host "rcon.exe exited $rconRc; the exit command may not have been delivered."
}

# Wait for the server to exit. Get-Process throws for a pid that is gone,
# which is the outcome being waited for.
$waited = 0
while ($waited -lt $GraceSeconds) {
    $running = $null
    try { $running = Get-Process -Id $serverPid -ErrorAction Stop } catch { }
    if (-not $running) { break }
    Start-Sleep -Seconds 1
    $waited++
    Write-Host "Waiting for server to exit... ${waited}s"
}

$running = $null
try { $running = Get-Process -Id $serverPid -ErrorAction Stop } catch { }
if ($running) {
    Write-Host "Server did not exit after ${GraceSeconds}s; leaving it to the daemon's stop method."
    exit 1
}

Write-Host 'Server gracefully shut down.'
exit 0
