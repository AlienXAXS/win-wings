# StarRupture - win-wings pre-start script
#
# Windows port of the Linux egg's startup script, minus the parts the daemon
# now does itself. Runs to completion before every boot, in $env:SERVER_DIR, as
# the server's own account, after any steamcmd update. See "Pre-start scripts"
# in docs\PANEL-API.md for the contract.
#
# What it does:
#   - refuses to configure a session whose save directory exists but is missing
#     AutoSave0.met / AutoSave0.sav
#   - writes DSSettings.txt from the egg's variables, choosing new game vs load
#   - updates the ModLoader when the installed BuildTag is behind the latest
#     GitHub release
#   - enables or disables the ModLoader (DISABLE_MODLOADER)
#   - generates Password.json / PlayerPassword.json once, via the
#     starrupture-utilities.com hashing API
#   - sanity-checks the server executable and records its fingerprint
#
# What the Linux script did that is NOT here, and why:
#   - steamcmd, wine/Proton prefix setup, WINEDLLOVERRIDES, the process dumps:
#     no wine on this host. The Windows depot is the native build.
#   - deleting steamapps\: the daemon's AUTO_UPDATE reads the appmanifest in
#     there before every boot. Removing it forces a full validate each time.
#   - launching the server and tailing its log: the daemon owns the process.
#     Put those in the profile - startup command, Console output -> follow
#     StarRupture/Saved/Logs/StarRupture.log.
#   - the RCON shutdown trap: that is pre-stop.ps1 beside this file, run by the
#     daemon as the egg's pre-stop script.
#   - generating a random RCON password: a value set here cannot reach the
#     startup command, which is substituted from the Panel's variables. Give
#     RCON_PASSWORD a default in the egg instead. This script only warns.
#
##
#
# Variables
# SESSION_NAME      - save session name. Required.
# SAVE_INTERVAL     - autosave interval, written to DSSettings.txt as-is
# RCON_PASSWORD     - RCON password. Warned about if blank, see above
# ADMIN_PASSWORD    - admin password if any
# PLAYER_PASSWORD   - server password if any
# DISABLE_MODLOADER - 1/true/yes to boot vanilla with no mods
#
# Supplied by the daemon rather than the egg:
#   SERVER_DIR      the server's files, and this script's working directory
#   TEMP / TMP      scratch space outside the disk quota and backups
#
##

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

# .NET builds its default web proxy from the WinINET settings under HKCU, which
# an account whose profile has never been loaded does not have. The failure it
# produces mentions neither the registry nor the profile, so turn it into a
# warning here where it can be recognised.
try {
    $null = [System.Net.WebRequest]::DefaultWebProxy
} catch {
    Write-Host "Default web proxy configuration is unreadable ($($_.Exception.Message))."
    Write-Host 'Continuing with no proxy. If this host needs one, web requests will fail.'
    try { [System.Net.WebRequest]::DefaultWebProxy = $null } catch { }
}

$ServerDir = $env:SERVER_DIR

# The daemon points TEMP at a scratch directory outside the sandbox, so nothing
# written there is charged against the disk quota or copied into backups.
$TempDir = if ($env:TEMP) { $env:TEMP } else { [System.IO.Path]::GetTempPath() }
New-Item -ItemType Directory -Force -Path $TempDir | Out-Null


## ---------------------------------------------------------------------------
## Helpers
## ---------------------------------------------------------------------------

# The Linux script's step() banner, without the one-second sleep: the console
# is a log here, not something being watched scroll past.
function Write-Section {
    param([string]$Text)
    Write-Host ''
    Write-Host '========================================='
    Write-Host "  $Text"
    Write-Host '========================================='
}

# Write a file as UTF-8 with no BOM and LF endings. Set-Content -Encoding utf8
# emits a BOM on Windows PowerShell 5.1 and defaults to CRLF, and the game is
# not guaranteed to tolerate either in a file it parses itself.
function Write-TextFile {
    param([string]$Path, [string]$Content)
    $dir = Split-Path -Parent $Path
    if ($dir) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    [System.IO.File]::WriteAllText($Path, ($Content -replace "`r`n", "`n"),
        (New-Object System.Text.UTF8Encoding($false)))
}

