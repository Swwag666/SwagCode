<#
.SYNOPSIS
  Measure SwagCod performance budget metrics (DECISIONS.md section 2).

.DESCRIPTION
  Measures exactly what Stage 0 must prove:
    - cold start to interactive window (target < 400 ms)
    - RSS at rest (target < 150 MB)
    - binary size
  The measurement is honest: process starts from scratch, the window is found
  via its handle, and memory is summed over the WHOLE process family - Tauri
  spawns WebView2 as child processes, so counting only the parent would
  understate real usage by a factor of several.

  NOTE: keep all output strings ASCII. Windows PowerShell 5.1 reads .ps1 as
  ANSI unless there is a BOM, which mangles non-ASCII text at parse time.

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File tools/bench-startup.ps1
#>

param(
    [string]$ExePath = "",
    [int]$Runs = 3,
    [int]$SettleSeconds = 8,
    [string]$OutFile = ""
)

$ErrorActionPreference = 'Stop'

if (-not $ExePath) {
    $candidates = @(
        "$PSScriptRoot\..\target\release\swagcod-app.exe",
        "$PSScriptRoot\..\target\debug\swagcod-app.exe"
    )
    $ExePath = $candidates | Where-Object { Test-Path $_ } | Select-Object -First 1
    if (-not $ExePath) {
        Write-Error "Binary not found. Build it: cargo build --release -p swagcod-app"
    }
}
$ExePath = (Resolve-Path $ExePath).Path
$exeInfo = Get-Item $ExePath

Write-Host "exe  : $ExePath"
Write-Host ("size : {0:N1} MB" -f ($exeInfo.Length / 1MB))
Write-Host ""

# Pre-flight 1: clear ORPHANED WebView2 processes left behind by a killed
# instance. build-release.ps1 stops SwagCod.exe with Stop-Process, and WebView2
# children can outlive their parent; whatever they hold is paid for by the next
# measurement without appearing in it.
# Selection is deliberately narrow. The command line must mention THIS app - the
# live profile is ...\xyz.swagcod.desktop\EBWebView and the browser process also
# carries --webview-exe-name=SwagCod.exe; 'swagsave' is a previous bundle
# identifier of this same app and was found once as a genuine orphan - AND the
# parent process must be dead. WebView2 is shared machinery (Windows Search runs
# its own msedgewebview2 tree under MicrosoftWindows.Client.CBS), so a live
# instance and every other application's WebView2 are never touched here.
$markers = @('swagcod', 'swagsave')
$orphans = @()
try {
    $orphans = @(Get-CimInstance Win32_Process -Filter "Name='msedgewebview2.exe'" -ErrorAction Stop |
        Where-Object { $cl = $_.CommandLine; @($markers | Where-Object { $cl -like "*$_*" }).Count -gt 0 } |
        Where-Object { -not (Get-Process -Id $_.ParentProcessId -ErrorAction SilentlyContinue) })
} catch {
    Write-Host ("note: WebView2 enumeration unavailable ({0})" -f $_.Exception.Message)
}
foreach ($o in $orphans) {
    try {
        Stop-Process -Id $o.ProcessId -Force -ErrorAction Stop
        Write-Host ("cleared orphaned WebView2 pid {0} (dead parent)" -f $o.ProcessId)
    } catch {
        Write-Host ("note: could not clear WebView2 pid {0}" -f $o.ProcessId)
    }
}
if ($orphans.Count -gt 0) { Start-Sleep -Seconds 2 }

# Pre-flight 2 (a note, not a guard): the FIRST launch after a fresh release
# build is not representative. The new binary is cold in the file cache and is
# scanned on first execution, so the WebView2 family comes up only partially and
# startup reads several times high. Measured in rev49, inside one and the same
# bench run: 1438 ms with 9 processes and 5.5 MB our private, then 183 ms, then
# 112 ms with 12 processes and 14.3 MB. So if run 1 is a wild outlier while runs
# 2-3 agree with each other, that is warm-up and not a regression - re-run and
# compare medians before believing an "OVER BUDGET" line.

