<#
.SYNOPSIS
    Runs the whole test suite the right way: starts PostgreSQL + Redis in
    Docker, sets the test environment variables, runs go vet and go test,
    and (optionally) produces a merged coverage report.

.EXAMPLE
    .\scripts\test.ps1                      # all tests, once
    .\scripts\test.ps1 -Race                # with Go's race detector
    .\scripts\test.ps1 -Coverage            # + coverage per package, coverage.html
    .\scripts\test.ps1 -Run Concurrency -Count 20 -Race   # repeat the concurrency tests
#>
param(
    [switch]$Race,        # enable -race (slower, catches data races)
    [switch]$Coverage,    # merged cross-package coverage + coverage.html
    [int]$Count = 1,      # run each test N times (-count)
    [string]$Run = "",    # only tests matching this regex (-run)
    [switch]$SkipDocker   # do not start containers (they are already running)
)

# Not "Stop": Windows PowerShell 5.1 treats ANY stderr output of a native
# program (docker's progress messages, go's build output) as a terminating
# error under "Stop". We check $LASTEXITCODE explicitly instead.
$ErrorActionPreference = "Continue"
Set-Location (Split-Path $PSScriptRoot -Parent)   # project root

function Step($msg) { Write-Host "`n==> $msg" -ForegroundColor Cyan }

# 1. Dependencies -------------------------------------------------------------
if (-not $SkipDocker) {
    Step "Starting PostgreSQL and Redis (docker compose)"
    docker compose up -d postgres redis 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "docker compose up failed - is Docker Desktop running?" }
    foreach ($svc in "postgres", "redis") {
        for ($i = 0; $i -lt 30; $i++) {
            $health = docker inspect --format "{{.State.Health.Status}}" (docker compose ps -q $svc)
            if ($health -eq "healthy") { break }
            Start-Sleep 1
        }
        if ($health -ne "healthy") { throw "$svc did not become healthy" }
        Write-Host "    $svc is healthy"
    }
}

# 2. Environment --------------------------------------------------------------
# Without these, integration tests SKIP (and the run looks green but proves
# much less). Step 4 counts skips so that can never go unnoticed.
$env:TEST_DATABASE_URL = "postgres://app:app_dev_password@localhost:5432/inventory_test?sslmode=disable"
$env:TEST_REDIS_URL    = "redis://localhost:6379/15"

# 3. Static checks ------------------------------------------------------------
Step "gofmt + go vet"
$unformatted = gofmt -l .
if ($unformatted) { throw "Files not gofmt-formatted:`n$unformatted" }
go vet ./...
if ($LASTEXITCODE -ne 0) { throw "go vet failed" }

# 4. Tests --------------------------------------------------------------------
# NOTE: arguments containing '=' and '.' MUST be quoted in PowerShell.
# Unquoted, -coverpkg=./internal/... is split into two arguments and
# cross-package coverage silently disappears (this happened in Stage 13).
$testArgs = @("test", "-count=$Count", "-v")
if ($Race) { $testArgs += "-race" }
if ($Run)  { $testArgs += "-run=$Run" }

$coverDir = Join-Path $env:TEMP "inventory-covdata"
if ($Coverage) {
    Remove-Item -Recurse -Force $coverDir -ErrorAction SilentlyContinue
    New-Item -ItemType Directory $coverDir | Out-Null
    $testArgs += @("-cover", "-coverpkg=./internal/...")
}
$testArgs += "./..."
if ($Coverage) { $testArgs += @("-args", "-test.gocoverdir=$coverDir") }

Step ("go " + ($testArgs -join " "))
$log = Join-Path $env:TEMP "inventory-test.log"
& go @testArgs 2>&1 | Tee-Object -FilePath $log | Where-Object { $_ -match '^(ok|FAIL|---\s+FAIL|panic)' } | ForEach-Object { Write-Host "    $_" }
$exit = $LASTEXITCODE

$lines   = Get-Content $log
$passed  = ($lines | Select-String -Pattern '^\s*--- PASS').Count
$failed  = ($lines | Select-String -Pattern '^\s*--- FAIL').Count
$skipped = ($lines | Select-String -Pattern '^\s*--- SKIP').Count

Step "Summary"
Write-Host ("    passed: {0}   failed: {1}   skipped: {2}" -f $passed, $failed, $skipped)
if ($skipped -gt 0) {
    Write-Host "    WARNING: skipped tests prove nothing. Which ones:" -ForegroundColor Yellow
    $lines | Select-String -Pattern '^\s*--- SKIP' -Context 0,1 | ForEach-Object { Write-Host "      $($_.Line.Trim())" }
}
Write-Host "    full log: $log"

# 5. Coverage -----------------------------------------------------------------
if ($Coverage -and $exit -eq 0) {
    Step "Coverage (merged across all test binaries)"
    # Write-Host everywhere, so the report prints in order with the rest.
    go tool covdata percent "-i=$coverDir" | Where-Object { $_ -match '%' } | ForEach-Object {
        $line = $_.Trim() -replace 'inventory-order-engine/internal/', '' -replace '\s+coverage:\s+', ' ' -replace ' of statements', ''
        $pkg, $pct = $line -split ' '
        Write-Host ("    {0,-12} {1,7}" -f $pkg, $pct)
    }
    go tool covdata textfmt "-i=$coverDir" "-o=coverage.out"
    go tool cover "-html=coverage.out" "-o=coverage.html"
    $total = (go tool cover "-func=coverage.out" | Select-Object -Last 1) -replace '.*\s', ''
    Write-Host ("    {0,-12} {1,7}" -f "TOTAL", $total) -ForegroundColor Green
    Write-Host "    HTML report: coverage.html (open it in a browser)"
}

if ($exit -ne 0) { Write-Host "`nTESTS FAILED" -ForegroundColor Red; exit $exit }
Write-Host "`nALL TESTS PASSED" -ForegroundColor Green
