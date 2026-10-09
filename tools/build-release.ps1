<#
  SwagCod local release build with updater signing.
  Mirrors the DSH Phone script (qwe/scripts/build-release.ps1).

  Why a script: tauri.conf.json has createUpdaterArtifacts: true, so the
  bundler MUST sign artifacts and fails without TAURI_SIGNING_*. The
  private key + password live in the user profile (outside the repo, so
  they can never be committed).

  Usage:
      powershell -NoProfile -ExecutionPolicy Bypass -File tools\build-release.ps1
      ...  -Sign            # sign updater artifacts
      ...  -Sign -Latest    # also compose latest.json manifest

  Key (generated once, already in ~/.swagcod-updater):
      npx tauri signer generate -w <path>\swagcod-updater.key

  ASCII-only on purpose: Windows PowerShell 5.1 reads BOM-less UTF-8 as
  ANSI and chokes on Cyrillic strings (lesson of revision 27).
#>
[CmdletBinding()]
param(
    # Sign updater artifacts. Without this the bundle builds but no .sig
    # is produced, i.e. auto-update cannot be verified for this build.
    [switch]$Sign,
    # Compose latest.json - the manifest the in-app updater reads.
    [switch]$Latest,
    [string]$KeyDir = (Join-Path $env:USERPROFILE '.swagcod-updater'),
    [string]$Notes = ''
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

Write-Host '== SwagCod: release build ==' -ForegroundColor Cyan
Write-Host "repo root: $root"

# --- 0. app + sidecars must not hold files -------------------------------
Stop-Process -Name SwagCod -Force -ErrorAction SilentlyContinue
Get-CimInstance Win32_Process -Filter "Name='node.exe'" |
    Where-Object { $_.CommandLine -match 'store-server|plugin-server|fake-mcp' } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Start-Sleep -Seconds 1

# --- 1. signing key -------------------------------------------------------
$keyPath = Join-Path $KeyDir 'swagcod-updater.key'
$pwPath = Join-Path $KeyDir 'password.txt'
if ($Sign) {
    if (-not (Test-Path $keyPath)) { throw "no private key: $keyPath" }
    if (-not (Test-Path $pwPath)) { throw "no password file: $pwPath" }
    $env:TAURI_SIGNING_PRIVATE_KEY = Get-Content $keyPath -Raw
    $env:TAURI_SIGNING_PRIVATE_KEY_PASSWORD = (Get-Content $pwPath -Raw).Trim()
    Write-Host 'signing: key loaded' -ForegroundColor Green
} else {
    Write-Host 'signing OFF (pass -Sign for updater artifacts)' -ForegroundColor Yellow
    # empty values - otherwise tauri picks them up from the environment
    $env:TAURI_SIGNING_PRIVATE_KEY = ''
    $env:TAURI_SIGNING_PRIVATE_KEY_PASSWORD = ''
}

# --- 2. vendor payload (bundled node + sidecar = installer resources) ----
$needVendor = (-not (Test-Path (Join-Path $root 'vendor\node\node.exe'))) -or
              (-not (Test-Path (Join-Path $root 'vendor\sidecar\store-server.js')))
if ($needVendor) {
    Write-Host '== assembling vendor (fetch-node + bundle-sidecar) ==' -ForegroundColor Cyan
    powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'fetch-node.ps1')
    powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'bundle-sidecar.ps1')
}

# --- 3. frontend (pnpm is broken on this machine; use the vite shim) -----
Write-Host '== frontend (vite build) ==' -ForegroundColor Cyan
Push-Location (Join-Path $root 'apps\desktop')
try {
    & .\node_modules\.bin\vite.CMD build
    if ($LASTEXITCODE -ne 0) { throw "vite build failed ($LASTEXITCODE)" }
} finally {
    Pop-Location
}

# --- 4. bundle (tauri build; beforeBuildCommand disabled via override) ---
Write-Host '== tauri build (slow) ==' -ForegroundColor Cyan
$cfg = Join-Path $PSScriptRoot 'tauri-nobuild.json'
Push-Location (Join-Path $root 'crates\app')
try {
    & (Join-Path $root 'apps\desktop\node_modules\.bin\tauri.CMD') build --config $cfg
    $code = $LASTEXITCODE
} finally {
    Pop-Location
}
if ($code -ne 0) { throw "tauri build failed ($code)" }

