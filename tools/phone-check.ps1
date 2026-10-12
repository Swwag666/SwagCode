<#
.SYNOPSIS
  Manual check of the SwagCod loopback HTTP API - the routes SwagCod Phone uses (F-9).

.DESCRIPTION
  Walks the API route by route over raw sockets and compares status codes and
  bodies with what the phone gateway expects. No dependencies beyond PowerShell
  and .NET sockets. The app must be running and the API enabled (prefs key
  http_api_port, or env SWAGCOD_HTTP_PORT).

  ASCII only on purpose: every script in tools/ is ASCII, because Windows
  PowerShell 5.1 decodes a BOM-less UTF-8 file as ANSI and Cyrillic strings
  then break the parser.

  Checks:
     1. GET  /health                     liveness, no token
     2. GET  /v1/diagnostics             401 without token, 401 with a wrong one
     3. GET  /v1/diagnostics             200 with the token
     4. GET  /v1/sessions                200
     5. GET  /v1/events                  SSE stream: headers + a data frame
     6. POST /v1/sessions                create a session in a temp dir, 200
     7. POST /v1/sessions                relative cwd rejected, 400
     8. POST /v1/sessions/transcript     200 and an array
     9. POST /v1/sessions/transcript     unknown session, 400
    10. POST /v1/checkpoints             200 and an array
    11. POST /v1/turns                   blank message, 400
    12. POST /v1/turns/stop              no live turn, 400
    13. POST /v1/approvals               unknown call_id, 400
    14. GET  /v1/nope                    404
  With -Turn it additionally starts a real turn and waits for turn_ended in
  the SSE stream (spends provider tokens, hence opt-in).

.PARAMETER Token
  HTTP API token: Settings -> Security -> "Copy token".

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File tools\phone-check.ps1 -Token <hex>

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File tools\phone-check.ps1 -Token <hex> -Turn
#>
[CmdletBinding()]
param(
    [string]$Token = "",
    [int]$Port = 47479,
    [switch]$Turn,
    [int]$SseWaitMs = 10000
)

$ErrorActionPreference = "Stop"
$script:results = @()

function Add-Check {
    param([string]$Name, [string]$Expect, [string]$Actual, [bool]$Pass)
    $script:results += [pscustomobject]@{
        Check  = $Name
        Expect = $Expect
        Actual = $Actual
        Result = $(if ($Pass) { "OK" } else { "FAIL" })
    }
    $mark = if ($Pass) { "ok  " } else { "FAIL" }
    Write-Host ("[{0}] {1} | expect {2} | got {3}" -f $mark, $Name, $Expect, $Actual)
}

# HTTP/1.1 over TcpClient. Invoke-WebRequest on Windows PowerShell 5.1 throws
# on 4xx and hides the error body, and honest status codes are the point here.
# The API answers connection: close, so "read to end" is the right strategy.
function Send-Api {
    param(
        [string]$Method,
        [string]$Path,
        [string]$Body = $null,
        [string]$UseToken = $Token,
        [int]$TimeoutMs = 20000
    )
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $client.Connect("127.0.0.1", $Port)
        $stream = $client.GetStream()
        $stream.ReadTimeout = $TimeoutMs
        $head = "$Method $Path HTTP/1.1`r`nhost: 127.0.0.1`r`nconnection: close`r`n"
        if ($UseToken) { $head += "authorization: Bearer $UseToken`r`n" }
        $payload = $null
        if ($null -ne $Body) {
            $payload = [System.Text.Encoding]::UTF8.GetBytes($Body)
            $head += "content-type: application/json`r`ncontent-length: $($payload.Length)`r`n"
        }
        $head += "`r`n"
        $headBytes = [System.Text.Encoding]::UTF8.GetBytes($head)
        $stream.Write($headBytes, 0, $headBytes.Length)
        if ($payload) { $stream.Write($payload, 0, $payload.Length) }
        $stream.Flush()

        $ms = New-Object System.IO.MemoryStream
        $buf = New-Object byte[] 65536
        while ($true) {
            $n = 0
            try { $n = $stream.Read($buf, 0, $buf.Length) } catch { break }
            if ($n -le 0) { break }
            $ms.Write($buf, 0, $n)
        }
        $raw = [System.Text.Encoding]::UTF8.GetString($ms.ToArray())
        $sep = $raw.IndexOf("`r`n`r`n")
        $headPart = if ($sep -ge 0) { $raw.Substring(0, $sep) } else { $raw }
        $bodyPart = if ($sep -ge 0) { $raw.Substring($sep + 4) } else { "" }
        $status = 0
        $first = ($headPart -split "`r`n")[0]
        $parts = $first -split " "
        if ($parts.Count -ge 2) { [void][int]::TryParse($parts[1], [ref]$status) }
        return [pscustomobject]@{ Status = $status; Body = $bodyPart; Raw = $raw }
    }
    finally {
        $client.Close()
    }
}

