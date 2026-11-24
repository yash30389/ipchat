$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
New-Item -ItemType Directory -Force -Path 'test-results' | Out-Null
$raw = Join-Path $root 'test-results\go-test.jsonl'
$log = Join-Path $root 'test-results\go-test.log'

Write-Host 'Resolving pinned Go dependencies...'
& go mod download
if ($LASTEXITCODE -ne 0) {
  Set-Content -Path $log -Value 'go mod download failed. Check network access and dependency proxy settings.'
  throw 'Dependency download failed; tests were not executed.'
}
Write-Host 'Running Go automated tests...'
& go test -json ./... 2>&1 | Tee-Object -FilePath $raw | Out-Host
$exitCode = $LASTEXITCODE
$events = @()
Get-Content $raw | ForEach-Object {
  try { $events += ($_ | ConvertFrom-Json) } catch { }
}
$cases = @($events | Where-Object { $_.Action -in @('pass','fail','skip') -and $_.Test } | ForEach-Object {
  [pscustomobject]@{ Package=$_.Package; Test=$_.Test; Status=$_.Action.ToUpperInvariant(); Output=$_.Output }
})
$passed = @($cases | Where-Object Status -eq 'PASS').Count
$failed = @($cases | Where-Object Status -eq 'FAIL').Count
$skipped = @($cases | Where-Object Status -eq 'SKIP').Count
$rows = foreach ($case in $cases) {
  $cls = switch ($case.Status) { 'PASS' {'pass'} 'FAIL' {'fail'} default {'skip'} }
  '<tr><td>' + [System.Net.WebUtility]::HtmlEncode($case.Package) + '</td><td>' + [System.Net.WebUtility]::HtmlEncode($case.Test) + '</td><td class="' + $cls + '">' + $case.Status + '</td></tr>'
}
$stamp = (Get-Date).ToString('yyyy-MM-dd HH:mm:ss zzz')
$html = @"
<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>IPChat Test Results</title>
<style>body{font:15px system-ui;background:#f5f7fb;color:#182235;margin:0;padding:32px}.wrap{max-width:1100px;margin:auto}.card{background:white;border:1px solid #e1e7f0;border-radius:14px;padding:20px;margin:16px 0}.stats{display:flex;gap:12px;flex-wrap:wrap}.stat{padding:14px 20px;background:#f0f4fa;border-radius:10px}.pass{color:#087443;font-weight:700}.fail{color:#b42318;font-weight:700}.skip{color:#9a6700;font-weight:700}table{width:100%;border-collapse:collapse}td,th{text-align:left;padding:11px;border-bottom:1px solid #e7ebf2}th{background:#f7f9fc}small{color:#65738a}</style></head><body><div class="wrap"><h1>IPChat · Automated Test Results</h1><small>Generated: $stamp</small><div class="card"><div class="stats"><div class="stat">Passed <b>$passed</b></div><div class="stat">Failed <b>$failed</b></div><div class="stat">Skipped <b>$skipped</b></div><div class="stat">Exit code <b>$exitCode</b></div></div></div><div class="card"><h2>Executed test cases</h2><table><thead><tr><th>Package</th><th>Test</th><th>Status</th></tr></thead><tbody>$($rows -join "`n")</tbody></table></div><div class="card"><h2>Important coverage limits</h2><p>These Go tests are unit tests. PostgreSQL, Redis, browser, WebSocket multi-instance, encryption, file upload, deployment, and load tests require integration environments and are not considered passed unless separately executed and reported.</p></div></div></body></html>
"@
Set-Content -Path (Join-Path $root 'test-results\index.html') -Value $html -Encoding UTF8
Write-Host "Report: $root\test-results\index.html"
if ($exitCode -ne 0) { exit $exitCode }