# 1/true/yes, case-insensitively, the way the Linux script's ${VAR,,} tests did.
function Test-Flag {
    param([string]$Value)
    return ($Value -match '^(1|true|yes)$')
}

# Download a file, falling back to curl.exe.
#
# Invoke-WebRequest is the obvious tool and the fragile one: on Windows
# PowerShell 5.1 it buffers the whole response in memory, and it drags in the
# whole of .NET's proxy and configuration machinery to make one HTTP request.
# curl.exe has shipped in System32 since Windows 10 1803, talks to WinHTTP
# directly, and is unbothered by all of it.
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

# Fetch the latest release object for a repo, or $null.
#
# jq becomes ConvertFrom-Json, which Invoke-RestMethod does implicitly. Returns
# $null rather than throwing so that a GitHub rate limit skips the optional
# update check with a warning instead of failing the boot.
function Get-LatestRelease {
    param([string]$Repo)
    $headers = @{
        'Accept'     = 'application/vnd.github+json'
        'User-Agent' = 'win-wings-egg'   # GitHub rejects requests without one
    }
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        try {
            return Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
                -Headers $headers -UseBasicParsing -TimeoutSec 60
        } catch {
            Write-Host "  GitHub API attempt ${attempt} failed: $($_.Exception.Message)"
            if ($attempt -lt 3) { Start-Sleep -Seconds 2 }
        }
    }
    return $null
}

# The entry names inside a zip, without extracting it. `unzip -l` has no
# equivalent cmdlet; the framework class works on 5.1 and 7 alike.
function Get-ZipEntryNames {
    param([string]$Path)
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [System.IO.Compression.ZipFile]::OpenRead($Path)
    try {
        return @($zip.Entries | ForEach-Object { $_.FullName })
    } finally {
        $zip.Dispose()
    }
}


## ---------------------------------------------------------------------------
## Preflight
## ---------------------------------------------------------------------------

if (-not $ServerDir) {
    Write-Host 'SERVER_DIR is not set. This script must be run by win-wings. Aborting.'
    exit 1
}
if (-not (Test-Path $ServerDir)) {
    Write-Host "SERVER_DIR ($ServerDir) does not exist. Aborting."
    exit 1
}
if (-not $env:SESSION_NAME) {
    Write-Host 'SESSION_NAME is not set. It is required to name the save session. Aborting.'
    exit 1
}

$SessionName  = $env:SESSION_NAME
$SaveInterval = $env:SAVE_INTERVAL
$BinariesDir  = Join-Path $ServerDir 'StarRupture\Binaries\Win64'
$SaveDir      = Join-Path $ServerDir "StarRupture\Saved\SaveGames\$SessionName"
$ServerExe    = Join-Path $BinariesDir 'StarRuptureServerEOS-Win64-Shipping.exe'

Write-Host 'Starting server, please wait...'
Write-Host '----- environment -----'
Write-Host "  SERVER_DIR   = $ServerDir"
Write-Host "  SESSION_NAME = $SessionName"
Write-Host "  PowerShell   = $($PSVersionTable.PSVersion)"
Write-Host "  Running as   = $([System.Security.Principal.WindowsIdentity]::GetCurrent().Name)"
Write-Host '------------------------'


## ---------------------------------------------------------------------------
## Save files
##
## The Linux script exited here to stop the server booting. The daemon logs a
## non-zero exit from this script and starts the server anyway, so the exit is
## kept for the log, and DSSettings.txt is deliberately NOT rewritten: the
## server boots with whatever it had last time rather than being told to start
## a new game over a world that is merely misnamed.
## ---------------------------------------------------------------------------

Write-Section 'Checking save files'

$autoSaveMet = Join-Path $SaveDir 'AutoSave0.met'
$autoSaveSav = Join-Path $SaveDir 'AutoSave0.sav'

