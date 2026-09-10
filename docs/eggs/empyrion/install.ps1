# Empyrion: Galactic Survival - win-wings Egg Installation Script
#
# Windows port of the Pterodactyl Empyrion egg, which installs via the generic
# "steamcmd Base Installation Script". Server files land in $env:SERVER_DIR.
#
# Empyrion has no Linux server build. The Linux egg therefore forces the Windows
# depot with `@sSteamCmdForcePlatformType windows` (that is what its
# WINDOWS_INSTALL=1 does) and then runs EmpyrionDedicated.exe under Wine. On
# win-wings the Windows depot is simply the native one and the binary runs
# directly, so both the platform override and Wine are gone. This egg is the case
# where the port removes the most moving parts rather than adding any.
#
##
#
# Variables
# STEAM_USER, STEAM_PASS, STEAM_AUTH - Steam user setup. If a user has 2fa enabled
#                     the login will most likely fail on timeout. Leave blank for
#                     an anonymous install; Empyrion's dedicated server is a free
#                     anonymous download.
# SRCDS_APPID       - Steam app id. Defaults to 530870, Empyrion - Galactic
#                     Survival Dedicated Server.
# SRCDS_BETAID      - beta branch of the app. Leave blank for the default branch.
#                     Empyrion publishes its experimental builds this way.
# SRCDS_BETAPASS    - password for that beta branch, if one is required.
# INSTALL_FLAGS     - additional SteamCMD flags, free text, split on whitespace.
# WINDOWS_INSTALL   - accepted and ignored; this host IS Windows. See above.
# AUTO_UPDATE       - 0/1, read by the daemon before every start, not by this
#                     script.
#
# The egg's many gameplay variables - SERVER_NAME, GAME_SEED, MAX_PLAYERS,
# TELNET_PORT and the rest - are not read here. They are substituted into
# dedicated.yaml by the Panel at boot, and the dedicated server ships that file
# itself, so there is nothing for the install to write.
#
# Supplied by the daemon rather than the egg:
#   SERVER_DIR      the server's files, and this script's working directory
#   STEAMCMD_DIR    this server's steamcmd directory, outside SERVER_DIR
#   TEMP / TMP      scratch space outside the disk quota and backups
#
##


## ---------------------------------------------------------------------------
## Preamble
## ---------------------------------------------------------------------------

# The nearest equivalent of `set -e`. It does NOT cover native executables, whose
# failures show up only in $LASTEXITCODE, so those are checked individually.
$ErrorActionPreference = 'Stop'

# Invoke-WebRequest renders a progress bar by default, which on Windows
# PowerShell 5.1 costs more time than the download itself on a slow link.
$ProgressPreference = 'SilentlyContinue'

# 5.1 defaults to TLS 1.0/1.1, which every host worth downloading from has long
# since refused. Harmless on PowerShell 7, where it is already the default.
try {
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.SecurityProtocolType]::Tls12 -bor [Net.SecurityProtocolType]::Tls13
} catch {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
}

# .NET builds its default web proxy from the WinINET settings under HKCU. An
# account whose profile has never been loaded has no HKCU, and the construction
# fails on the first web request as:
#
#   Error creating the Web Proxy specified in the 'system.net/defaultProxy'
#   configuration section
#
# which mentions neither the registry nor the profile. win-wings loads the
# profile before an install, so this should not arise -- but the check is two
# lines and turns a fatal, badly-labelled failure into a warning where it does.
try {
    $null = [System.Net.WebRequest]::DefaultWebProxy
} catch {
    Write-Host "Default web proxy configuration is unreadable ($($_.Exception.Message))."
    Write-Host 'Continuing with no proxy. If this host needs one, its installs will fail.'
    try { [System.Net.WebRequest]::DefaultWebProxy = $null } catch { }
}

$ServerDir = $env:SERVER_DIR

# The daemon points TEMP at a scratch directory outside the sandbox, so nothing
# written there is charged against the disk quota or copied into backups.
$TempDir = if ($env:TEMP) { $env:TEMP } else { [System.IO.Path]::GetTempPath() }
New-Item -ItemType Directory -Force -Path $TempDir | Out-Null

