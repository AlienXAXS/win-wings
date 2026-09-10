# StarRupture - win-wings Egg Installation Script
#
# Windows port of the Pterodactyl Linux egg. Server files land in $env:SERVER_DIR.
#
##
#
# Variables
# STEAM_USER, STEAM_PASS, STEAM_AUTH - Steam user setup. Leave blank for anon install.
# WINDOWS_INSTALL   - accepted and ignored; this host IS Windows
# SRCDS_APPID       - Steam app id (StarRupture Dedicated Server = 3809400)
# SRCDS_BETAID      - beta branch. Leave blank for the default branch
# SRCDS_BETAPASS    - beta branch password, if required
# INSTALL_FLAGS     - additional SteamCMD flags
# STEAM_NO_HTTP2    - 1 to disable HTTP/2 downloads (workaround for stalled/0-byte
#                     depot fetches on some networks). Default 0.
# STEAM_RATE_KBPS   - optional download throttle in Kbps. Blank = unlimited.
# AUTO_UPDATE       - 0/1, auto update on boot
# ADMIN_PASSWORD    - admin password if any
# PLAYER_PASSWORD   - server password if any
#
##

# The nearest equivalent of `set -e`. It does NOT cover native executables, whose
# failures show up only in $LASTEXITCODE, so those are checked individually.
$ErrorActionPreference = 'Stop'

# Invoke-WebRequest renders a progress bar by default, which on Windows
# PowerShell 5.1 costs more time than the download itself on a slow link.
$ProgressPreference = 'SilentlyContinue'

# 5.1 defaults to TLS 1.0/1.1, which every host used here has long since
# refused. Harmless on PowerShell 7, where it is already the default.
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
# which mentions neither the registry nor the profile, and looks for all the
# world like a misconfigured proxy. win-wings loads the profile before launching
# an install, so this should not arise -- but the check is two lines and turns a
# fatal, badly-labelled failure into a warning on any host where it still does.
#
# Only cleared when reading it actually throws, so a host with a real proxy keeps
# using it.
try {
    $null = [System.Net.WebRequest]::DefaultWebProxy
} catch {
    Write-Host "Default web proxy configuration is unreadable ($($_.Exception.Message))."
    Write-Host 'Continuing with no proxy. If this host needs one, its installs will fail.'
    try { [System.Net.WebRequest]::DefaultWebProxy = $null } catch { }
}

$SteamRoot = $env:SERVER_DIR

# The daemon points TEMP at a scratch directory outside the sandbox, so it is
# neither charged against the user's disk quota nor copied into backups. It lives
# for as long as the server does and is removed with it.
$TempDir = if ($env:TEMP) { $env:TEMP } else { [System.IO.Path]::GetTempPath() }
New-Item -ItemType Directory -Force -Path $TempDir | Out-Null

# SteamCMD must live OUTSIDE the install directory, which is where the Linux egg
# put it and where a straight port would put it too.
#
# steamcmd.exe treats its own directory as the Steam root. Asking it to install
# into an ancestor of that directory makes the install path contain the Steam
# folder, which it refuses:
#
#   Please set the game install path to something other than the Steam install
#   folder
#
# It does not reliably fail when it says this. It ignores +force_install_dir,
# installs into its own steamapps\common instead, reports "Success! App ... fully
# installed", and exits 7 or even 0 - so the game lands one directory too deep,
# no appmanifest appears where anything looks for it, and the server has nothing
# to run. That is what makes this worth a paragraph rather than a line.
#
# On Linux none of this arises: steamcmd.sh roots itself at ~/Steam regardless of
# where the script lives, so /mnt/server/steamcmd with an install path of
# /mnt/server is fine. This is a genuine platform difference, not a port mistake.
#
# win-wings therefore gives every server a steamcmd directory of its own, beside
# its files rather than inside them, and hands it to this script as
# STEAMCMD_DIR. It is outside the install path, on the same volume so committing
# a download is a move rather than a copy, writable by this account, outside the
# disk quota and backups, and removed along with the server.
#
# It is also where the daemon looks before every start: with AUTO_UPDATE set it
# runs steamcmd from there to update the server, so leaving it anywhere else
# means the server never updates.
$SteamCmdDir = $env:STEAMCMD_DIR

