# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# install-cli.ps1 - install the `wardyn` command-line client for the person
# running it. A release asset: the release job writes its own version into the
# $ReleaseVersion line below.
#
#   irm https://github.com/cjohnstoniv/wardyn/releases/download/v<version>/install-cli.ps1 | iex
#
# This installs `wardyn` and nothing else. It is not install.sh (the platform
# installer). Run in WSL2 instead and the macOS/Linux script, install-cli.sh,
# applies.
#
# `irm | iex` cannot pass an argument, so the options are also read from the
# environment: WARDYN_ARTIFACT_BASE (an internal mirror: the assets are fetched
# from <base>/v<version>/), WARDYN_VERSION. A parameter wins over its variable.
#
#   $env:WARDYN_ARTIFACT_BASE='https://mirror.example/wardyn'; irm <base>/v<version>/install-cli.ps1 | iex
#
# Parameters: -Version, -ArtifactBase, -RequireSignature (refuse unless cosign
# verified the checksum file), -Org (print the exact `wardyn login <url>` line).
# To pass them: & ([scriptblock]::Create((irm <url>))) -ArtifactBase <url>
#
# Windows' own certificate store is used for the downloads, which is where an
# organisation's managed roots live; there is no -CaFile.
#
# It installs per user, with no elevation, into %LOCALAPPDATA%\Wardyn\bin and adds
# that directory to the user PATH. It writes a sibling file and renames it over
# the target (a running wardyn.exe is renamed aside first), never writing in
# place. It never signs in, never writes a token and never writes the CLI profile.
param(
  [string]$Version = '',
  [string]$ArtifactBase = '',
  [switch]$RequireSignature,
  [string]$Org = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$Repo = 'cjohnstoniv/wardyn'
# The release job rewrites this one line (and only this line).
$ReleaseVersion = '@WARDYN_VERSION@'
$Issuer = 'https://token.actions.githubusercontent.com'

function Say([string]$m) { Write-Host "==> $m" }
function Stop-Install([string]$m) { throw "error: $m" }

try {
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

  $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
  if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Stop-Install 'refusing to run elevated: wardyn installs for one person, into their own directory. Run this from a normal PowerShell window.'
  }

  if (-not $Version) { $Version = $env:WARDYN_VERSION }
  if (-not $Version) { $Version = $ReleaseVersion }
  if ($Version.StartsWith('@')) { Stop-Install 'this copy of install-cli.ps1 was not stamped with a release version: pass -Version <x.y.z>, or use the copy attached to the release' }
  $Version = $Version.TrimStart('v')
  if ($Version -notmatch '^[0-9A-Za-z.+-]+$') { Stop-Install "not a release version: $Version" }

  # The parameter wins over WARDYN_ARTIFACT_BASE.
  $base = $ArtifactBase
  if (-not $base) { $base = $env:WARDYN_ARTIFACT_BASE }
  if (-not $base) { $base = "https://github.com/$Repo/releases/download" }
  $base = $base.TrimEnd('/')
  if (-not $base.StartsWith('https://')) { Stop-Install "the artifact base must be an https:// URL, got: $base" }
  if ($Org -and $Org -notmatch '^https?://[^\s"''`$;&|<>\\()]+$') { Stop-Install "-Org must be an http:// or https:// URL, got: $Org" }

  switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Stop-Install "no wardyn CLI is published for $($env:PROCESSOR_ARCHITECTURE) (published: amd64 and arm64)" }
  }
  $asset = "wardyn-windows-$arch.exe"
  $rel = "$base/v$Version"

  $cosign = Get-Command cosign -ErrorAction SilentlyContinue
  if ($RequireSignature -and -not $cosign) {
    Stop-Install '-RequireSignature: cosign is not on PATH, so the signature cannot be checked. Install cosign (https://docs.sigstore.dev/cosign/installation/), or drop the switch and accept the checksum alone.'
  }

  $dir = Join-Path $env:LOCALAPPDATA 'Wardyn\bin'
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $work = Join-Path ([IO.Path]::GetTempPath()) ("wardyn-install-" + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Force -Path $work | Out-Null
  $stage = Join-Path $dir (".wardyn.new." + [Guid]::NewGuid().ToString('N') + '.exe')

  try {
    function Get-Asset([string]$name, [string]$dest) {
      try { Invoke-WebRequest -UseBasicParsing -Uri "$rel/$name" -OutFile $dest }
      catch { Stop-Install "could not fetch $name from $rel/$name ($($_.Exception.Message)). A mirror must hold the release's own layout, $base/v$Version/<asset>, under the same asset names." }
    }

    Say "installing wardyn $Version ($asset) from $rel"
    Get-Asset 'SHA256SUMS' (Join-Path $work 'SHA256SUMS')

    if ($cosign) {
      Get-Asset 'SHA256SUMS.sig' (Join-Path $work 'SHA256SUMS.sig')
      Get-Asset 'SHA256SUMS.pem' (Join-Path $work 'SHA256SUMS.pem')
      $identity = "https://github.com/$Repo/.github/workflows/release.yml@refs/tags/v$Version"
      $log = & cosign verify-blob (Join-Path $work 'SHA256SUMS') --signature (Join-Path $work 'SHA256SUMS.sig') `
        --certificate (Join-Path $work 'SHA256SUMS.pem') --certificate-identity $identity `
        --certificate-oidc-issuer $Issuer 2>&1
      if ($LASTEXITCODE -ne 0) {
        $log | Select-Object -Last 5 | ForEach-Object { Write-Host $_ }
        Stop-Install "cosign refused the signature on SHA256SUMS (identity $identity); nothing was installed"
      }
      Say "signature: verified with cosign against $identity"
      Say '  a script piped from the same origin also names this identity, so for a piped run that line is what you check by hand'
    } else {
      Say 'signature: not checked (cosign not found)'
    }

    Get-Asset $asset $stage
    $want = $null
    foreach ($line in Get-Content (Join-Path $work 'SHA256SUMS')) {
      $f = $line -split '\s+', 2
      if ($f.Count -eq 2 -and ($f[1].Trim().TrimStart('*').TrimStart('.', '/') -eq $asset)) { $want = $f[0].ToLowerInvariant(); break }
    }
    if (-not $want) { Stop-Install "SHA256SUMS lists no $asset; nothing was installed" }
    $got = (Get-FileHash -Algorithm SHA256 -Path $stage).Hash.ToLowerInvariant()
    if ($want -ne $got) { Stop-Install "checksum mismatch for $asset (want $want, got $got); nothing was installed" }
    Say 'checksum: matches SHA256SUMS from the same place: catches a broken download, not a tampered release'

    $target = Join-Path $dir 'wardyn.exe'
    if (Test-Path $target) {
      # A running wardyn.exe cannot be overwritten, but it can be renamed.
      $old = Join-Path $dir 'wardyn.exe.old'
      Remove-Item -Force -ErrorAction SilentlyContinue $old
      Move-Item -Force -Path $target -Destination $old
    }
    Move-Item -Force -Path $stage -Destination $target
    Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $dir 'wardyn.exe.old')
    Say "installed $target"
  } finally {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $work
    Remove-Item -Force -ErrorAction SilentlyContinue $stage
  }

  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (-not (($userPath -split ';') -contains $dir)) {
    [Environment]::SetEnvironmentVariable('Path', (($userPath, $dir | Where-Object { $_ }) -join ';'), 'User')
    Say "added $dir to your PATH; open a new terminal to use it"
  }
  if (-not (($env:Path -split ';') -contains $dir)) { $env:Path = "$env:Path;$dir" }

  if ($Org) { Say "Next: wardyn login $Org" }
  else { Say "Next: wardyn login <your organisation's URL>" }
} catch {
  # `iex` must not close the person's terminal, so no `exit`: report and set the code.
  Write-Host ($_.Exception.Message) -ForegroundColor Red
  $global:LASTEXITCODE = 1
}
