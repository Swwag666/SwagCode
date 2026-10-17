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
  $rawTarget = (Get-Item $link).Target | Select-Object -First 1
  if ($rawTarget) {
    # pnpm symlinks store a RELATIVE target (e.g. ..\..\node_modules\.pnpm\...),
    # relative to the link's own directory - not to the caller's CWD.
    # Using it raw points robocopy at a nonexistent source -> exit 16.
    if (-not [System.IO.Path]::IsPathRooted($rawTarget)) {
      $rawTarget = [System.IO.Path]::GetFullPath((Join-Path (Split-Path $link -Parent) $rawTarget))
    }
    $link = $rawTarget
  }
  if (-not (Test-Path $link)) { throw "package $dep resolved to missing path: $link" }
  $destDep = Join-Path $nm $dep
  if (Test-Path $destDep) { Remove-Item -Recurse -Force $destDep }
  robocopy $link $destDep /E /XD node_modules /NFL /NDL /NJH /NJS | Out-Null
  if ($LASTEXITCODE -ge 8) { throw "robocopy ${dep}: exit $LASTEXITCODE (src=$link dst=$destDep)" }
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
{"id":5,"method":"exec","sql":"INSERT INTO sessions(id, cwd, model, title, created_ms) VALUES (?1, ?2, ?3, ?4, ?5)","params":["s-vendor","D:/probe","m","vendor",1]}
{"id":6,"method":"exec","sql":"INSERT INTO messages(session_id, ord, role, content) VALUES (?1, ?2, ?3, ?4)","params":["s-vendor",0,"user","vendorprobe token"]}
{"id":7,"method":"query","sql":"SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?1","params":["\"vendorprobe\"*"]}
{"id":8,"method":"shutdown"}' | & $node (Join-Path $vendor 'store-server.js') 2>$null
$ErrorActionPreference = 'Stop'
$okCount = @($ping | Where-Object { $_ -match '"ok":true' }).Count
if ($okCount -lt 8) { throw "vendor sidecar did not answer all requests: $ping" }
# F-10: the FTS5 index has to work in the BUNDLED node, not only in the dev
# one. Request 7 must come back with a count of 1: a vendor assembled against
# a different better-sqlite3 ABI (or a stale store-server.js without the FTS5
# schema) can still open the database and answer the prefs roundtrip, while
# search silently returns nothing on the primary backend.
$ftsHit = @($ping | Where-Object { $_ -match '"rows":\[\[1\]\]' }).Count
if ($ftsHit -lt 1) { throw "vendor sidecar FTS5 search did not find the probe row: $ping" }

# 4b. Acceptance: the PLUGIN sidecar must reach prefs through the bundled
# better-sqlite3. require() resolves from the script's own path, and CI
# installs JS deps with --ignore-scripts, so only vendor/sidecar carries the
# native binding: without this probe a stale or incomplete bundle passes the
# store ping while api.prefs.get() silently returns null inside plugins
# (the exact failure of revision 49).
$probeDir = Join-Path $env:TEMP ("swagcod-plugin-probe-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $probeDir | Out-Null
$probeDb = Join-Path $probeDir 'prefs.db'
$probeJs = Join-Path $probeDir 'probe.js'
Set-Content -Path $probeJs -Encoding ASCII -Value @'
swagcod.define({
  name: 'probe',
  description: 'prefs bridge probe',
  parameters: { type: 'object' },
  handler: (args, api) => 'pref=' + api.prefs.get('vendor_probe'),
});
'@
$ErrorActionPreference = 'Continue'
$mkdb = '{"id":1,"method":"open","path":' + ($probeDb -replace '\\', '/' | ConvertTo-Json) + '}
{"id":2,"method":"exec","sql":"INSERT INTO prefs(key, value) VALUES (?1, ?2)","params":["vendor_probe","7"]}
{"id":3,"method":"shutdown"}' | & $node (Join-Path $vendor 'store-server.js') 2>$null
$plugin = '{"id":1,"method":"load","params":{"dir":' + ($probeDir -replace '\\', '/' | ConvertTo-Json) + ',"db":' + ($probeDb -replace '\\', '/' | ConvertTo-Json) + '}}
{"id":2,"method":"call","params":{"name":"probe","arguments":{}}}
{"id":3,"method":"shutdown"}' | & $node (Join-Path $vendor 'plugin-server.js') 2>$null
$ErrorActionPreference = 'Stop'
Remove-Item -Recurse -Force $probeDir -ErrorAction SilentlyContinue
if (@($plugin | Where-Object { $_ -match '"output":"pref=7"' }).Count -lt 1) {
  throw "vendor plugin-server.js did not read prefs through better-sqlite3: $plugin (seed: $mkdb)"
}

$size = (Get-ChildItem $vendor -Recurse | Measure-Object -Property Length -Sum).Sum
$mb = [math]::Round($size / 1MB, 1)
Write-Output "vendor/sidecar assembled and accepted by ping: $mb MB"
