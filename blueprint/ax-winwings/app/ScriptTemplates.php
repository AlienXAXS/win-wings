<?php

namespace Pterodactyl\BlueprintFramework\Extensions\{identifier};

/**
 * Starting points for a PowerShell install script.
 *
 * Porting an egg is mostly mechanical — the same four or five shapes come up
 * over and over — but the environment differs from a Linux egg in ways that are
 * not obvious until something fails: there is no curl, no tar, no package
 * manager, the script is not root, and the working directory is $env:SERVER_DIR
 * rather than /mnt/server.
 *
 * These templates exist so that the first script an operator writes already has
 * the right shape. They are inserted into the editor and then edited; nothing
 * runs them as-is.
 *
 * Served to the admin page over a route rather than embedded in the Blade file,
 * because Blueprint strips backslash escapes out of inline scripts in a view and
 * PowerShell is nothing but backslashes.
 */
class ScriptTemplates
{
    /**
     * The preamble every template shares.
     *
     * `$ErrorActionPreference = 'Stop'` is the important line: without it a
     * failed Invoke-WebRequest writes an error and carries on, the script exits
     * 0, and the Panel reports a successful install of an empty directory.
     */
    private const HEADER = <<<'PS'
# Runs as: pwsh.exe -NoProfile -NonInteractive -NoLogo -ExecutionPolicy Bypass -File install.ps1
#
#   $env:SERVER_DIR       the server's directory, and this script's working directory
#   $env:INSTALL_RUNTIME  the runtime named by this egg's Windows profile
#   $env:RUNTIME_PATH     that runtime's bin directory, already on PATH
#   plus every variable the egg defines
#
# There is no container. This runs as the server's own unprivileged account, so
# it cannot install software machine-wide -- put anything it needs on the host
# first (scripts/provision-host.ps1 does the usual ones).

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'   # Invoke-WebRequest is ~10x faster without it

Set-Location $env:SERVER_DIR
Write-Host "Installing into $env:SERVER_DIR"

PS;