# SSE lives on its own connection that never closes by itself: subscribe, let
# the server reach its subscribe call, fire the triggering request on a second
# connection, then read frames until the needle shows up or the wait expires.
function Read-Sse {
    param([scriptblock]$Trigger, [int]$WaitMs = 10000, [string]$Needle = "data: ")
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $client.Connect("127.0.0.1", $Port)
        $stream = $client.GetStream()
        $req = "GET /v1/events HTTP/1.1`r`nhost: 127.0.0.1`r`nauthorization: Bearer $Token`r`nconnection: close`r`n`r`n"
        $reqBytes = [System.Text.Encoding]::UTF8.GetBytes($req)
        $stream.Write($reqBytes, 0, $reqBytes.Length)
        $stream.Flush()
        Start-Sleep -Milliseconds 500
        if ($Trigger) { & $Trigger | Out-Null }

        $ms = New-Object System.IO.MemoryStream
        $buf = New-Object byte[] 65536
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        $text = ""
        while ($sw.ElapsedMilliseconds -lt $WaitMs) {
            if ($stream.DataAvailable) {
                $n = $stream.Read($buf, 0, $buf.Length)
                if ($n -le 0) { break }
                $ms.Write($buf, 0, $n)
            }
            else {
                Start-Sleep -Milliseconds 100
            }
            $text = [System.Text.Encoding]::UTF8.GetString($ms.ToArray())
            if ($text.Contains($Needle)) { break }
        }
        return [System.Text.Encoding]::UTF8.GetString($ms.ToArray())
    }
    finally {
        $client.Close()
    }
}

function ConvertTo-Body {
    param([hashtable]$Map)
    return ($Map | ConvertTo-Json -Compress)
}

# ── checks ─────────────────────────────────────────────────────────────────
Write-Host "== SwagCod Phone check: 127.0.0.1:$Port =="
if (-not $Token) {
    Write-Host "Need -Token: Settings -> Security -> Copy token."
    Write-Host "The API must be enabled (prefs http_api_port=$Port) and the app running."
    exit 2
}

# 1. liveness without a token
$r = Send-Api -Method GET -Path "/health" -UseToken ""
Add-Check "GET /health" "200 ok:true" "$($r.Status)" ($r.Status -eq 200 -and $r.Body -match '"ok":true')

# 2-3. authentication
$r = Send-Api -Method GET -Path "/v1/diagnostics" -UseToken ""
Add-Check "GET /v1/diagnostics (no token)" "401" "$($r.Status)" ($r.Status -eq 401)
$r = Send-Api -Method GET -Path "/v1/diagnostics" -UseToken "wrong-token"
Add-Check "GET /v1/diagnostics (wrong token)" "401" "$($r.Status)" ($r.Status -eq 401)
$r = Send-Api -Method GET -Path "/v1/diagnostics"
Add-Check "GET /v1/diagnostics" "200 with version" "$($r.Status)" ($r.Status -eq 200 -and $r.Body -match '"version"')

# 4. session list
$r = Send-Api -Method GET -Path "/v1/sessions"
Add-Check "GET /v1/sessions" "200" "$($r.Status)" ($r.Status -eq 200)

