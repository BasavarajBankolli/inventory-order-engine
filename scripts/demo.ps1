<#
.SYNOPSIS
    Runs the three headline demonstrations against the running Docker stack:

      1. Stock = 1, 100 concurrent orders -> exactly 1 succeeds, 99 OUT_OF_STOCK,
         inventory never negative.
      2. The same Idempotency-Key sent 50 times at once -> one order, no duplicates.
      3. Payment declined -> reservation released -> inventory restored.

.EXAMPLE
    .\scripts\demo.ps1
    .\scripts\demo.ps1 -Buyers 200 -Stock 5

.NOTES
    LOCAL DEVELOPMENT ONLY. It creates a demo admin account (promoted directly
    in the database) and temporarily raises the API rate limit, because 100+
    logins from one IP is exactly what the rate limiter exists to stop. The
    normal limit is restored at the end.
#>
param(
    [int]$Buyers = 100,
    [int]$Stock = 1
)

$ErrorActionPreference = "Continue"   # see scripts/test.ps1 for why not "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

$base = "http://localhost:8080"
$adminEmail = "demo-admin@example.com"
$adminPassword = "demo-admin-password"   # local-only demo account

function Step($msg) { Write-Host "`n==> $msg" -ForegroundColor Cyan }

function Wait-Ready {
    for ($i = 0; $i -lt 60; $i++) {
        try {
            Invoke-RestMethod "$base/ready" -TimeoutSec 2 | Out-Null
            return
        } catch { Start-Sleep -Milliseconds 500 }
    }
    throw "API did not become ready at $base"
}

function Set-ApiRateLimit($perMinute) {
    if ($perMinute) { $env:RATE_LIMIT_PER_MINUTE = $perMinute } else { Remove-Item Env:RATE_LIMIT_PER_MINUTE -ErrorAction SilentlyContinue }
    docker compose up -d api 2>&1 | Out-Null   # recreates the api container with the new setting
    Wait-Ready
}

Step "Starting the stack (docker compose up -d --build)"
docker compose up -d --build 2>&1 | Out-Null
if ($LASTEXITCODE -ne 0) { throw "docker compose failed" }
Wait-Ready

Step "Preparing the demo admin ($adminEmail)"
try {
    $body = @{ email = $adminEmail; name = "Demo Admin"; password = $adminPassword } | ConvertTo-Json
    Invoke-RestMethod -Method Post "$base/api/v1/auth/register" -ContentType "application/json" -Body $body | Out-Null
} catch { }   # 409: already exists from an earlier run
docker compose exec -T postgres psql -U app -d inventory -q -c "UPDATE users SET role = 'ADMIN' WHERE email = '$adminEmail';" | Out-Null

Step "Raising the rate limit for the demo (100+ logins from one IP)"
Set-ApiRateLimit "5000"

$failed = 0
try {
    Step "Demo 1: $Buyers concurrent buyers, stock = $Stock"
    go run ./cmd/concurrency-demo -admin-email $adminEmail -admin-password $adminPassword -buyers $Buyers -stock $Stock
    if ($LASTEXITCODE -ne 0) { $failed++ }

    Step "Demo 2: the same Idempotency-Key sent 50 times at once"
    go run ./cmd/concurrency-demo -mode idempotency -buyers 50 -admin-email $adminEmail -admin-password $adminPassword
    if ($LASTEXITCODE -ne 0) { $failed++ }

    Step "Demo 3: payment failure releases the reservation"
    go run ./cmd/concurrency-demo -mode payment-failure -stock 5 -quantity 2 -admin-email $adminEmail -admin-password $adminPassword
    if ($LASTEXITCODE -ne 0) { $failed++ }
}
finally {
    Step "Restoring the normal rate limit"
    Set-ApiRateLimit $null
}

if ($failed -eq 0) {
    Write-Host "`nALL DEMOS PASSED" -ForegroundColor Green
} else {
    Write-Host "`n$failed DEMO(S) FAILED" -ForegroundColor Red
    exit 1
}
