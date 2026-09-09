<#
.SYNOPSIS
    Provisions a Windows host to run game servers under win-wings.

.DESCRIPTION
    Installs the runtimes and native dependencies game servers need, applies the
    performance and Defender settings that matter for server workloads, and
    prints the runtime map to paste into config.yml.

    Idempotent: winget skips packages already present, and the system tweaks are
    all set-to-desired-state.

    Every package ID here was checked against the live winget index. Written for
    Windows Server 2025 with Desktop Experience.

.PARAMETER Java
    Java versions to install. Defaults to the four that cover the Minecraft
    ecosystem. 8 is still needed for 1.8-1.16, which remains widely run.

.PARAMETER SkipDefender
    Do not add Defender exclusions.

.PARAMETER WhatIf
    Show what would happen without changing anything.

.EXAMPLE
    .\provision-host.ps1
    .\provision-host.ps1 -Java 17,21 -SkipDefender
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [int[]]$Java = @(8, 11, 17, 21),
    [switch]$SkipDefender,
    [string]$DataRoot = 'C:\ProgramData\WinWings'
)

$ErrorActionPreference = 'Stop'

function Write-Step($msg) { Write-Host "`n=== $msg ===" -ForegroundColor Cyan }
function Write-Note($msg) { Write-Host "  $msg" -ForegroundColor DarkGray }
function Write-Warn($msg) { Write-Host "  ! $msg" -ForegroundColor Yellow }

if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
        ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Run this from an elevated PowerShell session."
}

# --- winget ------------------------------------------------------------------
#
# Windows Server has historically not shipped the App Installer package that
# provides winget. Server 2025 is better about this but it is not guaranteed on
# a minimal image, so bootstrap it rather than failing halfway through.

Write-Step "Checking winget"
if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
    Write-Warn "winget not found; bootstrapping App Installer"
    try {
        Install-PackageProvider -Name NuGet -Force -Scope AllUsers | Out-Null
        Install-Script -Name winget-install -Force -Scope AllUsers -Repository PSGallery
        & winget-install.ps1
    } catch {
        throw @"
Could not bootstrap winget automatically: $_

Install it by hand from https://aka.ms/getwinget (the .msixbundle), or use
Chocolatey instead, which is generally more reliable on Server SKUs.
"@
    }
}
Write-Note "winget $(winget --version)"

$wingetArgs = @(
    '--silent'
    '--accept-package-agreements'
    '--accept-source-agreements'
    '--disable-interactivity'
    '--source', 'winget'
)

function Install-Pkg {
    param([string]$Id, [string]$Why)

    Write-Host ("  {0,-42} {1}" -f $Id, $Why)
    if (-not $PSCmdlet.ShouldProcess($Id, 'winget install')) { return }

    # 0x8A15002B / -1978335189 means "already installed, nothing to do".
    & winget install --id $Id @wingetArgs 2>&1 | Out-Null
    switch ($LASTEXITCODE) {
        0           { }
        -1978335189 { Write-Note "already installed" }
        -1978335216 { Write-Warn "no applicable upgrade found" }
        default     { Write-Warn "winget returned $LASTEXITCODE for $Id" }
    }
}

# --- Visual C++ runtimes -----------------------------------------------------
#
# The single most common cause of a native game server refusing to start. Source
# engine games in particular need the x86 2013 runtime even on a 64-bit host,
# and plenty of Unity and Unreal titles link against several generations.
#
# These are small; install them all rather than diagnosing missing DLLs later.

Write-Step "Visual C++ runtimes"
foreach ($id in @(
    'Microsoft.VCRedist.2015+.x64'
    'Microsoft.VCRedist.2015+.x86'
    'Microsoft.VCRedist.2013.x64'
    'Microsoft.VCRedist.2013.x86'
    'Microsoft.VCRedist.2012.x64'
    'Microsoft.VCRedist.2012.x86'
    'Microsoft.VCRedist.2010.x64'
    'Microsoft.VCRedist.2010.x86'
    'Microsoft.VCRedist.2008.x64'
    'Microsoft.VCRedist.2008.x86'
)) {
    Install-Pkg $id 'native game servers'
}

# --- Java --------------------------------------------------------------------
#
# JREs rather than JDKs: servers only need to run, and the JRE is roughly a third
# of the size. An egg needing javac should ask for a JDK explicitly.
#
# Multiple versions coexisting is the norm, which is why win-wings resolves the
# egg's requested runtime to a directory rather than relying on PATH.

Write-Step "Java runtimes"
$javaRoots = @{}
foreach ($v in $Java) {
    Install-Pkg "EclipseAdoptium.Temurin.$v.JRE" "Minecraft and JVM servers"
}

