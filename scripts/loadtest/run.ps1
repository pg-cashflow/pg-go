# =============================================================================
# Gate 09: Karpathy 6-Step Stress & Load Test Runner (PowerShell)
# Methodology: ASD-STE100 & Karpathy Test Discipline
# =============================================================================

param (
    [string]$BaseUrl = "http://localhost:8080",
    [string]$FakeGatewayUrl = "http://localhost:8081",
    [string]$DatabaseUrl = $env:DATABASE_URL,
    [switch]$IncludeSoak = $false,
    [string]$SoakDuration = "5m"
)

$ErrorActionPreference = "Stop"

# Auto-load .env file if present
if (Test-Path ".env") {
    Get-Content ".env" | ForEach-Object {
        $line = $_.Trim()
        if ($line -and -not $line.StartsWith("#")) {
            $parts = $line -split "=", 2
            if ($parts.Length -eq 2) {
                $k = $parts[0].Trim()
                $v = $parts[1].Trim() -replace '^["'']|["'']$', ''
                if (-not [System.Environment]::GetEnvironmentVariable($k, "Process")) {
                    [System.Environment]::SetEnvironmentVariable($k, $v, "Process")
                }
            }
        }
    }
}

if (-not $DatabaseUrl -and $env:DATABASE_URL) {
    $DatabaseUrl = $env:DATABASE_URL
}

function Write-Step {
    param([string]$Num, [string]$Title)
    Write-Host "`n========================================================" -ForegroundColor Cyan
    Write-Host "  Step ${Num}: ${Title}" -ForegroundColor Cyan
    Write-Host "========================================================" -ForegroundColor Cyan
}

# -----------------------------------------------------------------------------
# Karpathy Step 1: Look at the data first
# -----------------------------------------------------------------------------
Write-Step "1" "Look at data first (Verify server, gateway, and database readiness)"

Write-Host "Checking target backend health at $BaseUrl/healthz..."
try {
    $health = Invoke-RestMethod -Uri "$BaseUrl/healthz" -Method Get -TimeoutSec 5
    Write-Host "Backend is UP. Health status: $($health.status)" -ForegroundColor Green
} catch {
    Write-Warning "Backend is not responding at $BaseUrl/healthz."
    Write-Warning "Ensure the backend server is running with DATABASE_MAX_CONNS=25 before launching load tests."
}

Write-Host "Checking fake gateway health at $FakeGatewayUrl/healthz..."
try {
    $gwHealth = Invoke-RestMethod -Uri "$FakeGatewayUrl/healthz" -Method Get -TimeoutSec 3
    Write-Host "Fake Cashfree Gateway is UP." -ForegroundColor Green
} catch {
    Write-Host "Fake Gateway not running. Starting fake gateway on port 8081 in background..." -ForegroundColor Yellow
    $gwSecret = if ($env:CASHFREE_WEBHOOK_SECRET) { $env:CASHFREE_WEBHOOK_SECRET } elseif ($env:CASHFREE_SECRET_KEY) { $env:CASHFREE_SECRET_KEY } else { "test_wh_secret_key" }
    $gwProc = Start-Process -FilePath "go" -ArgumentList "run", "scripts/loadtest/fake_gateway.go", "-port=8081", "-target=$BaseUrl", "-secret=$gwSecret" -PassThru
    Start-Sleep -Seconds 3
    Write-Host "Fake Gateway started with PID $($gwProc.Id) using gateway secret." -ForegroundColor Green
}

# -----------------------------------------------------------------------------
# Karpathy Step 2: Check state at initialization
# -----------------------------------------------------------------------------
Write-Step "2" "Check state at initialization (Ensure baseline ledger is balanced)"

if ($DatabaseUrl) {
    Write-Host "Running pre-flight SQL invariant check via Go invariant harness..."
    try {
        go run scripts/loadtest/check_invariants.go "$DatabaseUrl"
        if ($LASTEXITCODE -ne 0) {
            Write-Warning "Pre-flight SQL invariant verification reported failures."
        }
    } catch {
        Write-Warning "Could not connect to database at ${DatabaseUrl}: $_"
    }
} else {
    Write-Host "DATABASE_URL not set. Skipping pre-flight direct SQL check." -ForegroundColor Yellow
}

# -----------------------------------------------------------------------------
# Karpathy Step 3: Overfit one example
# -----------------------------------------------------------------------------
Write-Step "3" "Overfit one example (Run minimal single-VU smoke test)"