# SteamCMD must live OUTSIDE the install directory, which is where the Linux
# script put it (/mnt/server/steamcmd, installing into /mnt/server) and where a
# straight port would put it too.
#
# steamcmd.exe treats its own directory as the Steam root and refuses an install
# path that contains it:
#
#   Please set the game install path to something other than the Steam install
#   folder
#
# It does not reliably fail when it says so. It ignores +force_install_dir,
# installs into its own steamapps\common instead, can still report success, and
# exits 7 or even 0 - so the game lands one directory too deep and the server has
# nothing to run.
#
# On Linux none of this arises: steamcmd.sh roots itself at ~/Steam regardless of
# where the script lives, which is why the original layout is fine there. It is a
# genuine platform difference, not a port mistake.
#
# win-wings therefore gives every server a steamcmd directory beside its files
# and hands it over as STEAMCMD_DIR. It is also where the daemon looks before
# every start, so that AUTO_UPDATE works: put steamcmd anywhere else and the
# server never updates.
$SteamCmdDir = $env:STEAMCMD_DIR


## ---------------------------------------------------------------------------
## Helpers
## ---------------------------------------------------------------------------

function Write-Section {
    param([string]$Text)
    Write-Host '-----------------------------------------'
    Write-Host $Text
    Write-Host '-----------------------------------------'
}

# Download a file, falling back to curl.exe.
#
# Invoke-WebRequest is the obvious tool and the fragile one: on Windows
# PowerShell 5.1 it buffers the whole response in memory, and it drags in the
# whole of .NET's proxy and configuration machinery to make one HTTP request.
# curl.exe has shipped in System32 since Windows 10 1803, talks to WinHTTP
# directly, and is unbothered by all of it. Trying it second means the common
# path is unchanged and the awkward hosts still work.
function Get-RemoteFile {
    param([string]$Uri, [string]$OutFile, [int]$TimeoutSec = 300)

    try {
        Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing -TimeoutSec $TimeoutSec
        return
    } catch {
        $reason = $_.Exception.Message
    }

    $curl = Join-Path $env:SystemRoot 'System32\curl.exe'
    if (-not (Test-Path $curl)) { throw $reason }

    Write-Host "  Invoke-WebRequest failed ($reason); retrying with curl.exe"
    # -f fails on an HTTP error rather than saving the error page as the payload,
    # -L follows redirects.
    & $curl -fsSL --max-time $TimeoutSec -o $OutFile $Uri
    if ($LASTEXITCODE -ne 0) {
        throw "curl.exe exited $LASTEXITCODE (Invoke-WebRequest had failed with: $reason)"
    }
}


## ---------------------------------------------------------------------------
## Preflight
##
## The Linux script checked for curl, tar and ca-certificates because the install
## image might not have them; here those are language features. What is worth
## checking instead is that the daemon handed us a usable environment and that
## every egg variable this script needs is set - a missing SRCDS_APPID otherwise
## surfaces as steamcmd doing something bizarre with an empty +app_update.
## ---------------------------------------------------------------------------

if (-not $ServerDir) {
    Write-Host 'SERVER_DIR is not set. This script must be run by win-wings. Aborting.'
    exit 1
}
if (-not (Test-Path $ServerDir)) {
    Write-Host "SERVER_DIR ($ServerDir) does not exist. Aborting."
    exit 1
}
if (-not $SteamCmdDir) {
    Write-Host 'STEAMCMD_DIR is not set. This script must be run by win-wings. Aborting.'
    exit 1
}

# The Panel always supplies this. Defaulting it keeps the script runnable by
# hand, and this egg installs exactly one app either way.
$AppId = $env:SRCDS_APPID
if (-not $AppId) {
    $AppId = '530870'
    Write-Host "SRCDS_APPID is not set; using $AppId (Empyrion Dedicated Server)."
}