# --- .NET --------------------------------------------------------------------

Write-Step ".NET runtimes"
Install-Pkg 'Microsoft.DotNet.DesktopRuntime.8' 'modern .NET servers'
Install-Pkg 'Microsoft.DotNet.DesktopRuntime.6' 'older .NET servers'
Write-Note ".NET Framework 4.8 is already part of Server 2025"

# --- Tooling install scripts rely on ----------------------------------------
#
# PowerShell's Expand-Archive only understands zip. Game server downloads are
# routinely tar.gz, 7z or rar, so 7-Zip is not optional in practice.

Write-Step "Tooling"
Install-Pkg '7zip.7zip'            'install scripts extracting non-zip archives'
Install-Pkg 'Git.Git'              'install scripts that clone repositories'
Install-Pkg 'Valve.SteamCMD'       'every Steam-based dedicated server'
Install-Pkg 'OpenJS.NodeJS.LTS'    'Node-based servers and bots'
Install-Pkg 'Python.Python.3.12'   'Python-based servers and tooling'
Install-Pkg 'Microsoft.PowerShell' 'PowerShell 7, preferred for install scripts'

# --- Defender ----------------------------------------------------------------
#
# This is the single biggest performance lever on a game server host. Real-time
# scanning of a world directory being written continuously is brutal, and
# Defender periodically quarantines legitimate anti-cheat and server binaries as
# false positives.
#
# Scoped to the server data tree only; the rest of the host stays protected.

if (-not $SkipDefender) {
    Write-Step "Windows Defender exclusions"
    if (Get-Command Add-MpPreference -ErrorAction SilentlyContinue) {
        foreach ($p in @("$DataRoot\volumes", "$DataRoot\tmp", "$DataRoot\backups")) {
            Write-Host "  path: $p"
            if ($PSCmdlet.ShouldProcess($p, 'Defender path exclusion')) {
                New-Item -ItemType Directory -Force $p | Out-Null
                Add-MpPreference -ExclusionPath $p -ErrorAction SilentlyContinue
            }
        }
        Write-Note "the rest of the host remains protected"
    } else {
        Write-Warn "Defender cmdlets unavailable; skipping"
    }
}

# --- Performance -------------------------------------------------------------

Write-Step "Performance settings"

# Server SKUs default to Balanced, which parks cores and adds latency under the
# bursty load a game server produces.
if ($PSCmdlet.ShouldProcess('power plan', 'set High performance')) {
    powercfg /setactive 8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c 2>&1 | Out-Null
    Write-Note "power plan: High performance"
}

# A Job Object memory limit makes allocations fail rather than triggering an OOM
# kill, so a server sitting near its cap depends on the page file to absorb
# spikes. System-managed is the right default here.
$cs = Get-CimInstance Win32_ComputerSystem
if (-not $cs.AutomaticManagedPagefile) {
    Write-Warn "page file is not system-managed; consider enabling it"
} else {
    Write-Note "page file: system-managed"
}

# --- Report ------------------------------------------------------------------

Write-Step "Runtime map for config.yml"

$runtimes = [ordered]@{}
Get-ChildItem 'C:\Program Files\Eclipse Adoptium' -Directory -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -match '^jre-(\d+)' -or $_.Name -match '^jdk-(\d+)' } |
    ForEach-Object {
        if ($_.Name -match '(\d+)') { $runtimes["java-$($Matches[1])"] = $_.FullName }
    }

if ($runtimes.Count -eq 0) {
    Write-Warn "no Java installations found under C:\Program Files\Eclipse Adoptium"
} else {
    Write-Host ""
    Write-Host "runtime:" -ForegroundColor Green
    Write-Host "  runtimes:" -ForegroundColor Green
    foreach ($k in $runtimes.Keys) {
        Write-Host ("    {0}: '{1}'" -f $k, $runtimes[$k]) -ForegroundColor Green
    }
    Write-Host ""
    Write-Note "an egg's Windows profile sets runtime to one of these names;"
    Write-Note "win-wings then puts its bin directory ahead of PATH for that server."
}

Write-Step "Remaining manual steps"
@"
  1. Create the per-server local accounts and grant each the
     "Log on as a batch job" right.          -> docs\DEPLOYMENT.md step 2

  2. ACL the instances directory and config.yml so server accounts
     cannot write to them.                    -> docs\DEPLOYMENT.md step 3

  3. Install the service.                     -> docs\DEPLOYMENT.md step 5

  Not automated because they need decisions about how many servers this node
  will run and which account the service uses.
"@ | Write-Host

Write-Host "`nDone.`n" -ForegroundColor Green
