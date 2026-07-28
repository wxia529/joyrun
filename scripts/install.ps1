[CmdletBinding()]
param(
    [string]$Version = "latest",
    [string]$InstallDir = $(if ($env:JOYRUN_INSTALL_DIR) {
        $env:JOYRUN_INSTALL_DIR
    } else {
        Join-Path $env:LOCALAPPDATA "Programs\JoyRun"
    }),
    [switch]$Check,
    [switch]$AddToPath
)

$ErrorActionPreference = "Stop"
$Repository = "wxia529/joyrun"
$ReleasesUrl = "https://github.com/$Repository/releases"

function Fail([string]$Message) {
    throw "joyrun installer: $Message"
}

function Test-Version([string]$Value) {
    if ($Value -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') {
        Fail "invalid release version: $Value"
    }
}

function Resolve-LatestVersion {
    $headers = @{
        Accept = "application/vnd.github+json"
        "User-Agent" = "JoyRun-Installer"
    }
    $release = Invoke-RestMethod `
        -Uri "https://api.github.com/repos/$Repository/releases/latest" `
        -Headers $headers
    $resolved = [string]$release.tag_name
    Test-Version $resolved
    return $resolved
}

function Get-InstalledVersion {
    $target = Join-Path $InstallDir "joyrun.exe"
    $binary = $null
    if (Test-Path -LiteralPath $target -PathType Leaf) {
        $binary = $target
    } else {
        $command = Get-Command joyrun -CommandType Application -ErrorAction SilentlyContinue
        if ($command) {
            $binary = $command.Source
        }
    }
    if (-not $binary) {
        return $null
    }
    try {
        $reported = (& $binary version 2>$null | Out-String).Trim()
        if ($reported -match '^joyrun (.+)$') {
            return $Matches[1]
        }
    } catch {
        return $null
    }
    return $null
}

function Get-ExpectedChecksum(
    [string]$ChecksumsPath,
    [string]$AssetName
) {
    foreach ($line in Get-Content -LiteralPath $ChecksumsPath) {
        if ($line -match '^([0-9a-fA-F]{64})\s+\*?(.+)$') {
            if ($Matches[2].Trim() -eq $AssetName) {
                return $Matches[1].ToLowerInvariant()
            }
        }
    }
    Fail "SHA256SUMS does not contain a valid checksum for $AssetName"
}

if ($Version -eq "latest") {
    $Version = Resolve-LatestVersion
} else {
    Test-Version $Version
}

$current = Get-InstalledVersion
if ($Check) {
    if ($current) {
        Write-Output "Installed: $current"
    } else {
        Write-Output "Installed: not found"
    }
    Write-Output "Latest stable: $Version"
    exit 0
}

$architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
if ($architecture -ne [System.Runtime.InteropServices.Architecture]::X64) {
    Fail "unsupported Windows CPU architecture: $architecture (JoyRun currently publishes windows/amd64)"
}

$package = "joyrun-$Version-windows-amd64"
$asset = "$package.zip"
$downloadBase = "$ReleasesUrl/download/$Version"
$temporaryDir = Join-Path ([System.IO.Path]::GetTempPath()) `
    "joyrun-install-$([Guid]::NewGuid().ToString('N'))"

New-Item -ItemType Directory -Path $temporaryDir | Out-Null
try {
    $archive = Join-Path $temporaryDir $asset
    $checksums = Join-Path $temporaryDir "SHA256SUMS"

    Write-Output "Downloading JoyRun $Version for windows/amd64..."
    Invoke-WebRequest -UseBasicParsing -Uri "$downloadBase/$asset" -OutFile $archive
    Invoke-WebRequest -UseBasicParsing -Uri "$downloadBase/SHA256SUMS" -OutFile $checksums

    $expected = Get-ExpectedChecksum $checksums $asset
    $actual = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        Fail "checksum verification failed for $asset"
    }

    Expand-Archive -LiteralPath $archive -DestinationPath $temporaryDir
    $candidate = Join-Path (Join-Path $temporaryDir $package) "joyrun.exe"
    if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) {
        Fail "release archive does not contain $package/joyrun.exe"
    }

    $candidateVersion = (& $candidate version 2>$null | Out-String).Trim()
    if ($candidateVersion -ne "joyrun $Version") {
        Fail "downloaded binary reports an unexpected version: $candidateVersion"
    }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $resolvedInstallDir = (Resolve-Path -LiteralPath $InstallDir).Path
    $target = Join-Path $resolvedInstallDir "joyrun.exe"
    $backup = Join-Path $resolvedInstallDir "joyrun.previous.exe"
    $staged = Join-Path $resolvedInstallDir ".joyrun.new.$([Guid]::NewGuid().ToString('N')).exe"
    Copy-Item -LiteralPath $candidate -Destination $staged

    if (Test-Path -LiteralPath $target -PathType Leaf) {
        if (Test-Path -LiteralPath $backup) {
            Remove-Item -LiteralPath $backup -Force
        }
        [System.IO.File]::Replace($staged, $target, $backup, $true)
    } else {
        [System.IO.File]::Move($staged, $target)
    }

    $installedVersion = (& $target version 2>$null | Out-String).Trim()
    if ($installedVersion -ne "joyrun $Version") {
        if (Test-Path -LiteralPath $backup -PathType Leaf) {
            [System.IO.File]::Replace($backup, $target, $null, $true)
        } elseif (Test-Path -LiteralPath $target) {
            Remove-Item -LiteralPath $target -Force
        }
        Fail "installed binary failed verification; the previous installation was restored"
    }

    Write-Output "Installed JoyRun $Version to $target"
    if ($current -and $current -ne $Version) {
        Write-Output "Previous version: $current"
    }

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $pathEntries = @($userPath -split ';' | Where-Object { $_ })
    $onUserPath = $pathEntries | Where-Object {
        $_.TrimEnd('\') -ieq $resolvedInstallDir.TrimEnd('\')
    }
    if (-not $onUserPath) {
        if ($AddToPath) {
            $newUserPath = if ($userPath) {
                "$userPath;$resolvedInstallDir"
            } else {
                $resolvedInstallDir
            }
            [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
            $env:Path = "$env:Path;$resolvedInstallDir"
            Write-Output "Added $resolvedInstallDir to the user PATH."
        } else {
            Write-Warning "Add $resolvedInstallDir to the user PATH, or rerun with -AddToPath."
        }
    }
} finally {
    if (Test-Path -LiteralPath $temporaryDir) {
        Remove-Item -LiteralPath $temporaryDir -Recurse -Force
    }
}
