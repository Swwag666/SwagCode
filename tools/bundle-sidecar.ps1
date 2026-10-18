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
{"id":8,"method":"exec","sql":"INSERT INTO memory(scope, text, created_ms, session_id, dir) VALUES (?1, ?2, ?3, ?4, ?5)","params":["folder","vendorprobe fact",1,null,"D:/probe"]}
{"id":9,"method":"query","sql":"SELECT text, dir FROM memory WHERE scope = ?1 AND dir = ?2","params":["folder","D:/probe"]}
{"id":10,"method":"shutdown"}' | & $node (Join-Path $vendor 'store-server.js') 2>$null
$ErrorActionPreference = 'Stop'
$okCount = @($ping | Where-Object { $_ -match '"ok":true' }).Count
if ($okCount -lt 10) { throw "vendor sidecar did not answer all requests: $ping" }
# F-10: the FTS5 index has to work in the BUNDLED node, not only in the dev
# one. Request 7 must come back with a count of 1: a vendor assembled against
# a different better-sqlite3 ABI (or a stale store-server.js without the FTS5
# schema) can still open the database and answer the prefs roundtrip, while
# search silently returns nothing on the primary backend.
$ftsHit = @($ping | Where-Object { $_ -match '"rows":\[\[1\]\]' }).Count
if ($ftsHit -lt 1) { throw "vendor sidecar FTS5 search did not find the probe row: $ping" }
# rev56: the folder-scope owner column has to survive the bundle too. Request 9
# reads back the row inserted by request 8, so a stale store-server.js (schema
# without memory.dir) fails HERE instead of on the first folder fact written by
# the shipped app. Same reasoning as the FTS probe above: opening the database
# and answering the prefs roundtrip proves nothing about a column added later.
$dirHit = @($ping | Where-Object { $_ -match 'vendorprobe fact' -and $_ -match 'D:/probe' }).Count
if ($dirHit -lt 1) { throw "vendor sidecar memory.dir roundtrip failed: $ping" }

# 4b. Acceptance: the PLUGIN sidecar must reach prefs through the bundled
# better-sqlite3. require() resolves from the script's own path, and CI
# installs JS deps with --ignore-scripts, so only vendor/sidecar carries the
# native binding: without this probe an incomplete bundle passes the store
# ping while api.prefs.get() silently returns null inside plugins (the exact
# failure of revision 49).
#
# Transport: a temp driver.js spawned with the bundled node, talking to both
# servers over child_process pipes. Piping JSON lines from PowerShell into a
# native exe is fine on a dev box but lost the FIRST request line on the
# Windows runner (the plugin server answered id 2 and 3 only), so the probe
# does not depend on PowerShell stdin plumbing at all and prints one JSON
# verdict with the load errors inside.
$probeDir = Join-Path $env:TEMP ("swagcod-plugin-probe-" + [guid]::NewGuid().ToString('N'))
$plugDir = Join-Path $probeDir 'plugins'
New-Item -ItemType Directory -Force -Path $plugDir | Out-Null
$probeJs = Join-Path $plugDir 'probe.js'
Set-Content -Path $probeJs -Encoding ASCII -Value @'
swagcod.define({
  name: 'probe',
  description: 'prefs bridge probe',
  parameters: { type: 'object' },
  handler: (args, api) => 'pref=' + api.prefs.get('vendor_probe'),
});
'@
$driverJs = Join-Path $probeDir 'driver.js'
Set-Content -Path $driverJs -Encoding ASCII -Value @'
'use strict';
/* Acceptance driver: seeds prefs.db through the bundled store-server.js, then
 * loads THIS directory as a plugin dir in the bundled plugin-server.js and
 * calls the probe. Verdict is one JSON line; exit 0 only when prefs really
 * came back through better-sqlite3. */
const { spawn } = require('child_process');
const path = require('path');

const node = process.execPath;
const here = __dirname;
/* argv[2] = vendor/sidecar of the bundle under test: the driver lives in a
 * temp dir, so it cannot derive the repo root itself. */
const vendor = process.argv[2];
const plug = path.join(here, 'plugins');
const db = path.join(here, 'prefs.db');
if (!vendor) throw new Error('usage: driver.js <vendor-sidecar-dir>');

function rpc(script, requests) {
  return new Promise((resolve, reject) => {
    const child = spawn(node, [script], { stdio: ['pipe', 'pipe', 'pipe'] });
    const out = [];
    const err = [];
    let buf = '';
    child.stdout.on('data', (d) => {
      buf += d.toString('utf8');
      let nl;
      while ((nl = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        try { out.push(JSON.parse(line)); } catch (_) { /* not protocol */ }
      }
    });
    child.stderr.on('data', (d) => err.push(d.toString('utf8')));
    child.on('error', reject);
    child.on('exit', () => resolve({ out, err: err.join('') }));
    for (const r of requests) child.stdin.write(JSON.stringify(r) + '\n');
    child.stdin.end();
  });
}

(async () => {
  const seed = await rpc(path.join(vendor, 'store-server.js'), [
    { id: 1, method: 'open', path: db },
    { id: 2, method: 'exec', sql: 'INSERT INTO prefs(key, value) VALUES (?1, ?2)', params: ['vendor_probe', '7'] },
    { id: 3, method: 'shutdown' },
  ]);
  const seedOk = seed.out.some((r) => r.id === 2 && r.ok === true && r.changes === 1);
  const run = await rpc(path.join(vendor, 'plugin-server.js'), [
    { id: 1, method: 'load', params: { dir: plug, db } },
    { id: 2, method: 'call', params: { name: 'probe', arguments: {} } },
    { id: 3, method: 'shutdown' },
  ]);
  const load = run.out.find((r) => r.id === 1) || null;
  const call = run.out.find((r) => r.id === 2) || null;
  const output = call && call.result ? String(call.result.output || '') : '';
  const verdict = {
    ok: Boolean(seedOk && output === 'pref=7'),
    seedOk,
    seedOut: seed.out.length,
    output,
    loadAnswered: load !== null,
    loadPlugins: load && load.result && load.result.plugins ? load.result.plugins.map((p) => p.name) : null,
    loadErrors: load && load.result ? load.result.errors : null,
    stderr: String(run.err).slice(0, 400),
    vendor,
  };
  process.stdout.write(JSON.stringify(verdict) + '\n');
  process.exit(verdict.ok ? 0 : 1);
})().catch((e) => {
  process.stdout.write(JSON.stringify({ ok: false, fatal: String((e && e.message) || e) }) + '\n');
  process.exit(1);
});
'@
$ErrorActionPreference = 'Continue'
$verdict = (& $node $driverJs $vendor 2>&1 | Out-String).Trim()
$probeExit = $LASTEXITCODE
$ErrorActionPreference = 'Stop'
Remove-Item -Recurse -Force $probeDir -ErrorAction SilentlyContinue
if ($probeExit -ne 0 -or $verdict -notmatch '"ok":true') {
  throw "vendor plugin-server.js did not read prefs through better-sqlite3: $verdict"
}
Write-Output "vendor plugin prefs bridge accepted: $verdict"

$size = (Get-ChildItem $vendor -Recurse | Measure-Object -Property Length -Sum).Sum
$mb = [math]::Round($size / 1MB, 1)
Write-Output "vendor/sidecar assembled and accepted by ping: $mb MB"
