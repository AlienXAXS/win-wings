# TEMPLATE — win-wings egg install script
#
# Copy this file, fill in the marked sections, delete what the egg does not
# need. It is a working skeleton, not pseudocode: as it stands it parses, runs,
# and exits 0 having done nothing.
#
# It is distilled from docs\eggs\starrupture\install.ps1, which is the reference
# implementation and worth reading alongside this. Every rule below is here
# because getting it wrong cost real debugging time, and most of them fail
# silently rather than loudly.
#
#
# THE RULES THAT MATTER
#
#  1. NEVER pipe, capture or redirect a native command whose output should be
#     visible.  `& $exe @args | ForEach-Object { ... }`, `2>&1 |`, `$out = & $exe`
#     and `*> file` all hand the child a pipe. Its C runtime then switches from
#     line to full buffering, and PowerShell splits native output on newlines
#     while installers report progress with bare carriage returns — so a long
#     download shows nothing at all and then dumps everything at the end. The
#     daemon gives this script a pseudo console precisely so that does not
#     happen; a pipeline stops it dead at PowerShell.
#     The trap survives a function boundary: `$rc = Invoke-Thing` captures the
#     child's stdout even when the native call is two frames down. Call native
#     commands as statements and read $LASTEXITCODE afterwards.
#
#  2. Relax $ErrorActionPreference around native calls. PowerShell 7.4 turns a
#     native command's non-zero exit into a terminating error when it is 'Stop',
#     which kills any retry loop on its first failed attempt.
#
#  3. Decide success from the filesystem, not from the installer's words.
#     Exit codes lie (steamcmd reports 0 on some real failures and 7 on some
#     successes) and output strings move between versions. Check that the file
#     the server needs is where the server will look for it.
#
#  4. $ErrorActionPreference = 'Stop' does not cover native executables. Their
#     failures appear only in $LASTEXITCODE, so check each one.
#
#  5. There is no apt-get, curl, wget, tar, unzip, jq, chmod or chown. Use
#     Invoke-WebRequest, Expand-Archive, ConvertFrom-Json. Permissions are the
#     daemon's job and are already correct.
#
#  6. Write files as UTF-8 without a BOM and with LF endings. Set-Content
#     -Encoding utf8 emits a BOM on Windows PowerShell 5.1 and defaults to CRLF,
#     which breaks anything parsing the file byte for byte — .pteroignore
#     included.
#
#  7. The script may run on Windows PowerShell 5.1. Do not assume PowerShell 7:
#     no ternaries, no ?., no -Parallel. The daemon prefers pwsh.exe when it is
#     installed and falls back to 5.1 when it is not.
#
#  8. Exit non-zero on failure. A zero exit marks the server installed, and a
#     server marked installed with nothing in it is worse than a failed install.
#
# See "How install scripts differ" in docs\PANEL-API.md for the contract this
# script runs under.


## ---------------------------------------------------------------------------
## Variables
##
## TEMPLATE: document every egg variable the script reads, and say which are
## required. This block is the first thing anyone porting the egg will read.
## ---------------------------------------------------------------------------
#
# SRCDS_APPID   - Steam app id. Required for a Steam game.
# INSTALL_FLAGS - extra installer flags, free text, split on whitespace.
#
# Supplied by the daemon rather than the egg:
#   SERVER_DIR      the server's files, and this script's working directory
#   STEAMCMD_DIR    this server's steamcmd directory, outside SERVER_DIR
#   INSTALL_RUNTIME the runtime the egg asked for, if any
#   TEMP / TMP      scratch space outside the disk quota and backups


## ---------------------------------------------------------------------------
## Preamble
##
## TEMPLATE: keep all of this. None of it is specific to any one egg.
## ---------------------------------------------------------------------------

# The nearest equivalent of `set -e`. See rule 4: it does NOT cover native
# executables.
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
# profile before an install, so this should not arise — but the check is two
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


## ---------------------------------------------------------------------------
## Helpers
##
## TEMPLATE: keep the ones the script uses, delete the rest.
## ---------------------------------------------------------------------------

function Write-Section {
    param([string]$Text)
    Write-Host '-----------------------------------------'
    Write-Host $Text
    Write-Host '-----------------------------------------'
}