function Write-Section {
    param([string]$Text)
    Write-Host '-----------------------------------------'
    Write-Host $Text
    Write-Host '-----------------------------------------'
}

# Write a file as UTF-8 with no BOM and LF endings.
#
# Set-Content -Encoding utf8 emits a BOM on Windows PowerShell 5.1, and the
# default line ending is CRLF. Both break consumers that parse these files
# byte-for-byte, .pteroignore included.
function Write-TextFile {
    param([string]$Path, [string]$Content)
    $dir = Split-Path -Parent $Path
    if ($dir) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    [System.IO.File]::WriteAllText($Path, ($Content -replace "`r`n", "`n"),
        (New-Object System.Text.UTF8Encoding($false)))
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
    # -L follows the redirects every GitHub release download starts with.
    & $curl -fsSL --max-time $TimeoutSec -o $OutFile $Uri
    if ($LASTEXITCODE -ne 0) {
        throw "curl.exe exited $LASTEXITCODE (Invoke-WebRequest had failed with: $reason)"
    }
}

## ---------------------------------------------------------------------------
## Preflight
##
## The Linux script checked for curl, jq, unzip and tar because its install
## image might not have them. Here they are language features: Invoke-WebRequest,
## ConvertFrom-Json and Expand-Archive ship with PowerShell. What is worth
## checking instead is that the daemon handed us a usable environment.
## ---------------------------------------------------------------------------

if (-not $SteamRoot) {
    Write-Host 'SERVER_DIR is not set. This script must be run by win-wings. Aborting.'
    exit 1
}
if (-not (Test-Path $SteamRoot)) {
    Write-Host "SERVER_DIR ($SteamRoot) does not exist. Aborting."
    exit 1
}
if (-not $env:SRCDS_APPID) {
    Write-Host 'SRCDS_APPID is not set. Aborting.'
    exit 1
}
if (-not $SteamCmdDir) {
    Write-Host 'STEAMCMD_DIR is not set. This script must be run by win-wings. Aborting.'
    exit 1
}