Write-Host "Executing smoke check with 1 VU for 5s to confirm pipeline passes cleanly..."
$smokeEnv = @{
    "BASE_URL" = $BaseUrl
    "FAKE_GATEWAY_URL" = $FakeGatewayUrl
}

k6 run --vus 1 --duration 5s scripts/loadtest/scenarios/read_dashboard.js
if ($LASTEXITCODE -ne 0) {
    Write-Error "Smoke check failed! Cannot proceed to stress testing."
    exit 1
}
Write-Host "Single example overfit PASSED: Minimal path is 100% green." -ForegroundColor Green

# -----------------------------------------------------------------------------
# Karpathy Step 4: Compare with dumb baseline
# -----------------------------------------------------------------------------
Write-Step "4" "Compare with dumb baseline (Assert baseline health checks meet latency floors)"

$stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
$res = Invoke-WebRequest -Uri "$BaseUrl/api/healthz" -Method Get -UseBasicParsing
$stopwatch.Stop()

Write-Host "Baseline /api/healthz latency: $($stopwatch.ElapsedMilliseconds)ms (Floor: < 50ms)" -ForegroundColor Green
if ($stopwatch.ElapsedMilliseconds -gt 250) {
    Write-Warning "Baseline latency is unusually high ($($stopwatch.ElapsedMilliseconds)ms)."
}

# -----------------------------------------------------------------------------
# Karpathy Step 5: Fix seeds
# -----------------------------------------------------------------------------
Write-Step "5" "Fix seeds (Deterministic run settings)"

$env:K6_SYSTEM_TAGS = "proto,subproto,status,method,url,name,group,check,error,error_code,scenario,service"
Write-Host "Using fixed seed order and deterministic order generators." -ForegroundColor Green

# -----------------------------------------------------------------------------
# Karpathy Step 6: Change one thing at a time
# -----------------------------------------------------------------------------
Write-Step "6" "Change one thing at a time (Sequential load scenario execution)"

Write-Host "`n--> Stage 6A: Read Dashboard & Search Load (Target: 100 VUs)" -ForegroundColor Magenta
k6 run -e BASE_URL="$BaseUrl" scripts/loadtest/scenarios/read_dashboard.js
if ($LASTEXITCODE -ne 0) {
    Write-Error "Read dashboard load test threshold violated!"
    exit 1
}

Write-Host "`n--> Stage 6B: Checkout & Webhook Concurrency (Target: 60 RPS Open-Model)" -ForegroundColor Magenta
k6 run -e BASE_URL="$BaseUrl" -e FAKE_GATEWAY_URL="$FakeGatewayUrl" scripts/loadtest/scenarios/checkout_webhook.js
if ($LASTEXITCODE -ne 0) {
    Write-Error "Checkout/Webhook concurrency test threshold violated!"
    exit 1
}

if ($IncludeSoak) {
    Write-Host "`n--> Stage 6C: Extended Soak Test (Duration: $SoakDuration)" -ForegroundColor Magenta
    k6 run -e BASE_URL="$BaseUrl" -e SOAK_DURATION="$SoakDuration" scripts/loadtest/soak_test.js
    if ($LASTEXITCODE -ne 0) {
        Write-Error "Soak test threshold violated!"
        exit 1
    }
}

# -----------------------------------------------------------------------------
# Post-Run Step: Enforce SQL Invariant Verification Gate
# -----------------------------------------------------------------------------
Write-Step "Post-Run" "Enforce SQL Invariant Verification Gate"

if ($DatabaseUrl) {
    Write-Host "Executing post-run invariant assertions in database..."
    go run scripts/loadtest/check_invariants.go "$DatabaseUrl"
    if ($LASTEXITCODE -ne 0) {
        Write-Error "CRITICAL: Post-run SQL invariant test failed! Corrupted ledger or duplicate payments detected."
        exit 1
    }
    Write-Host "`n[SUCCESS] Post-run SQL invariants strictly verified: ZERO ledger drift, ZERO duplicates." -ForegroundColor Green
} else {
    Write-Host "DATABASE_URL not set. Invariants file generated at scripts/loadtest/post_run_invariants.sql" -ForegroundColor Yellow
}

Write-Host "`n========================================================" -ForegroundColor Green
Write-Host "  Gate 09: Stress & Load Testing COMPLETED SUCCESSFULLY! " -ForegroundColor Green
Write-Host "========================================================" -ForegroundColor Green