# Write a file as UTF-8 with no BOM and LF endings. See rule 6.
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
    # -L follows the redirects a GitHub release download starts with.
    & $curl -fsSL --max-time $TimeoutSec -o $OutFile $Uri
    if ($LASTEXITCODE -ne 0) {
        throw "curl.exe exited $LASTEXITCODE (Invoke-WebRequest had failed with: $reason)"
    }
}

# Fetch the newest release asset matching a filter, or $null.
#
# Returns $null rather than throwing so that a GitHub rate limit skips one
# optional component with a warning instead of failing the whole install —
# the behaviour the Linux scripts' `|| return 1` produced.
function Get-LatestReleaseAsset {
    param([string]$Repo, [scriptblock]$Filter)
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


## ---------------------------------------------------------------------------
## Preflight
##
## The Linux scripts checked for curl, jq, unzip and tar because the install
## image might not have them. Here those are language features. What is worth
## checking instead is that the daemon handed us a usable environment, and that
## every egg variable the script needs is actually set — a missing one otherwise
## surfaces as an installer doing something bizarre with an empty argument.
##
## TEMPLATE: add a check per required variable.
## ---------------------------------------------------------------------------

if (-not $ServerDir) {
    Write-Host 'SERVER_DIR is not set. This script must be run by win-wings. Aborting.'
    exit 1
}
if (-not (Test-Path $ServerDir)) {
    Write-Host "SERVER_DIR ($ServerDir) does not exist. Aborting."
    exit 1
}


## ---------------------------------------------------------------------------
## Environment diagnostics
##
## These end up in the install log and are the first thing to read when an
## install fails. Keep them cheap: the Linux eggs reported free space and MTU,
## and Get-NetIPInterface can sit for minutes on a host with virtual adapters
## before returning nothing useful.
## ---------------------------------------------------------------------------

Write-Host '----- environment -----'
Write-Host "  SERVER_DIR   = $ServerDir"
Write-Host "  TEMP         = $TempDir"
Write-Host "  PowerShell   = $($PSVersionTable.PSVersion)"
Write-Host "  Running as   = $([System.Security.Principal.WindowsIdentity]::GetCurrent().Name)"
Write-Host '------------------------'


## ---------------------------------------------------------------------------
## OPTIONAL — Steam games
##
## Delete this whole section for an egg that does not use steamcmd.
##
## steamcmd.exe treats its own directory as the Steam root and refuses to
## install into that directory or any directory above it, printing "Please set
## the game install path to something other than the Steam install folder". It
## does not reliably fail when it says so: it ignores +force_install_dir,
## installs into its own steamapps\common, and can still exit 0 — so the game
## lands one directory too deep and the server has nothing to run.
##
## The Linux layout (steamcmd inside the server directory, updating that same
## directory) therefore cannot be ported as-is. win-wings gives every server a
## steamcmd directory beside its files and hands it over as STEAMCMD_DIR. It is
## also where the daemon looks before every start, so that AUTO_UPDATE works:
## put steamcmd anywhere else and the server never updates.
## ---------------------------------------------------------------------------

$SteamCmdDir = $env:STEAMCMD_DIR
$AppId       = $env:SRCDS_APPID

if ($SteamCmdDir -and $AppId) {

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

    $SteamUser = $env:STEAM_USER
    $SteamPass = $env:STEAM_PASS
    $SteamAuth = $env:STEAM_AUTH
    if ([string]::IsNullOrEmpty($SteamUser) -or [string]::IsNullOrEmpty($SteamPass)) {
        Write-Host 'Steam user is not set. Using anonymous user.'
        $SteamUser = 'anonymous'
        $SteamPass = ''
        $SteamAuth = ''
    }

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

    # SteamCMD writes appmanifest_*.acf here and errors out if it is absent.
    New-Item -ItemType Directory -Force -Path (Join-Path $ServerDir 'steamapps') | Out-Null

    Set-Location $SteamCmdDir

    # Notes carried from the Linux eggs, all of which still apply:
    #  - `validate` is skipped on the first attempt: on a clean directory it
    #    only adds a full rehash pass and makes a failure slower to diagnose.
    #  - Partial downloads are NOT deleted between attempts. Wiping
    #    steamapps\downloading turns a transient CDN hiccup into three full-size
    #    redownloads. Only the appinfo cache is cleared, which is the piece that
    #    actually goes stale.
    #  - content_log.txt is dumped on failure rather than discarded.
    #
    # Dropped in the port: @sSteamCmdForcePlatformType. The Linux eggs forced
    # the Windows depot; here the native platform is already the right one and
    # forcing it invites a mismatch with the running SteamCMD.
    $Retries   = 3
    $InstallOk = $false

    for ($i = 1; $i -le $Retries; $i++) {
        Write-Host '==================================================='
        Write-Host "SteamCMD install attempt $i of $Retries"
        Write-Host '==================================================='

        # A corrupt appinfo cache produces "state is 0x202" with 0/0 progress.
        # app_info_update refreshes it; it does not repair it. Delete it.
        if ($i -gt 1) {
            Remove-Item (Join-Path $SteamCmdDir 'appcache\appinfo.vdf') `
                -Force -ErrorAction SilentlyContinue
        }

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

        # This log is shown in the Panel and kept on disk, so credentials are
        # masked. The Linux scripts printed the password in clear.
        $shown = $steamArgs | ForEach-Object {
            if (($SteamPass -and $_ -eq $SteamPass) -or ($SteamAuth -and $_ -eq $SteamAuth)) {
                '<redacted>'
            } elseif ($_ -match '\s') { '"' + $_ + '"' } else { $_ }
        }
        Write-Host "Running command:`n  steamcmd.exe $($shown -join ' ')"

        # RULE 1 and RULE 2 in one place. Do not add a pipeline here, do not
        # assign the result, and do not wrap this in a function whose return
        # value is captured — any of those gives steamcmd a pipe and the
        # download goes silent for its entire duration.
        $previousEAP = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            & $SteamCmdExe @steamArgs
            $steamRc = $LASTEXITCODE
        } finally {
            $ErrorActionPreference = $previousEAP
        }

        # RULE 3. Having refused the install path, steamcmd installs into its
        # own library, which leaves a manifest here that has no business
        # existing. A far sturdier signal than matching its console output.
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

        # RULE 3 again. StateFlags is a bitfield, not a value:
        # k_EAppStateFullyInstalled is bit 4, and a healthy install routinely
        # carries more than that bit alone — 6 is fully installed with an update
        # available, and 1024 shows up while an update is queued. Matching the
        # literal string "4" rejects installs that succeeded.
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

    # The Linux scripts copied steamclient.so into .steam\sdk32 and .steam\sdk64,
    # where the Linux Steamworks redistributable looks. Windows has no such
    # convention: it resolves steamclient64.dll through the Steam registry keys
    # under HKCU, which a server account that has never run the Steam client does
    # not have. A copy beside the server binary is the equivalent.
    #
    # TEMPLATE: point this at the egg's own binary directory.
    #
    # $binariesDir = Join-Path $ServerDir 'MyGame\Binaries\Win64'
    # New-Item -ItemType Directory -Force -Path $binariesDir | Out-Null
    # foreach ($dll in @('steamclient64.dll', 'steamclient.dll',
    #                    'tier0_s64.dll', 'vstdlib_s64.dll')) {
    #     $src = Join-Path $SteamCmdDir $dll
    #     if (Test-Path $src) { Copy-Item $src (Join-Path $binariesDir $dll) -Force }
    # }
}


## ---------------------------------------------------------------------------
## OPTIONAL — .pteroignore
##
## Patterns must start at column 0. The Linux heredoc was indented, which wrote
## leading spaces onto every line, stopped the patterns matching, and put the
## whole install into every backup. Written with the BOM-less writer for the
## same class of reason — see rule 6.
##
## TEMPLATE: list what is worth backing up for this game, or delete the section.
## ---------------------------------------------------------------------------

# $pteroignore = Join-Path $ServerDir '.pteroignore'
# if (-not (Test-Path $pteroignore)) {
#     Write-TextFile -Path $pteroignore -Content @'
# *
# !saves/
# '@
# }


## ---------------------------------------------------------------------------
## Verify
##
## RULE 3, one last time: prove the server has something to run before reporting
## success. This is the check that stops a broken install being marked good.
##
## TEMPLATE: name the file the startup command actually executes.
## ---------------------------------------------------------------------------

# $serverBinary = Join-Path $ServerDir 'MyGameServer.exe'
# if (-not (Test-Path $serverBinary)) {
#     Write-Host "The install finished but $serverBinary is missing. Aborting."
#     exit 1
# }

Write-Section 'Installation completed...'
exit 0
