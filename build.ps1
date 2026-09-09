<#
.SYNOPSIS
    Builds win-wings.

.DESCRIPTION
    Produces both binaries into .\build. They must be deployed together: the
    daemon spawns the worker from alongside itself and refuses to start without
    it.

.PARAMETER Release
    Strip debug information and trim paths.

.PARAMETER Test
    Run the test suite instead of building.

.EXAMPLE
    .\build.ps1
    .\build.ps1 -Release
    .\build.ps1 -Test
#>
[CmdletBinding()]
param(
    [switch]$Release,
    [switch]$Test
)

$ErrorActionPreference = 'Stop'

# Go is often installed after the shell started, so fall back to the standard
# location when it is not on PATH.
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $candidate = Join-Path $env:ProgramFiles 'Go\bin'
    if (Test-Path (Join-Path $candidate 'go.exe')) {
        $env:PATH = "$candidate;$env:PATH"
    } else {
        throw "Go was not found on PATH or at $candidate. Install Go 1.25 or newer."
    }
}

$version = 'dev'
if (Get-Command git -ErrorAction SilentlyContinue) {
    try { $version = (git rev-parse --short=8 HEAD).Trim() } catch { }
}

if ($Test) {
    Write-Host "Running tests..." -ForegroundColor Cyan
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "tests failed" }
    Write-Host "All tests passed." -ForegroundColor Green
    return
}

New-Item -ItemType Directory -Force build | Out-Null

$ldflags = "-X github.com/pterodactyl/wings/system.Version=$version"
$extra = @()
if ($Release) {
    $ldflags = "-s -w $ldflags"
    $extra = @('-trimpath')
}

Write-Host "Building win-wings ($version)..." -ForegroundColor Cyan

& go build @extra -ldflags="$ldflags" -o build\wings.exe wings.go
if ($LASTEXITCODE -ne 0) { throw "failed to build wings.exe" }

& go build @extra -ldflags="$ldflags" -o build\winwings-worker.exe .\cmd\winwings-worker
if ($LASTEXITCODE -ne 0) { throw "failed to build winwings-worker.exe" }

Get-ChildItem build\*.exe | ForEach-Object {
    "{0,-24} {1,8:N1} MB" -f $_.Name, ($_.Length / 1MB)
}

Write-Host ""
Write-Host "Deploy both binaries to the same directory." -ForegroundColor Yellow
Write-Host "See docs\DEPLOYMENT.md for host setup." -ForegroundColor Yellow