# Pre-flight 3: refuse to pretend a measurement is meaningful while an instance
# is already up. tauri-plugin-single-instance makes a second launch hand over to
# the running one and exit, so startupMs would measure a handover and memory
# would be read off somebody else's window.
$live = @(Get-Process -Name 'SwagCod', 'swagcod-app' -ErrorAction SilentlyContinue)
if ($live.Count -gt 0) {
    $pids = ($live | ForEach-Object { $_.Id }) -join ','
    Write-Host ("WARNING: SwagCod is already running (pid {0})." -f $pids)
    Write-Host "         Single-instance handover makes these numbers meaningless."
    Write-Host "         Close it and re-run, or pass -ExePath to a different build."
}

$results = @()

for ($i = 1; $i -le $Runs; $i++) {
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $proc = Start-Process -FilePath $ExePath -PassThru
    $procId = $proc.Id

    $windowFound = $false
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 10
        try {
            $p = Get-Process -Id $procId -ErrorAction Stop
        } catch {
            Write-Host "  process $procId exited early" -ForegroundColor Red
            break
        }
        if ($p.MainWindowHandle -ne 0 -and $p.MainWindowTitle) {
            $sw.Stop()
            $windowFound = $true
            break
        }
    }
    if (-not $windowFound -and $sw.IsRunning) { $sw.Stop() }

    # Let the UI settle: WebView2 loads the bundle, the bus emits startup events.
    Start-Sleep -Seconds $SettleSeconds

    # Collect the whole family: children and grandchildren of our process.
    $family = @()
    $main = Get-Process -Id $procId -ErrorAction SilentlyContinue
    if ($main) { $family += $main }
    $children = Get-CimInstance Win32_Process -Filter "ParentProcessId=$procId" -ErrorAction SilentlyContinue
    foreach ($c in $children) {
        $cp = Get-Process -Id $c.ProcessId -ErrorAction SilentlyContinue
        if ($cp) { $family += $cp }
        $grand = Get-CimInstance Win32_Process -Filter "ParentProcessId=$($c.ProcessId)" -ErrorAction SilentlyContinue
        foreach ($g in $grand) {
            $gp = Get-Process -Id $g.ProcessId -ErrorAction SilentlyContinue
            if ($gp) { $family += $gp }
        }
    }
    $family = @($family | Where-Object { $_ } | Sort-Object Id -Unique)
    $rssBytes = ($family | Measure-Object WorkingSet64 -Sum).Sum
    $rssMb = [math]::Round($rssBytes / 1MB, 1)
    # Private bytes: the budget metric of DECISIONS.md section 2 (D-013).
    # RSS counts shared pages twice across the family, private does not.
    $ourPrivateMb = 0.0
    if ($main) { $ourPrivateMb = [math]::Round($main.PrivateMemorySize64 / 1MB, 1) }
    $famPrivateMb = [math]::Round((($family | Measure-Object PrivateMemorySize64 -Sum).Sum) / 1MB, 1)
    $procCount = $family.Count

    # Kill the whole tree or the next run measures someone else's memory.
    foreach ($p in ($family | Sort-Object Id -Descending)) {
        try { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } catch {}
    }
    Start-Sleep -Milliseconds 600

    $startupMs = $null
    if ($windowFound) { $startupMs = [math]::Round($sw.Elapsed.TotalMilliseconds, 1) }
    $results += [pscustomobject]@{
        run           = $i
        startupMs     = $startupMs
        rssMb         = $rssMb
        ourPrivateMb  = $ourPrivateMb
        famPrivateMb  = $famPrivateMb
        processes     = $procCount
    }

    $status = if ($windowFound) { 'ok' } else { 'WINDOW NOT FOUND' }
    Write-Host ("run {0}: startup {1} ms | RSS {2} MB | private our/fam {3}/{4} MB | procs {5} | {6}" -f `
        $i, $startupMs, $rssMb, $ourPrivateMb, $famPrivateMb, $procCount, $status)
}

$valid = @($results | Where-Object { $null -ne $_.startupMs })
if ($valid.Count -eq 0) {
    Write-Error "No run produced a valid measurement (window never appeared)."
}

function Get-Median([double[]]$values) {
    $s = @($values | Sort-Object)
    $n = $s.Count
    if ($n -eq 0) { return $null }
    if ($n % 2 -eq 1) { return $s[[int][math]::Floor($n / 2)] }
    return ($s[$n / 2 - 1] + $s[$n / 2]) / 2
}

$startups = @($valid | ForEach-Object { [double]$_.startupMs })
$rsss     = @($results | ForEach-Object { [double]$_.rssMb })
$ourPrivs = @($results | ForEach-Object { [double]$_.ourPrivateMb })
$famPrivs = @($results | ForEach-Object { [double]$_.famPrivateMb })

# Budgets come from CI env (see .github/workflows/ci.yml) with local defaults,
# so the same numbers gate CI comments and local runs.
$budgetStartupMs = if ($env:BUDGET_STARTUP_MS) { [int]$env:BUDGET_STARTUP_MS } else { 400 }
$budgetRssMb = 150
$budgetOurPrivateMb = if ($env:BUDGET_OUR_PRIVATE_MB) { [int]$env:BUDGET_OUR_PRIVATE_MB } else { 40 }
$budgetFamPrivateMb = if ($env:BUDGET_FAMILY_PRIVATE_MB) { [int]$env:BUDGET_FAMILY_PRIVATE_MB } else { 200 }

$summary = [pscustomobject]@{
    exe                  = (Split-Path $ExePath -Leaf)
    exeSizeMb            = [math]::Round($exeInfo.Length / 1MB, 2)
    runs                 = $Runs
    settleSeconds        = $SettleSeconds
    startupMsMedian      = [math]::Round((Get-Median $startups), 1)
    startupMsBest        = ($startups | Measure-Object -Minimum).Minimum
    startupMsWorst       = ($startups | Measure-Object -Maximum).Maximum
    rssMbMedian          = [math]::Round((Get-Median $rsss), 1)
    rssMbMax             = ($rsss | Measure-Object -Maximum).Maximum
    ourPrivateMbMedian   = [math]::Round((Get-Median $ourPrivs), 1)
    famPrivateMbMedian   = [math]::Round((Get-Median $famPrivs), 1)
    processesMax         = ($results | Measure-Object processes -Maximum).Maximum
    budgetStartupMs      = $budgetStartupMs
    budgetRssMb          = $budgetRssMb
    budgetOurPrivateMb   = $budgetOurPrivateMb
    budgetFamPrivateMb   = $budgetFamPrivateMb
    startupInBudget      = ($null -ne (Get-Median $startups)) -and ((Get-Median $startups) -lt $budgetStartupMs)
    rssInBudget          = ((Get-Median $rsss) -lt $budgetRssMb)
    ourPrivateInBudget   = ((Get-Median $ourPrivs) -lt $budgetOurPrivateMb)
    famPrivateInBudget   = ((Get-Median $famPrivs) -lt $budgetFamPrivateMb)
}

Write-Host ""
Write-Host "--- summary ---" -ForegroundColor Cyan
$summary | Format-List | Out-String | Write-Host

$startupOk = [bool]$summary.startupInBudget
$rssOk = [bool]$summary.rssInBudget
$fmt = if ($startupOk) { 'Green' } else { 'Red' }
Write-Host ("cold start < {0} ms : {1}" -f $budgetStartupMs, $(if ($startupOk) { 'IN BUDGET' } else { 'OVER BUDGET' })) -ForegroundColor $fmt
$fmt2 = if ($rssOk) { 'Green' } else { 'Yellow' }
Write-Host ("RSS at rest < {0} MB : {1} (informational, D-013)" -f $budgetRssMb, $(if ($rssOk) { 'IN BUDGET' } else { 'OVER BUDGET' })) -ForegroundColor $fmt2
$ourPrivOk = [bool]$summary.ourPrivateInBudget
$fmt3 = if ($ourPrivOk) { 'Green' } else { 'Red' }
Write-Host ("our private < {0} MB : {1}" -f $budgetOurPrivateMb, $(if ($ourPrivOk) { 'IN BUDGET' } else { 'OVER BUDGET' })) -ForegroundColor $fmt3
$famPrivOk = [bool]$summary.famPrivateInBudget
$fmt4 = if ($famPrivOk) { 'Green' } else { 'Red' }
Write-Host ("family private < {0} MB : {1}" -f $budgetFamPrivateMb, $(if ($famPrivOk) { 'IN BUDGET' } else { 'OVER BUDGET' })) -ForegroundColor $fmt4

if ($OutFile) {
    $payload = [pscustomobject]@{
        timestamp = (Get-Date).ToString('o')
        machine   = (Get-CimInstance Win32_Processor).Name
        ramGb     = [math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory / 1GB, 1)
        summary   = $summary
        runs      = $results
    }
    $payload | ConvertTo-Json -Depth 6 | Set-Content -Path $OutFile -Encoding UTF8
    Write-Host ""
    Write-Host "written: $OutFile"
}
