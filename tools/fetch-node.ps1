<# E-1: download pinned node.exe into vendor/node.

Node is pinned on purpose: the better-sqlite3 native binding ABI
(node-v137) must match the runtime major version. The same version is
recorded in .nvmrc. Idempotent: does nothing when the file already
exists (-Force re-downloads).

ASCII-only on purpose: Windows PowerShell 5.1 reads BOM-less UTF-8 as
ANSI and chokes on Cyrillic strings (lesson of revision 27).
#>
param(
  [string]$Version = '24.19.0',
  [switch]$Force
)

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$dest = Join-Path $root 'vendor\node\node.exe'

if ((Test-Path $dest) -and -not $Force) {
  $v = & $dest --version
  Write-Output "node.exe already in place: $v ($dest)"
  exit 0
}

New-Item -ItemType Directory -Force -Path (Join-Path $root 'vendor\node') | Out-Null
$zip = Join-Path $env:TEMP "node-v$Version-win-x64.zip"
if (-not (Test-Path $zip) -or $Force) {
  $url = "https://nodejs.org/dist/v$Version/node-v$Version-win-x64.zip"
  Write-Output "downloading $url"
  curl.exe -L --silent --show-error -o $zip $url
  if ($LASTEXITCODE -ne 0) { throw "node zip download failed: exit $LASTEXITCODE" }
}

# Only one file is needed from the archive: node.exe.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$archive = [System.IO.Compression.ZipFile]::OpenRead($zip)
try {
  $entry = $archive.Entries | Where-Object { $_.FullName -like "node-v*/node.exe" } | Select-Object -First 1
  if (-not $entry) { throw 'node.exe not found in archive' }
  [System.IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $dest, $true)
} finally {
  $archive.Dispose()
}

$v = & $dest --version
$mb = [math]::Round((Get-Item $dest).Length / 1MB, 1)
Write-Output "node.exe installed: $v ($dest, $mb MB)"