# 5-6. SSE stream, triggered by creating a session (that publishes Status)
$dir = Join-Path ([System.IO.Path]::GetTempPath()) ("swagcod-phone-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
New-Item -ItemType Directory -Path $dir -Force | Out-Null
$createBody = ConvertTo-Body @{ cwd = $dir; model = "phone-check" }
$sse = Read-Sse -Trigger { Send-Api -Method POST -Path "/v1/sessions" -Body $createBody } -WaitMs $SseWaitMs -Needle "data: "
$hasStream = $sse -match "text/event-stream"
$hasFrame = $sse -match "data: "
Add-Check "GET /v1/events (SSE)" "event-stream + data frame" "stream=$hasStream frame=$hasFrame" ($hasStream -and $hasFrame)

# the created session must show up in the list
$r = Send-Api -Method GET -Path "/v1/sessions"
$sid = ""
try {
    $list = $r.Body | ConvertFrom-Json
    $mine = @($list | Where-Object { $_.cwd -eq $dir })
    if ($mine.Count -gt 0) { $sid = [string]$mine[0].id }
}
catch { }
Add-Check "POST /v1/sessions" "session present in list" $(if ($sid) { $sid } else { "not found" }) ($sid.Length -gt 0)

# 7. relative cwd is rejected
$r = Send-Api -Method POST -Path "/v1/sessions" -Body '{"cwd":"rel/path"}'
Add-Check "POST /v1/sessions (relative cwd)" "400" "$($r.Status)" ($r.Status -eq 400)

# 8. transcript of the new session
$body = ConvertTo-Body @{ session_id = $sid }
$r = Send-Api -Method POST -Path "/v1/sessions/transcript" -Body $body
Add-Check "POST /v1/sessions/transcript" "200 + array" "$($r.Status)" ($r.Status -eq 200 -and $r.Body.TrimStart().StartsWith("["))

# 9. unknown session
$r = Send-Api -Method POST -Path "/v1/sessions/transcript" -Body '{"session_id":"no-such-session"}'
Add-Check "POST /v1/sessions/transcript (unknown)" "400" "$($r.Status)" ($r.Status -eq 400)

# 10. checkpoints
$r = Send-Api -Method POST -Path "/v1/checkpoints" -Body $body
Add-Check "POST /v1/checkpoints" "200 + array" "$($r.Status)" ($r.Status -eq 200 -and $r.Body.TrimStart().StartsWith("["))

# 11. blank message
$r = Send-Api -Method POST -Path "/v1/turns" -Body (ConvertTo-Body @{ session_id = $sid; message = "   " })
Add-Check "POST /v1/turns (blank message)" "400" "$($r.Status)" ($r.Status -eq 400)

# 12. stop with no live turn
$r = Send-Api -Method POST -Path "/v1/turns/stop" -Body $body
Add-Check "POST /v1/turns/stop (no live turn)" "400" "$($r.Status)" ($r.Status -eq 400)

# 13. approval for an unknown call
$r = Send-Api -Method POST -Path "/v1/approvals" -Body '{"call_id":"no-such-call","decision":"approved"}'
Add-Check "POST /v1/approvals (unknown call_id)" "400" "$($r.Status)" ($r.Status -eq 400)

# 14. unknown route
$r = Send-Api -Method GET -Path "/v1/nope"
Add-Check "GET /v1/nope" "404" "$($r.Status)" ($r.Status -eq 404)

# 15. a real turn, opt-in: it spends provider tokens
if ($Turn -and $sid) {
    Write-Host "-- starting a real turn in $sid, waiting for turn_ended in SSE --"
    $turnBody = ConvertTo-Body @{ session_id = $sid; message = "Reply with one word: check. Do not call tools." }
    $sse = Read-Sse -Trigger { Send-Api -Method POST -Path "/v1/turns" -Body $turnBody } -WaitMs 180000 -Needle "turn_ended"
    $ended = $sse -match "turn_ended"
    Add-Check "POST /v1/turns + SSE turn_ended" "turn_ended in stream" $(if ($ended) { "turn_ended" } else { "no event" }) $ended
}

# ── summary ────────────────────────────────────────────────────────────────
Write-Host ""
$script:results | Format-Table -AutoSize | Out-String -Width 200 | Write-Host
$failed = @($script:results | Where-Object { $_.Result -ne "OK" })
Write-Host ("checks: {0}, failed: {1}" -f $script:results.Count, $failed.Count)
Write-Host "check workspace: $dir (empty, safe to delete)"
Write-Host "note: the API has no DELETE route on purpose, so the session row stays in the list."

if ($failed.Count -gt 0) { exit 1 }
exit 0