# --- 5. artifact layout ---------------------------------------------------
$rel = Join-Path $root 'releases'
New-Item -ItemType Directory -Force -Path $rel | Out-Null
$ver = (Get-Content (Join-Path $root 'crates\app\tauri.conf.json') -Raw | ConvertFrom-Json).version
$nsis = Join-Path $root 'target\release\bundle\nsis'
$exe = Get-Item (Join-Path $root 'target\release\swagcod-app.exe') -ErrorAction SilentlyContinue

# The nsis dir accumulates setups of all past versions, and name sorting
# puts 0.1.0 before 0.10.0 - take strictly the current version, else newest.
function Pick-Artifact {
    param([string]$Dir, [string]$Pattern, [string]$MustContain)
    $all = @(Get-ChildItem $Dir -Filter $Pattern -ErrorAction SilentlyContinue)
    if ($all.Count -eq 0) { return $null }
    $exact = $all | Where-Object { $_.Name -like "*$MustContain*" }
    if ($exact) { return ($exact | Sort-Object LastWriteTime -Descending | Select-Object -First 1) }
    return ($all | Sort-Object LastWriteTime -Descending | Select-Object -First 1)
}

$setup = Pick-Artifact $nsis '*-setup.exe' $ver
# .sig must belong to THIS setup, else the signature will not match the file
$sig = $null
if ($setup) {
    $want = Join-Path $nsis ($setup.Name + '.sig')
    if (Test-Path $want) { $sig = Get-Item $want } else { $sig = Pick-Artifact $nsis '*.sig' $ver }
}
if ($setup -and $setup.Name -notlike "*$ver*") {
    Write-Warning "found setup $($setup.Name) but config version is $ver - stale build?"
}

if ($exe) {
    Copy-Item $exe.FullName (Join-Path $rel 'SwagCod.exe') -Force
    # Root SwagCod.exe is the portable copy the user double-clicks; keep it
    # fresh. App was stopped in step 0, so the file is not locked.
    try {
        Copy-Item $exe.FullName (Join-Path $root 'SwagCod.exe') -Force -ErrorAction Stop
        Write-Host 'root SwagCod.exe updated' -ForegroundColor Green
    } catch {
        Write-Warning "root SwagCod.exe not updated: $($_.Exception.Message)"
    }
}
if ($setup) {
    Copy-Item $setup.FullName (Join-Path $rel 'SwagCod-setup.exe') -Force
    Write-Host "releases: $($setup.Name)" -ForegroundColor Green
}
if ($sig) {
    Copy-Item $sig.FullName (Join-Path $rel 'SwagCod-setup.exe.sig') -Force
}

# --- 6. latest.json --------------------------------------------------------
if ($Latest) {
    if (-not $setup) { throw 'no setup.exe - nothing to publish' }
    if (-not $sig) { throw 'no .sig - rebuild with -Sign' }
    $sigText = (Get-Content $sig.FullName -Raw).Trim()
    $noteText = if ($Notes) { $Notes } else { "SwagCod $ver" }
    $manifest = [ordered]@{
        version   = $ver
        notes     = $noteText
        pub_date  = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
        platforms = [ordered]@{
            'windows-x86_64' = [ordered]@{
                signature = $sigText
                url       = "https://github.com/Swwag666/SwagCode/releases/download/v$ver/SwagCod-setup.exe"
            }
        }
    }
    # UTF8 WITHOUT BOM: Set-Content -Encoding UTF8 in Windows PowerShell 5.1
    # writes a BOM, and the updater expects clean JSON (dsh-phone lesson).
    $json = $manifest | ConvertTo-Json -Depth 6
    [System.IO.File]::WriteAllText(
        (Join-Path $rel 'latest.json'),
        $json,
        (New-Object System.Text.UTF8Encoding($false))
    )
    Write-Host 'latest.json composed' -ForegroundColor Green
}

# --- 7. report -------------------------------------------------------------
Write-Host ''
Write-Host '== done ==' -ForegroundColor Green
Get-ChildItem $rel | Sort-Object LastWriteTime -Descending |
    Select-Object Name, @{n = 'MB'; e = { [math]::Round($_.Length / 1MB, 2) }}, LastWriteTime |
    Format-Table -AutoSize
if ($sig) { Write-Host "updater signature: SwagCod-setup.exe.sig" -ForegroundColor Green }