    public static function all(): array
    {
        return [
            'blank' => [
                'label' => 'Minimal',
                'description' => 'Just the preamble and the environment notes.',
                'script' => self::HEADER . "\nWrite-Host \"Nothing to do.\"\n",
            ],

            'download' => [
                'label' => 'Download a single file',
                'description' => 'The shape most jar- and exe-based eggs need.',
                'script' => self::HEADER . <<<'PS'

# Most eggs boil down to this: fetch one file and leave it in the server
# directory. Invoke-WebRequest replaces curl/wget, which do not exist here.

$url  = "$env:DOWNLOAD_URL"
$dest = Join-Path $env:SERVER_DIR 'server.jar'

if ([string]::IsNullOrWhiteSpace($url)) {
    throw "DOWNLOAD_URL is empty. Set it on the egg, or hardcode a URL here."
}

Write-Host "Downloading $url"
Invoke-WebRequest -Uri $url -OutFile $dest -UseBasicParsing

if (-not (Test-Path $dest) -or (Get-Item $dest).Length -eq 0) {
    throw "Download produced no file."
}

Write-Host "Done."

PS,
            ],

            'archive' => [
                'label' => 'Download and unpack an archive',
                'description' => 'Expand-Archive replaces tar/unzip. Handles zip only.',
                'script' => self::HEADER . <<<'PS'

# Expand-Archive is the only unpacker guaranteed to be present, and it reads zip
# and nothing else. An egg that ships a .tar.gz needs 7-Zip installed on the host.

$url  = "$env:DOWNLOAD_URL"
$temp = Join-Path $env:TEMP ("install-" + [guid]::NewGuid().ToString() + ".zip")

Write-Host "Downloading $url"
Invoke-WebRequest -Uri $url -OutFile $temp -UseBasicParsing

Write-Host "Extracting"
Expand-Archive -Path $temp -DestinationPath $env:SERVER_DIR -Force
Remove-Item $temp -Force

# Many archives unpack into a single top-level directory. Flatten it, because the
# startup command expects the binary at the root of the server directory.
$entries = @(Get-ChildItem -Force $env:SERVER_DIR)
if ($entries.Count -eq 1 -and $entries[0].PSIsContainer) {
    Write-Host "Flattening $($entries[0].Name)"
    Get-ChildItem -Force $entries[0].FullName | Move-Item -Destination $env:SERVER_DIR -Force
    Remove-Item $entries[0].FullName -Recurse -Force
}

Write-Host "Done."

PS,
            ],

            'java' => [
                'label' => 'Java server',
                'description' => 'Checks the runtime resolved, then fetches a jar and writes eula.txt.',
                'script' => self::HEADER . <<<'PS'

# The runtime is not installed by this script and not provided by an image. The
# node maps this egg's runtime name to a directory and puts its bin first on
# PATH, so `java` here is the version the profile asked for -- but only if the
# node's config.yml actually has that name. Fail loudly if it does not, rather
# than silently installing against whatever java happens to be on the host.

$java = Get-Command java.exe -ErrorAction SilentlyContinue
if (-not $java) {
    throw "No java on PATH. The node has no runtime mapped for '$env:INSTALL_RUNTIME'."
}
Write-Host "Using $($java.Source)"
& java.exe -version 2>&1 | ForEach-Object { Write-Host "  $_" }

$url  = "$env:DOWNLOAD_URL"
$dest = Join-Path $env:SERVER_DIR 'server.jar'

Write-Host "Downloading $url"
Invoke-WebRequest -Uri $url -OutFile $dest -UseBasicParsing

# The egg's own variable decides this; do not accept an EULA on someone's behalf.
if ("$env:EULA" -eq 'true') {
    Set-Content -Path (Join-Path $env:SERVER_DIR 'eula.txt') -Value 'eula=true' -Encoding ascii
}

Write-Host "Done."

PS,
            ],

            'steamcmd' => [
                'label' => 'SteamCMD',
                'description' => 'Bootstraps steamcmd into the server directory and runs an app_update.',
                'script' => self::HEADER . <<<'PS'

# steamcmd lives inside the server directory rather than on the host: it
# self-updates, and a shared copy would have several servers rewriting each
# other's binaries.
#
# Set pseudo_console on this egg's profile. steamcmd inspects its stdout and
# emits carriage-return progress lines that a plain pipe turns into unreadable
# output, or drops entirely.

$steamDir = Join-Path $env:SERVER_DIR 'steamcmd'
$steamExe = Join-Path $steamDir 'steamcmd.exe'

if (-not (Test-Path $steamExe)) {
    New-Item -ItemType Directory -Force -Path $steamDir | Out-Null
    $zip = Join-Path $env:TEMP 'steamcmd.zip'
    Write-Host "Fetching steamcmd"
    Invoke-WebRequest -Uri 'https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip' -OutFile $zip -UseBasicParsing
    Expand-Archive -Path $zip -DestinationPath $steamDir -Force
    Remove-Item $zip -Force
}

$appId = "$env:SRCDS_APPID"
if ([string]::IsNullOrWhiteSpace($appId)) {
    throw "SRCDS_APPID is empty."
}

# Built as an array and splatted. A single "user pass" string would reach
# steamcmd as one argument and silently authenticate as nobody.
$steamArgs = @('+force_install_dir', $env:SERVER_DIR)

if ([string]::IsNullOrWhiteSpace($env:STEAM_USER)) {
    $steamArgs += @('+login', 'anonymous')
} else {
    $steamArgs += @('+login', $env:STEAM_USER, $env:STEAM_PASS)
}

$steamArgs += @('+app_update', $appId, 'validate', '+quit')

Write-Host "Running app_update $appId"
& $steamExe @steamArgs

# steamcmd exits 7 on a successful update surprisingly often, and 0 is not the
# only success. Treat anything outside the known-good set as a failure.
if ($LASTEXITCODE -notin @(0, 6, 7)) {
    throw "steamcmd exited $LASTEXITCODE"
}

Write-Host "Done."

PS,
            ],

            'dotnet' => [
                'label' => '.NET server',
                'description' => 'Verifies the .NET runtime resolved before unpacking.',
                'script' => self::HEADER . <<<'PS'

$dotnet = Get-Command dotnet.exe -ErrorAction SilentlyContinue
if (-not $dotnet) {
    throw "No dotnet on PATH. The node has no runtime mapped for '$env:INSTALL_RUNTIME'."
}
Write-Host "Using $($dotnet.Source)"
& dotnet.exe --list-runtimes | ForEach-Object { Write-Host "  $_" }

$url  = "$env:DOWNLOAD_URL"
$temp = Join-Path $env:TEMP ("install-" + [guid]::NewGuid().ToString() + ".zip")

Invoke-WebRequest -Uri $url -OutFile $temp -UseBasicParsing
Expand-Archive -Path $temp -DestinationPath $env:SERVER_DIR -Force
Remove-Item $temp -Force

Write-Host "Done."

PS,
            ],
        ];
    }

    public static function get(string $key): ?array
    {
        return self::all()[$key] ?? null;
    }
}