if (Test-Path $SaveDir) {
    Write-Host "Existing save directory detected: $SaveDir"
    Write-Host 'Checking required save files...'

    if (-not (Test-Path $autoSaveMet) -or -not (Test-Path $autoSaveSav)) {
        Write-Host @'
#
#
 /######## /#######  /#######   /######  /#######
| ##_____/| ##__  ##| ##__  ## /##__  ##| ##__  ##
| ##      | ##  \ ##| ##  \ ##| ##  \ ##| ##  \ ##
| #####   | #######/| #######/| ##  | ##| #######/
| ##__/   | ##__  ##| ##__  ##| ##  | ##| ##__  ##
| ##      | ##  \ ##| ##  \ ##| ##  | ##| ##  \ ##
| ########| ##  | ##| ##  | ##|  ######/| ##  | ##
|________/|__/  |__/|__/  |__/ \______/ |__/  |__/
#
#
'@
        Write-Host 'Save directory exists, but required save files are missing.'
        Write-Host 'Expected files:'
        Write-Host "  $autoSaveMet"
        Write-Host "  $autoSaveSav"
        Write-Host 'DSSettings.txt has not been updated.'
        Write-Host 'If this is an existing world, the file names must be exactly:'
        Write-Host '  AutoSave0.met'
        Write-Host '  AutoSave0.sav'
        exit 1
    }

    Write-Host 'Required save files found.'
} else {
    Write-Host "No existing save directory found for session '$SessionName', continuing normally."
}


## ---------------------------------------------------------------------------
## DSSettings.txt
## ---------------------------------------------------------------------------

Write-Section 'Generating DSSettings.txt'

$settingsFile = Join-Path $ServerDir 'DSSettings.txt'

if (Test-Path $autoSaveSav) {
    $startGame = 'false'
    $loadGame  = 'true'
    Write-Host 'Existing AutoSave0.sav detected, server will load saved game.'
} else {
    $startGame = 'true'
    $loadGame  = 'false'
    Write-Host 'No AutoSave0.sav detected, server will start a new game.'
}

# The booleans are strings on purpose: that is what the Linux heredoc produced
# and what the server has been parsing all along.
$settings = [ordered]@{
    SessionName      = $SessionName
    SaveGameInterval = $SaveInterval
    StartNewGame     = $startGame
    LoadSavedGame    = $loadGame
    SaveGameName     = 'AutoSave0.sav'
}
$settingsJson = $settings | ConvertTo-Json
Write-TextFile -Path $settingsFile -Content ($settingsJson + "`n")

Write-Host "DSSettings.txt created at $settingsFile"
Write-Host 'Contents:'
Write-Host $settingsJson


## ---------------------------------------------------------------------------
## RCON
##
## The Linux script generated a password here and passed it on the command
## line it then ran. This script runs nothing: the startup command is built by
## the daemon from the Panel's variables, and nothing set here reaches it.
## ---------------------------------------------------------------------------

Write-Section 'Configuring RCON'

if ([string]::IsNullOrEmpty($env:RCON_PASSWORD)) {
    Write-Host 'WARNING: RCON_PASSWORD is empty.'
    Write-Host '         The server will start without a usable RCON password, so the'
    Write-Host '         Stop command and console commands will not reach it.'
    Write-Host '         Set a value for RCON_PASSWORD in the Panel.'
} else {
    Write-Host 'RCON password is set.'
}


## ---------------------------------------------------------------------------
## ModLoader update check
##
## Compares the BuildTag in update_state.ini against the build_tag in the
## latest release's manifest-server.json, and extracts the Server zip over
## Binaries\Win64 when they differ. That is the directory the game executable
## lives in, so the archive is listed first and refused if it would overwrite
## the server binary.
## ---------------------------------------------------------------------------

Write-Section 'Checking for ModLoader updates'

$updateStateFile = $null
foreach ($candidate in @(
    (Join-Path $BinariesDir 'update_state.ini'),
    (Join-Path $BinariesDir 'ModLoader\update_state.ini')
)) {
    if (Test-Path $candidate) { $updateStateFile = $candidate; break }
}

