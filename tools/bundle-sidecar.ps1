<# E-1: assemble standalone vendor/sidecar.

Flattens pnpm symlinks into a plain node_modules (better-sqlite3 plus its
runtime deps), copies store-server.js, and verifies the result by pinging
it with the very vendor/node/node.exe that ships in the bundle. When the
native binding is missing (fresh machine, install script never ran), the
pinned prebuilt binary is downloaded for the pinned ABI.

better-sqlite3@12 runtime deps are fixed: bindings -> file-uri-to-path.
The DEPS list below is the only place to touch on upgrade.

ASCII-only on purpose: Windows PowerShell 5.1 reads BOM-less UTF-8 as
ANSI and chokes on Cyrillic strings (lesson of revision 27).
#>
param(
  [string]$BetterSqliteVersion = '12.11.1',
  [string]$Abi = 'node-v137-win32-x64'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$vendor = Join-Path $root 'vendor\sidecar'
$deps = @('better-sqlite3', 'bindings', 'file-uri-to-path')

# 1. Server scripts.
New-Item -ItemType Directory -Force -Path $vendor | Out-Null
Copy-Item (Join-Path $root 'sidecar\store-server.js') (Join-Path $vendor 'store-server.js') -Force
# E-4: JS plugins sidecar shares the bundle (same node, same node_modules).
Copy-Item (Join-Path $root 'sidecar\plugin-server.js') (Join-Path $vendor 'plugin-server.js') -Force

# 2. Flat node_modules from the pnpm store: copy real package directories,
#    resolving symlinks. /XD node_modules cuts nested pnpm links.
$nm = Join-Path $vendor 'node_modules'
New-Item -ItemType Directory -Force -Path $nm | Out-Null
foreach ($dep in $deps) {
  $link = Join-Path $root "sidecar\node_modules\$dep"
  if (-not (Test-Path $link)) { $link = Join-Path $root "node_modules\$dep" }
  if (-not (Test-Path $link)) {
    # Transitive deps (bindings, file-uri-to-path) are not hoisted in the
    # strict pnpm layout - find them in the .pnpm store instead.
    $stored = Get-ChildItem (Join-Path $root 'node_modules\.pnpm') -Directory -Filter "$dep@*" -ErrorAction SilentlyContinue |
      Select-Object -First 1
    if ($stored) { $link = Join-Path $stored.FullName "node_modules\$dep" }
  }
  if (-not (Test-Path $link)) { throw "package $dep not found - run pnpm install first" }
  $target = (Get-Item $link).Target
  if ($target) { $link = $target | Select-Object -First 1 }
  $destDep = Join-Path $nm $dep
  if (Test-Path $destDep) { Remove-Item -Recurse -Force $destDep }
  robocopy $link $destDep /E /XD node_modules /NFL /NDL /NJH /NJS | Out-Null
  if ($LASTEXITCODE -ge 8) { throw "robocopy ${dep}: exit $LASTEXITCODE" }
}

# 3. Native binding: prebuilt download when install left none.
$binding = Join-Path $nm 'better-sqlite3\build\Release\better_sqlite3.node'
if (-not (Test-Path $binding)) {
  New-Item -ItemType Directory -Force -Path (Split-Path $binding -Parent) | Out-Null
  $url = "https://github.com/WiseLibs/better-sqlite3/releases/download/v$BetterSqliteVersion/better-sqlite3-v$BetterSqliteVersion-$Abi.tar.gz"
  $tgz = Join-Path $env:TEMP 'bs3-prebuilt.tar.gz'
  Write-Output "downloading prebuilt: $url"
  curl.exe -L --silent --show-error -o $tgz $url
  if ($LASTEXITCODE -ne 0) { throw "prebuilt download failed: exit $LASTEXITCODE" }
  tar -xzf $tgz -C (Join-Path $nm 'better-sqlite3')
  if (-not (Test-Path $binding)) { throw "prebuilt extracted without $binding" }
}

# 4. Acceptance: ping the server with the bundled node.exe itself.
# NativeCommandError guard: with EAP=Stop Windows PowerShell 5.1 turns
# the sidecar's stderr banner into a terminating error, so relax EAP
# around the native call only.
$node = Join-Path $root 'vendor\node\node.exe'
if (-not (Test-Path $node)) { throw 'vendor\node\node.exe missing - run tools\fetch-node.ps1 first' }
$ErrorActionPreference = 'Continue'
$ping = '{"id":1,"method":"ping"}
{"id":2,"method":"open","path":":memory:"}
{"id":3,"method":"exec","sql":"INSERT INTO prefs(key, value) VALUES (?1, ?2)","params":["k","v"]}
{"id":4,"method":"query","sql":"SELECT value FROM prefs WHERE key = ?1","params":["k"]}
{"id":5,"method":"shutdown"}' | & $node (Join-Path $vendor 'store-server.js') 2>$null
$ErrorActionPreference = 'Stop'
$okCount = @($ping | Where-Object { $_ -match '"ok":true' }).Count
if ($okCount -lt 5) { throw "vendor sidecar did not answer all requests: $ping" }

$size = (Get-ChildItem $vendor -Recurse | Measure-Object -Property Length -Sum).Sum
$mb = [math]::Round($size / 1MB, 1)
Write-Output "vendor/sidecar assembled and accepted by ping: $mb MB"