# Asserted rather than assumed, because the failure it prevents is silent: an
# install that reports success and puts the game somewhere nothing looks.
$rootFull = [System.IO.Path]::GetFullPath($SteamRoot).TrimEnd('\')
$cmdFull  = [System.IO.Path]::GetFullPath($SteamCmdDir).TrimEnd('\')
if ($cmdFull.StartsWith($rootFull + '\', [StringComparison]::OrdinalIgnoreCase) -or
    $cmdFull.Equals($rootFull, [StringComparison]::OrdinalIgnoreCase)) {
    Write-Host "SteamCMD would be installed inside the game install path:"
    Write-Host "  install path = $rootFull"
    Write-Host "  steamcmd     = $cmdFull"
    Write-Host 'SteamCMD refuses that and silently installs to the wrong place instead.'
    Write-Host 'This means STEAMCMD_DIR is not pointing outside SERVER_DIR. Aborting.'
    exit 1
}

$SteamUser = $env:STEAM_USER
$SteamPass = $env:STEAM_PASS
$SteamAuth = $env:STEAM_AUTH

if ([string]::IsNullOrEmpty($SteamUser) -or [string]::IsNullOrEmpty($SteamPass)) {
    Write-Host 'Steam user is not set. Using anonymous user.'
    $SteamUser = 'anonymous'
    $SteamPass = ''
    $SteamAuth = ''
} else {
    Write-Host "Steam user set to $SteamUser"
}

## ---------------------------------------------------------------------------
## Install SteamCMD
##
## The Windows build is a zip rather than a tarball, and the binary is
## steamcmd.exe rather than steamcmd.sh. Same two mirrors: the Akamai host is
## occasionally unreachable while media.steampowered.com is fine.
## ---------------------------------------------------------------------------

New-Item -ItemType Directory -Force -Path $SteamCmdDir | Out-Null
$SteamCmdExe = Join-Path $SteamCmdDir 'steamcmd.exe'

if (-not (Test-Path $SteamCmdExe)) {
    $archive = Join-Path $TempDir 'steamcmd.zip'
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

# SteamCMD writes appmanifest_*.acf here and errors out if the directory is absent.
New-Item -ItemType Directory -Force -Path (Join-Path $SteamRoot 'steamapps') | Out-Null

# The Linux script ran `chown -R root:root /mnt` here to work around SteamCMD
# refusing to write. There is no equivalent, and none is needed: win-wings owns
# the permissions on this tree and has already granted this account exclusive
# write access to it.

Set-Location $SteamCmdDir

## ---------------------------------------------------------------------------
## Environment diagnostics - these end up in the install log and are the first
## thing to read when an install fails.
##
## The Linux egg also reported inodes, free space and MTU. Inodes have no NTFS
## equivalent. The other two are gone because they did not earn their place: the
## free space figure was wrong, and Get-NetIPInterface can sit for minutes on a
## host with virtual adapters before returning nothing useful - which on a slow
## install is time spent staring at a console that appears to have hung.
## ---------------------------------------------------------------------------

Write-Host '----- environment -----'
Write-Host "  SERVER_DIR      = $SteamRoot"
Write-Host "  TEMP            = $TempDir"
Write-Host "  STEAMCMD_DIR    = $SteamCmdDir"
Write-Host "  PowerShell      = $($PSVersionTable.PSVersion)"
Write-Host "  Running as      = $([System.Security.Principal.WindowsIdentity]::GetCurrent().Name)"
Write-Host '------------------------'

## ---------------------------------------------------------------------------
## SteamCMD install with retry
##
## Notes carried over from the Linux egg, all of which still apply:
##  - `validate` is skipped on the first attempt. On a clean directory it only
##    adds a full rehash pass, and it makes a failing install slower to diagnose.
##  - Partial downloads are NOT deleted between attempts. Wiping
##    steamapps/downloading turns a transient CDN hiccup into three full-size
##    redownloads. Only the appinfo cache is cleared, which is the piece that
##    actually goes stale.
##  - content_log.txt is dumped on failure rather than discarded.
##
## Two changes specific to this platform:
##  - @sSteamCmdForcePlatformType is gone. The Linux egg forced the Windows depot
##    because StarRupture ships no Linux server build; here the native platform
##    is already the right one, and forcing it invites a mismatch with the
##    running SteamCMD.
##  - The HTTP/2 toggle is the Windows convar, not the Linux one.
## ---------------------------------------------------------------------------

$Retries   = 3
$InstallOk = $false
$AppId     = $env:SRCDS_APPID

for ($i = 1; $i -le $Retries; $i++) {
    Write-Host '==================================================='
    Write-Host "SteamCMD install attempt $i of $Retries"
    Write-Host '==================================================='

    # A corrupt appinfo cache produces "state is 0x202" with 0/0 progress.
    # app_info_update refreshes it; it does not repair it. Delete it instead.
    if ($i -gt 1) {
        Remove-Item (Join-Path $SteamCmdDir 'appcache\appinfo.vdf') `
            -Force -ErrorAction SilentlyContinue
    }

    Write-Host "Priming Steam app info cache for $AppId..."
    $prime = @('+login', $SteamUser)
    if ($SteamPass) { $prime += $SteamPass }
    if ($SteamAuth) { $prime += $SteamAuth }
    $prime += @('+app_info_update', '1', '+app_info_print', $AppId, '+quit')
    & $SteamCmdExe @prime *> $null

    $steamArgs = @()

    # Transport tuning, before login.
    if ($env:STEAM_NO_HTTP2 -eq '1') {
        $steamArgs += @('+@nClientDownloadEnableHTTP2PlatformWindows', '0')
    }
    if ($env:STEAM_RATE_KBPS) {
        $steamArgs += @('+@nCSClientRateLimitKbps', $env:STEAM_RATE_KBPS)
    }

    $steamArgs += @('+force_install_dir', $SteamRoot, '+login', $SteamUser)
    if ($SteamPass) { $steamArgs += $SteamPass }
    if ($SteamAuth) { $steamArgs += $SteamAuth }

    $steamArgs += @('+app_info_update', '1', '+app_update', $AppId)

    if ($env:SRCDS_BETAID)   { $steamArgs += @('-beta', $env:SRCDS_BETAID) }
    if ($env:SRCDS_BETAPASS) { $steamArgs += @('-betapassword', $env:SRCDS_BETAPASS) }

    # Validate from the second attempt onward.
    if ($i -gt 1) { $steamArgs += 'validate' }

    # INSTALL_FLAGS is a free-text field, so it is split the way a shell would.
    if ($env:INSTALL_FLAGS) {
        $steamArgs += ($env:INSTALL_FLAGS -split '\s+' | Where-Object { $_ })
    }
    $steamArgs += '+quit'

    # The Linux script printed the command with the Steam password in it. This
    # log is shown in the Panel and kept on disk, so the credentials are masked.
    $shown = $steamArgs | ForEach-Object {
        if (($SteamPass -and $_ -eq $SteamPass) -or ($SteamAuth -and $_ -eq $SteamAuth)) {
            '<redacted>'
        } elseif ($_ -match '\s') { '"' + $_ + '"' } else { $_ }
    }
    Write-Host "Running command:`n  steamcmd.exe $($shown -join ' ')"

    # Deliberately NOT piped, captured, or redirected.
    #
    # `& $exe | ForEach-Object { ... }` and `2>&1 |` both hand the child a pipe
    # for its stdout, and two things then conspire. The C runtime switches from
    # line buffering to full buffering the moment stdout is not a character
    # device; and PowerShell splits a native command's output on newlines, while
    # steamcmd reports download progress with bare carriage returns, so it never
    # sees a line to emit until the whole update finishes. A twenty-minute
    # download shows nothing at all and then dumps every progress line at once --
    # which is exactly what it looks like when output is broken.
    #
    # No amount of work on the daemon's side fixes that: win-wings gives this
    # script a pseudo console, and a pipeline stops it dead at PowerShell so
    # steamcmd never sees one. Left alone, steamcmd inherits the console it was
    # given and its progress arrives as it happens.
    #
    # ErrorActionPreference is relaxed across this one call because PowerShell
    # 7.4 turns a native command's non-zero exit into a terminating error when it
    # is Stop -- which would abort on the first failed attempt, the one thing the
    # retry loop exists to survive.
    $previousEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & $SteamCmdExe @steamArgs
        $steamRc = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousEAP
    }

    # Detected from the filesystem rather than from steamcmd's console output.
    #
    # This used to match the message "install path to something other than the
    # Steam install folder", which meant capturing the output and paying for it
    # in the buffering above. The state it leaves behind is the better signal in
    # any case: having refused the install path, steamcmd ignores
    # +force_install_dir and installs into its own library, which puts an
    # appmanifest here that has no business existing.
    $strayManifest = Join-Path $SteamCmdDir "steamapps\appmanifest_$AppId.acf"
    if (Test-Path $strayManifest) {
        Write-Host ''
        Write-Host 'SteamCMD ignored the install path and installed into its own library'
        Write-Host 'instead, which is what it does when the install path contains the'
        Write-Host 'SteamCMD directory. The game is not where the server expects it.'
        Write-Host 'Aborting rather than retrying, because every attempt will do the same.'
        Write-Host "  install path   = $SteamRoot"
        Write-Host "  steamcmd       = $SteamCmdDir"
        Write-Host "  stray manifest = $strayManifest"
        exit 1
    }

    # steamcmd's exit code is not always trustworthy - confirm via the manifest.
    #
    # StateFlags is a bitfield, not a value. k_EAppStateFullyInstalled is bit 4,
    # and a healthy install routinely carries more than that bit alone: 6 is
    # fully installed with an update available, and the 1024 bit shows up while
    # an update is queued. Matching the literal string "4" therefore rejects
    # installs that succeeded, and did - steamcmd printed "Success! App ... fully
    # installed" and this called it a failure and started over.
    $manifest  = Join-Path $SteamRoot "steamapps\appmanifest_$AppId.acf"
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

    # These live under the steamcmd directory rather than the install directory:
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
    Write-Host '==================================================='
    Write-Host "SteamCMD install failed after $Retries attempts."
    Write-Host 'Directories preserved for inspection:'
    Write-Host "  $SteamRoot\steamapps"
    Write-Host "  $SteamCmdDir\logs"
    Write-Host 'Aborting so this server is not marked as successfully installed.'
    Write-Host '==================================================='
    exit 1
}

## ---------------------------------------------------------------------------
## Steam client library
##
## The Linux script copied steamclient.so into .steam/sdk32 and .steam/sdk64,
## which is where the Linux Steamworks redistributable looks. Windows has no such
## convention: it resolves steamclient64.dll through the Steam registry keys under
## HKCU, which this account does not have because it has never run the Steam
## client. Placing a copy next to the server binary is the equivalent, and is
## what a Windows dedicated server without an installed Steam client needs.
## ---------------------------------------------------------------------------

$binariesDir = Join-Path $SteamRoot 'StarRupture\Binaries\Win64'
New-Item -ItemType Directory -Force -Path $binariesDir | Out-Null

foreach ($dll in @('steamclient64.dll', 'steamclient.dll', 'tier0_s64.dll', 'vstdlib_s64.dll')) {
    $src = Join-Path $SteamCmdDir $dll
    if (Test-Path $src) {
        Copy-Item $src (Join-Path $binariesDir $dll) -Force
        Write-Host "Copied $dll to StarRupture\Binaries\Win64"
    }
}

## ---------------------------------------------------------------------------
## .pteroignore
##
## Patterns must start at column 0 - the original heredoc was indented, which
## wrote four leading spaces onto every line, stopped the patterns matching, and
## put the whole install into every backup. Written here with an explicit
## BOM-less UTF-8 writer for the same reason.
## ---------------------------------------------------------------------------

$pteroignore = Join-Path $SteamRoot '.pteroignore'
if (-not (Test-Path $pteroignore)) {
    Write-Host 'Creating default .pteroignore'
    Write-TextFile -Path $pteroignore -Content @'
*
!Password.json
!PlayerPassword.json
!DSSettings.txt
!StarRupture/Saved/SaveGames/*/AutoSave0.met
!StarRupture/Saved/SaveGames/*/AutoSave0.sav
!StarRupture/Saved/SaveGames/SaveData.dat
'@
}

## ---------------------------------------------------------------------------
## GitHub release helper
##
## jq becomes ConvertFrom-Json, which Invoke-RestMethod does implicitly. Failure
## returns $null rather than throwing, so that a GitHub rate limit skips one
## optional component with a warning instead of failing the whole install - the
## same behaviour the Linux script's `|| return 1` produced.
## ---------------------------------------------------------------------------

function Get-LatestReleaseAsset {
    param(
        [string]$Repo,
        [scriptblock]$Filter
    )
    $headers = @{
        'Accept'     = 'application/vnd.github+json'
        'User-Agent' = 'win-wings-egg'   # GitHub rejects requests without one
    }
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        try {
            $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
                -Headers $headers -UseBasicParsing -TimeoutSec 60
            return ($release.assets | Where-Object $Filter | Select-Object -First 1)
        } catch {
            Write-Host "  GitHub API attempt ${attempt} failed: $($_.Exception.Message)"
            if ($attempt -lt 3) { Start-Sleep -Seconds 2 }
        }
    }
    return $null
}

function Save-ReleaseAsset {
    param([object]$Asset, [string]$Destination)
    Write-Host "Downloading $($Asset.name)..."
    Get-RemoteFile -Uri $Asset.browser_download_url -OutFile $Destination -TimeoutSec 300
}

## ---------------------------------------------------------------------------
## StarRupture ModLoader (server)
## ---------------------------------------------------------------------------

Write-Section 'Fetching latest StarRupture ModLoader Server release from GitHub...'

$modLoaderAsset = Get-LatestReleaseAsset -Repo 'AlienXAXS/StarRupture-ModLoader' `
    -Filter { $_.name -like 'StarRupture-ModLoader-Server*' }

if (-not $modLoaderAsset) {
    Write-Host 'Warning: no StarRupture-ModLoader-Server asset found in the latest release. Skipping ModLoader install.'
} else {
    $modLoaderZip = Join-Path $TempDir $modLoaderAsset.name
    Save-ReleaseAsset -Asset $modLoaderAsset -Destination $modLoaderZip

    Write-Host "Extracting $($modLoaderAsset.name) to $binariesDir..."
    Expand-Archive -Path $modLoaderZip -DestinationPath $binariesDir -Force
    Write-Host 'ModLoader installed successfully.'

    $pluginConfigDir = Join-Path $binariesDir 'Plugins\config'
    Write-TextFile -Path (Join-Path $pluginConfigDir 'ServerUtility.ini') -Content @'
[General]
Enabled=1
[PluginSettings]
MaxPlayers=0
RemoteVulnerabilityPatch=1
'@
    Write-Host 'ServerUtility.ini created.'

    $serverUtilityAsset = Get-LatestReleaseAsset -Repo 'AlienXAXS/StarRupture-Plugin-ServerUtility' `
        -Filter { $_.name -like '*.zip' }

    if (-not $serverUtilityAsset) {
        Write-Host 'Warning: no ServerUtility release asset found. Skipping ServerUtility install.'
    } else {
        $serverUtilityZip = Join-Path $TempDir $serverUtilityAsset.name
        Save-ReleaseAsset -Asset $serverUtilityAsset -Destination $serverUtilityZip
        Expand-Archive -Path $serverUtilityZip -DestinationPath $binariesDir -Force
        Write-Host 'ServerUtility installed successfully.'
        Remove-Item $serverUtilityZip -Force -ErrorAction SilentlyContinue
    }

    Remove-Item $modLoaderZip -Force -ErrorAction SilentlyContinue
}

## ---------------------------------------------------------------------------
## rcon-cli
##
## The Windows asset is a zip containing rcon.exe, not a tarball containing an
## executable bit. No chmod equivalent is needed or possible.
## ---------------------------------------------------------------------------

Write-Section 'Installing rcon-cli...'

$rconAsset = Get-LatestReleaseAsset -Repo 'gorcon/rcon-cli' `
    -Filter { $_.name -match 'amd64_win(dows)?\.zip$' }

if (-not $rconAsset) {
    Write-Host 'Warning: no rcon-cli release asset found. Skipping rcon-cli install.'
} else {
    $rconZip     = Join-Path $TempDir $rconAsset.name
    $rconExtract = Join-Path $TempDir 'rcon-cli'
    Save-ReleaseAsset -Asset $rconAsset -Destination $rconZip

    Remove-Item $rconExtract -Recurse -Force -ErrorAction SilentlyContinue
    Expand-Archive -Path $rconZip -DestinationPath $rconExtract -Force

    # The archive nests the binary inside a versioned directory.
    $rconBin = Get-ChildItem -Path $rconExtract -Filter 'rcon.exe' -Recurse -File |
        Select-Object -First 1
    if ($rconBin) {
        Copy-Item $rconBin.FullName (Join-Path $SteamRoot 'rcon.exe') -Force
        Write-Host "rcon-cli installed to $SteamRoot\rcon.exe"
        Write-Host 'Usage: .\rcon.exe -a 127.0.0.1:27015 -p yourpassword "command"'
    } else {
        Write-Host 'Warning: rcon.exe not found in the archive.'
    }

    Remove-Item $rconZip -Force -ErrorAction SilentlyContinue
    Remove-Item $rconExtract -Recurse -Force -ErrorAction SilentlyContinue
}

## ---------------------------------------------------------------------------
## Save directory
## ---------------------------------------------------------------------------

New-Item -ItemType Directory -Force -Path (Join-Path $SteamRoot 'StarRupture\Saved\SaveGames') | Out-Null

Write-Section 'Installation completed...'
exit 0