if (-not $updateStateFile) {
    Write-Host 'update_state.ini not found in either expected location, skipping ModLoader update check.'
} else {
    Write-Host "Found update_state.ini at $updateStateFile"

    $currentBuildTag = ''
    $buildTagLine = Get-Content $updateStateFile | Where-Object { $_ -match '^\s*BuildTag\s*=' } |
        Select-Object -First 1
    if ($buildTagLine) {
        $currentBuildTag = (($buildTagLine -split '=', 2)[1]) -replace '[\s"]', ''
    }
    Write-Host "Current BuildTag: $currentBuildTag"

    Write-Host 'Fetching latest release info from GitHub API...'
    $release = Get-LatestRelease -Repo 'AlienXAXS/StarRupture-ModLoader'

    if (-not $release -or -not $release.tag_name) {
        Write-Host 'Warning: GitHub API did not return a valid release (rate limited, network issue, or repo/API problem). Skipping ModLoader update check.'
    } else {
        Write-Host "Latest release tag: $($release.tag_name)"

        $manifestAsset = $release.assets | Where-Object { $_.name -match 'manifest-server\.json$' } |
            Select-Object -First 1

        if (-not $manifestAsset) {
            Write-Host "Warning: no manifest-server.json asset found on release $($release.tag_name), skipping update check."
        } else {
            Write-Host "Fetching manifest from $($manifestAsset.browser_download_url)..."
            $latestBuildTag = ''
            try {
                $manifest = Invoke-RestMethod -Uri $manifestAsset.browser_download_url `
                    -Headers @{ 'User-Agent' = 'win-wings-egg' } -UseBasicParsing -TimeoutSec 60
                if ($manifest.build_tag) { $latestBuildTag = [string]$manifest.build_tag }
            } catch {
                Write-Host "Warning: could not fetch the manifest: $($_.Exception.Message)"
            }
            Write-Host "Latest manifest build_tag: $latestBuildTag"

            if (-not $latestBuildTag -or $currentBuildTag -eq $latestBuildTag) {
                Write-Host 'ModLoader is up to date, no action needed.'
            } else {
                $shownCurrent = if ($currentBuildTag) { $currentBuildTag } else { 'none' }
                Write-Host "BuildTag mismatch (installed: $shownCurrent / latest: $latestBuildTag). Updating ModLoader..."

                $serverAsset = $release.assets | Where-Object { $_.name -match 'Server.*\.zip$' } |
                    Select-Object -First 1

                if (-not $serverAsset) {
                    Write-Host "Warning: could not locate a Server .zip asset on release $($release.tag_name), skipping update."
                } else {
                    $tmpZip = Join-Path $TempDir $serverAsset.name
                    Write-Host "Downloading $($serverAsset.name) from $($serverAsset.browser_download_url)..."
                    try {
                        Get-RemoteFile -Uri $serverAsset.browser_download_url -OutFile $tmpZip
                    } catch {
                        Write-Host "Warning: ModLoader Server asset failed to download ($($_.Exception.Message)), skipping update."
                    }

                    if (Test-Path $tmpZip) {
                        Write-Host "Extracting to $BinariesDir ..."
                        $entries = @()
                        try {
                            $entries = Get-ZipEntryNames -Path $tmpZip
                        } catch {
                            Write-Host "Warning: could not read the archive ($($_.Exception.Message)), skipping update."
                        }

                        if ($entries.Count -gt 0) {
                            Write-Host 'Archive contents:'
                            $entries | Select-Object -First 40 | ForEach-Object { Write-Host "  $_" }

                            if ($entries -match 'StarRuptureServerEOS-Win64-Shipping\.exe') {
                                Write-Host 'REFUSING to extract: this archive contains the server executable.'
                                Write-Host 'Extracting it would overwrite the game binary. Skipping ModLoader update.'
                            } else {
                                Expand-Archive -Path $tmpZip -DestinationPath $BinariesDir -Force
                                Write-Host 'ModLoader update extracted.'
                            }
                        }
                        Remove-Item $tmpZip -Force -ErrorAction SilentlyContinue
                    }
                }
            }
        }
    }
}


## ---------------------------------------------------------------------------
## ModLoader on/off
##
## The ModLoader ships as a dwmapi.dll proxy next to the server exe, so the
## Windows loader picks it up before any Unreal code runs. That makes it the
## first thing to rule out when the server starts but never writes a log line.
##
## Under wine the Linux script disabled it by dropping the DLL override and
## leaving the file alone. There is no override on Windows: the loader takes
## the application directory first, so the only way to stop the proxy loading
## is for it not to be there. It is renamed rather than deleted, and renamed
## back when DISABLE_MODLOADER is cleared - the same mechanism an older version
## of the Linux script used, kept here because it is the one that exists.
## ---------------------------------------------------------------------------

Write-Section 'Checking ModLoader'

$modLoaderDll      = Join-Path $BinariesDir 'dwmapi.dll'
$modLoaderDisabled = "$modLoaderDll.disabled"

if (Test-Flag $env:DISABLE_MODLOADER) {
    Write-Host 'DISABLE_MODLOADER is set - starting VANILLA (no mods will load).'
    if (Test-Path $modLoaderDll) {
        # A ModLoader update above may have just written a fresh dwmapi.dll
        # beside a stale .disabled copy; the fresh one wins.
        Move-Item -Path $modLoaderDll -Destination $modLoaderDisabled -Force
        Write-Host '  - dwmapi.dll renamed to dwmapi.dll.disabled so it is never loaded.'
    } elseif (Test-Path $modLoaderDisabled) {
        Write-Host '  - dwmapi.dll is already disabled.'
    } else {
        Write-Host '  - no dwmapi.dll present, nothing to disable.'
    }
} else {
    if (Test-Path $modLoaderDisabled) {
        if (Test-Path $modLoaderDll) {
            # An update above extracted a fresh dwmapi.dll over the top; the
            # disabled copy is older and has nothing left to say.
            Remove-Item $modLoaderDisabled -Force
            Write-Host 'Removed stale dwmapi.dll.disabled left by an earlier boot.'
        } else {
            Move-Item -Path $modLoaderDisabled -Destination $modLoaderDll -Force
            Write-Host 'Restored dwmapi.dll that a previous boot had disabled.'
        }
    }
    if (Test-Path $modLoaderDll) {
        $dll = Get-Item $modLoaderDll
        Write-Host "ModLoader present: $($dll.Length) bytes, modified $($dll.LastWriteTime.ToString('yyyy-MM-dd HH:mm:ss'))"
    } else {
        Write-Host 'No dwmapi.dll found, server will run without the ModLoader.'
    }
}


## ---------------------------------------------------------------------------
## Password files
##
## The game wants hashed passwords in Password.json and PlayerPassword.json,
## and the hashing is done by starrupture-utilities.com. Both fields are always
## posted, as the Linux script did, and only the missing files are written.
##
## Invoke-RestMethod -Form does not exist on Windows PowerShell 5.1, so the
## multipart body is assembled by hand. It is four lines per field.
## ---------------------------------------------------------------------------

Write-Section 'Generating password files'

$adminPassword  = $env:ADMIN_PASSWORD
$playerPassword = $env:PLAYER_PASSWORD
$adminFile      = Join-Path $ServerDir 'Password.json'
$playerFile     = Join-Path $ServerDir 'PlayerPassword.json'

if ([string]::IsNullOrEmpty($adminPassword) -and [string]::IsNullOrEmpty($playerPassword)) {
    Write-Host 'No passwords set, skipping password file generation'
} elseif ((Test-Path $adminFile) -and (Test-Path $playerFile)) {
    Write-Host 'Both password files already exist, skipping generation'
} else {
    Write-Host 'At least one password is set, checking for existing files...'
    Write-Host 'One or more password files missing, generating...'

    $boundary = [guid]::NewGuid().ToString('N')
    $crlf = "`r`n"
    $body = New-Object System.Text.StringBuilder
    foreach ($field in @(@('adminpassword', $adminPassword), @('playerpassword', $playerPassword))) {
        [void]$body.Append("--$boundary$crlf")
        [void]$body.Append("Content-Disposition: form-data; name=`"$($field[0])`"$crlf$crlf")
        [void]$body.Append("$($field[1])$crlf")
    }
    [void]$body.Append("--$boundary--$crlf")
    # Bytes rather than a string, so the encoding is UTF-8 on 5.1 as well.
    $bodyBytes = [System.Text.Encoding]::UTF8.GetBytes($body.ToString())

    $response = $null
    try {
        $response = Invoke-RestMethod -Uri 'https://starrupture-utilities.com/passwords/' -Method Post `
            -ContentType "multipart/form-data; boundary=$boundary" -Body $bodyBytes `
            -UseBasicParsing -TimeoutSec 60
    } catch {
        Write-Host "Warning: password API request failed ($($_.Exception.Message)), cannot generate password files"
    }

    # The Linux script echoed the whole response. The hashes are secrets in
    # their own right and this log is kept, so only their lengths are shown.
    if ($response) {
        Write-Host 'API response received.'

        if (-not [string]::IsNullOrEmpty($adminPassword) -and -not (Test-Path $adminFile)) {
            Write-Host 'Generating Password.json...'
            $adminHash = [string]$response.adminpassword
            Write-Host "Extracted adminpassword, length: $($adminHash.Length) chars"
            if ($adminHash) {
                Write-TextFile -Path $adminFile -Content ((@{ password = $adminHash } | ConvertTo-Json) + "`n")
                Write-Host "Password.json created, size: $((Get-Item $adminFile).Length) bytes"
            } else {
                Write-Host 'Warning: adminpassword was empty or null in API response, skipping Password.json'
            }
        }

        if (-not [string]::IsNullOrEmpty($playerPassword) -and -not (Test-Path $playerFile)) {
            Write-Host 'Generating PlayerPassword.json...'
            $playerHash = [string]$response.playerpassword
            Write-Host "Extracted playerpassword, length: $($playerHash.Length) chars"
            if ($playerHash) {
                Write-TextFile -Path $playerFile -Content ((@{ password = $playerHash } | ConvertTo-Json) + "`n")
                Write-Host "PlayerPassword.json created, size: $((Get-Item $playerFile).Length) bytes"
            } else {
                Write-Host 'Warning: playerpassword was empty or null in API response, skipping PlayerPassword.json'
            }
        }
    } elseif ($null -eq $response) {
        # Already reported above, or the API returned nothing at all.
        Write-Host 'Warning: API response was empty, cannot generate password files'
    }
}


## ---------------------------------------------------------------------------
## Server executable
##
## The daemon is about to run it. A missing binary is reported here with more
## context than the launch failure would give, and a truncated one is worse
## than a missing one - it starts and dies with no useful output. The
## fingerprint is kept between boots so that a binary silently changing or
## disappearing can be tied to the boot it happened on, and to whether that
## coincided with a ModLoader update.
## ---------------------------------------------------------------------------

Write-Section 'Checking server executable'

Write-Host "  SESSION_NAME:   $SessionName"
Write-Host "  SAVE_INTERVAL:  $SaveInterval"

if (-not (Test-Path $ServerExe)) {
    Write-Host "FATAL: server executable not found at $ServerExe"
    Write-Host 'The rest of the install may still be intact - this usually means a'
    Write-Host 'SteamCMD update was interrupted part way through writing the binary,'
    Write-Host 'or the server ran out of disk. Reinstall from the panel to restore it.'
    if (Test-Path $BinariesDir) {
        Write-Host 'Contents of Binaries\Win64:'
        Get-ChildItem $BinariesDir | Select-Object -First 40 |
            ForEach-Object { Write-Host ("  {0,12} {1}" -f $_.Length, $_.Name) }
    } else {
        Write-Host "Binaries\Win64 does not exist at $BinariesDir"
    }
    exit 1
}

$exe     = Get-Item $ServerExe
$exeSize = $exe.Length
Write-Host "  SERVER_EXE:     $ServerExe ($exeSize bytes)"
# A shipping UE server build is tens of MB.
if ($exeSize -lt 1000000) {
    Write-Host "WARNING: the server executable is suspiciously small ($exeSize bytes)."
    Write-Host '         It is very likely truncated or corrupt - consider a reinstall.'
}

$exeStateFile   = Join-Path $ServerDir '.starrupture_exe_state'
$exeFingerprint = "$exeSize bytes, mtime $($exe.LastWriteTimeUtc.ToString('yyyy-MM-dd HH:mm:ss')) UTC"
if (Test-Path $exeStateFile) {
    $prevFingerprint = (Get-Content $exeStateFile -Raw -ErrorAction SilentlyContinue)
    if ($prevFingerprint -and $prevFingerprint.Trim() -ne $exeFingerprint) {
        Write-Host '  NOTE: the server executable changed since the last boot.'
        Write-Host "        was: $($prevFingerprint.Trim())"
        Write-Host "        now: $exeFingerprint"
    }
}
Write-TextFile -Path $exeStateFile -Content $exeFingerprint

Write-Host '-----------------------------------------'
Write-Host 'Pre-start complete, handing over to the server.'
exit 0