# Asserted rather than assumed, because the failure it prevents is silent.
$rootFull = [System.IO.Path]::GetFullPath($ServerDir).TrimEnd('\')
$cmdFull  = [System.IO.Path]::GetFullPath($SteamCmdDir).TrimEnd('\')
if ($cmdFull.StartsWith($rootFull + '\', [StringComparison]::OrdinalIgnoreCase) -or
    $cmdFull.Equals($rootFull, [StringComparison]::OrdinalIgnoreCase)) {
    Write-Host 'SteamCMD would be installed inside the game install path:'
    Write-Host "  install path = $rootFull"
    Write-Host "  steamcmd     = $cmdFull"
    Write-Host 'SteamCMD refuses that and silently installs to the wrong place instead.'
    Write-Host 'Aborting.'
    exit 1
}

## just in case someone removed the defaults.
$SteamUser = $env:STEAM_USER
$SteamPass = $env:STEAM_PASS
$SteamAuth = $env:STEAM_AUTH
if ([string]::IsNullOrEmpty($SteamUser) -or [string]::IsNullOrEmpty($SteamPass)) {
    Write-Host 'Steam user is not set.'
    Write-Host 'Using anonymous user.'
    $SteamUser = 'anonymous'
    $SteamPass = ''
    $SteamAuth = ''
} else {
    Write-Host "user set to $SteamUser"
}


## ---------------------------------------------------------------------------
## Environment diagnostics
##
## These end up in the install log and are the first thing to read when an
## install fails.
## ---------------------------------------------------------------------------

Write-Host '----- environment -----'
Write-Host "  SERVER_DIR   = $ServerDir"
Write-Host "  STEAMCMD_DIR = $SteamCmdDir"
Write-Host "  TEMP         = $TempDir"
Write-Host "  SRCDS_APPID  = $AppId"
Write-Host "  PowerShell   = $($PSVersionTable.PSVersion)"
Write-Host "  Running as   = $([System.Security.Principal.WindowsIdentity]::GetCurrent().Name)"
Write-Host '------------------------'


## ---------------------------------------------------------------------------
## Download and install SteamCMD
##
## The Linux script fetched steamcmd_linux.tar.gz and untarred it. Here it is a
## zip and Expand-Archive - there is no tar and none is needed.
##
## Nothing corresponds to the script's `chown -R root:root /mnt` or its
## `export HOME=/mnt/server`. Permissions are the daemon's job and are already
## correct, and steamcmd.exe roots itself at its own directory rather than at
## $HOME.
## ---------------------------------------------------------------------------

New-Item -ItemType Directory -Force -Path $SteamCmdDir | Out-Null
$SteamCmdExe = Join-Path $SteamCmdDir 'steamcmd.exe'

if (-not (Test-Path $SteamCmdExe)) {
    $archive = Join-Path $TempDir 'steamcmd.zip'
    # The Akamai host is occasionally unreachable while the other is fine.
    $mirrors = @(
        'https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip',
        'https://media.steampowered.com/client/installer/steamcmd.zip'
    )
    $downloaded = $false
    foreach ($url in $mirrors) {
        try {
            Write-Host "Downloading SteamCMD from $url"
            Get-RemoteFile -Uri $url -OutFile $archive -TimeoutSec 120
            $downloaded = $true
            break
        } catch {
            Write-Host "  mirror failed: $($_.Exception.Message)"
        }
    }
    if (-not $downloaded) {
        Write-Host 'Could not download SteamCMD from either mirror. Aborting.'
        exit 1
    }
    Expand-Archive -Path $archive -DestinationPath $SteamCmdDir -Force
    Remove-Item $archive -Force -ErrorAction SilentlyContinue
    if (-not (Test-Path $SteamCmdExe)) {
        Write-Host "steamcmd.exe was not found in the archive at $SteamCmdDir. Aborting."
        exit 1
    }
}

# Fix steamcmd disk write error when this folder is missing - the same reason the
# Linux script created it. SteamCMD writes appmanifest_*.acf here.
New-Item -ItemType Directory -Force -Path (Join-Path $ServerDir 'steamapps') | Out-Null

Set-Location $SteamCmdDir


## ---------------------------------------------------------------------------
## Install the app
##
## Differences from the one-line Linux invocation, all of them deliberate:
##
##  - It runs up to three times. A depot fetch that dies halfway is the single
##    most common install failure and one retry fixes nearly all of them.
##  - `validate` is skipped on the first attempt. On a clean directory it only
##    adds a full rehash pass and makes a failure slower to diagnose; the Linux
##    script passed it every time.
##  - NOTHING is deleted between attempts - not the partial download, and not
##    the appinfo cache. See the note in the loop: clearing the cache is what
##    makes this fail forever rather than succeed on the second try.
##  - content_log.txt is dumped on failure rather than discarded.
##  - @sSteamCmdForcePlatformType is dropped, along with the WINDOWS_INSTALL test
##    that gated it. The Linux egg forced the Windows depot because it was
##    running on Linux; here the native platform is already the right one, and
##    forcing it invites a mismatch with the running SteamCMD.
## ---------------------------------------------------------------------------

# Four, not three, because the first attempt is effectively a warm-up. A
# freshly unpacked SteamCMD has an empty appcache, and its first +app_update
# fails before it downloads anything:
#
#   ERROR! Failed to install app '530870' (Missing configuration)
#
# The app's config simply has not arrived yet - appinfo_log.txt shows the
# request for the app going out and the update job returning "apps updated 0"
# in the same second the install gives up. The second attempt, run against the
# cache the first one left behind, downloads normally. Measured on a clean
# steamcmd directory: attempt 1 fails this way, attempt 2 succeeds.
#
# This is why nothing below deletes appcache\appinfo.vdf between attempts.
# Doing so returns SteamCMD to the empty-cache state every time, so every
# attempt behaves like the first and the install can never get past
# "Missing configuration" - the failure looks permanent when it is really a
# self-healing first run. A genuinely corrupt cache is worth clearing by hand;
# it is not worth clearing on a schedule that guarantees this.
$Retries   = 4
$InstallOk = $false

for ($i = 1; $i -le $Retries; $i++) {
    Write-Host '==================================================='
    Write-Host "SteamCMD install attempt $i of $Retries"
    Write-Host '==================================================='

    $steamArgs = @('+force_install_dir', $ServerDir, '+login', $SteamUser)
    if ($SteamPass) { $steamArgs += $SteamPass }
    if ($SteamAuth) { $steamArgs += $SteamAuth }
    $steamArgs += @('+app_info_update', '1', '+app_update', $AppId)
    if ($env:SRCDS_BETAID)   { $steamArgs += @('-beta', $env:SRCDS_BETAID) }
    if ($env:SRCDS_BETAPASS) { $steamArgs += @('-betapassword', $env:SRCDS_BETAPASS) }
    if ($i -gt 1) { $steamArgs += 'validate' }
    # INSTALL_FLAGS is free text, so it is split the way a shell would.
    if ($env:INSTALL_FLAGS) {
        $steamArgs += ($env:INSTALL_FLAGS -split '\s+' | Where-Object { $_ })
    }
    $steamArgs += '+quit'

    # This log is shown in the Panel and kept on disk, so credentials are masked.
    # The Linux script printed the password in clear.
    $shown = $steamArgs | ForEach-Object {
        if (($SteamPass -and $_ -eq $SteamPass) -or ($SteamAuth -and $_ -eq $SteamAuth)) {
            '<redacted>'
        } elseif ($_ -match '\s') { '"' + $_ + '"' } else { $_ }
    }
    Write-Host "Running command:`n  steamcmd.exe $($shown -join ' ')"

    # Do not add a pipeline here, do not assign the result, and do not wrap this
    # in a function whose return value is captured - any of those gives steamcmd
    # a pipe, its C runtime switches to full buffering, and the download goes
    # silent for its entire duration instead of streaming to the Panel.
    #
    # $ErrorActionPreference is relaxed because PowerShell 7.4 turns a native
    # command's non-zero exit into a terminating error when it is 'Stop', which
    # would kill this retry loop on its first failed attempt.
    $previousEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & $SteamCmdExe @steamArgs
        $steamRc = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousEAP
    }

    # Having refused the install path, steamcmd installs into its own library,
    # which leaves a manifest here that has no business existing. A far sturdier
    # signal than matching its console output.
    $strayManifest = Join-Path $SteamCmdDir "steamapps\appmanifest_$AppId.acf"
    if (Test-Path $strayManifest) {
        Write-Host ''
        Write-Host 'SteamCMD ignored the install path and installed into its own library.'
        Write-Host 'The game is not where the server expects it. Aborting rather than'
        Write-Host 'retrying, because every attempt will do the same.'
        Write-Host "  install path   = $ServerDir"
        Write-Host "  steamcmd       = $SteamCmdDir"
        Write-Host "  stray manifest = $strayManifest"
        exit 1
    }

    # Success is decided from the filesystem, not from the installer's words:
    # steamcmd reports 0 on some real failures and 7 on some successes.
    #
    # StateFlags is a bitfield, not a value: k_EAppStateFullyInstalled is bit 4,
    # and a healthy install routinely carries more than that bit alone - 6 is
    # fully installed with an update available, and 1024 shows up while an update
    # is queued. Matching the literal string "4" rejects installs that succeeded.
    $manifest  = Join-Path $ServerDir "steamapps\appmanifest_$AppId.acf"
    $installed = $false
    $stateNote = 'no appmanifest was written'
    if (Test-Path $manifest) {
        $match = Select-String -Path $manifest -Pattern '"StateFlags"\s*"(\d+)"' |
            Select-Object -First 1
        if (-not $match) {
            $stateNote = 'the appmanifest has no StateFlags'
        } else {
            $state = [int]$match.Matches[0].Groups[1].Value
            $installed = ($state -band 4) -eq 4
            $stateNote = "StateFlags=$state"
        }
    }

    if ($steamRc -eq 0 -and $installed) {
        Write-Host "SteamCMD install succeeded on attempt $i ($stateNote)."
        $InstallOk = $true
        break
    }

    Write-Host "SteamCMD install failed on attempt $i (exit $steamRc, $stateNote)."

    # These live under the steamcmd directory, not the install directory:
    # force_install_dir moves the game, not steamcmd's own working files.
    foreach ($pair in @(
        @{ Path = 'logs\content_log.txt'; Lines = 60 },
        @{ Path = 'logs\stderr.txt';      Lines = 30 }
    )) {
        $logPath = Join-Path $SteamCmdDir $pair.Path
        if (Test-Path $logPath) {
            Write-Host "----- tail of $($pair.Path) -----"
            Get-Content $logPath -Tail $pair.Lines | Write-Host
            Write-Host '---------------------------------------------'
        }
    }

    if ($i -lt $Retries) {
        $backoff = $i * 15
        Write-Host "Retrying in ${backoff}s (partial download data preserved)..."
        Start-Sleep -Seconds $backoff
    }
}

if (-not $InstallOk) {
    Write-Host "SteamCMD install failed after $Retries attempts."
    Write-Host 'Directories preserved for inspection:'
    Write-Host "  $ServerDir\steamapps"
    Write-Host "  $SteamCmdDir\logs"
    Write-Host 'Aborting so this server is not marked as successfully installed.'
    exit 1
}


## ---------------------------------------------------------------------------
## Verify
##
## Prove the server has something to run before reporting success. This is the
## check that stops a broken install being marked good, and it runs before the
## two steps below so that neither can make an empty install look populated.
##
## DedicatedServer\EmpyrionDedicated.exe is what the startup command executes -
## the Linux egg cd's into DedicatedServer and runs it under Wine, and win-wings
## runs the same binary natively.
## ---------------------------------------------------------------------------

$BinariesDir  = Join-Path $ServerDir 'DedicatedServer'
$serverBinary = Join-Path $BinariesDir 'EmpyrionDedicated.exe'
if (-not (Test-Path $serverBinary)) {
    Write-Host "The install finished but $serverBinary is missing."
    Write-Host 'Aborting rather than marking this server installed.'
    exit 1
}


## ---------------------------------------------------------------------------
## Steam client redistributable
##
## The Linux script copied linux32/steamclient.so and linux64/steamclient.so into
## .steam/sdk32 and .steam/sdk64, where the Linux Steamworks redistributable
## looks for them. Windows has no such convention: it resolves steamclient64.dll
## through the Steam registry keys under HKCU, which a server account that has
## never run the Steam client does not have. A copy beside the server binary -
## here DedicatedServer, next to EmpyrionDedicated.exe - is the equivalent.
##
## Missing DLLs are skipped rather than treated as an error: which of these
## SteamCMD ships varies with its version, and Empyrion carries its own
## steam_api64.dll regardless.
## ---------------------------------------------------------------------------

$copied = @()
foreach ($dll in @('steamclient64.dll', 'steamclient.dll',
                   'tier0_s64.dll',     'tier0_s.dll',
                   'vstdlib_s64.dll',   'vstdlib_s.dll')) {
    $src = Join-Path $SteamCmdDir $dll
    if (Test-Path $src) {
        Copy-Item $src (Join-Path $BinariesDir $dll) -Force
        $copied += $dll
    }
}
if ($copied.Count -gt 0) {
    Write-Host "Copied to ${BinariesDir}: $($copied -join ', ')"
} else {
    Write-Host "No Steam client DLLs found in $SteamCmdDir; none were copied."
}


## ---------------------------------------------------------------------------
## Log directory
##
## The startup command passes `-logFile ..\Logs\server\server.log`. Unity opens
## that path but does not create the directories leading to it: if they are
## missing the server runs with no log at all, and the Panel console - which is
## fed by tailing that file - stays empty for a server that is working fine.
## The game creates Logs\ on its own eventually, but not before the first write.
## ---------------------------------------------------------------------------

New-Item -ItemType Directory -Force -Path (Join-Path $ServerDir 'Logs\server') | Out-Null


Write-Section 'Installation completed...'
exit 0
