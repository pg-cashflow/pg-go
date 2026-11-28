param(
  [int]$Port = 54329,
  [switch]$KeepContainer
)

$ErrorActionPreference = "Stop"
$env:PG_TEST_PORT = "$Port"
$dbUrl = "postgres://postgres:password@localhost:${Port}/pg_test?sslmode=disable"

$env:TEST_DATABASE_URL = $dbUrl
$env:DATABASE_URL = $dbUrl
$env:REQUIRE_DB = "1"
if (-not $env:JWT_SECRET) { $env:JWT_SECRET = "ci_test_jwt_secret_must_be_32_characters_long!!" }
if (-not $env:OTP_HMAC_SECRET) { $env:OTP_HMAC_SECRET = "ci_test_otp_hmac_secret_32_chars_long!!" }
if (-not $env:MAGIC_LINK_HMAC_SECRET) { $env:MAGIC_LINK_HMAC_SECRET = "ci_test_magic_link_secret_32_chars!" }
if (-not $env:KYC_IDENTITY_SECRET) { $env:KYC_IDENTITY_SECRET = "ci_test_kyc_identity_secret_32_chars!" }

Write-Host "Starting disposable PostgreSQL 17 container on port $Port..."
docker compose -f docker-compose.test.yml up -d --wait
if ($LASTEXITCODE -ne 0) { throw "Failed to start docker-compose.test.yml" }

try {
  Write-Host "Running migrations (001..latest)..."
  go run ./cmd/migrate -url $dbUrl
  if ($LASTEXITCODE -ne 0) { throw "Migrations failed" }

  Write-Host "Running live postgres tests (RLS, roles, payout cursor, webhook audit)..."
  go test -count=1 ./internal/postgres/...
  if ($LASTEXITCODE -ne 0) { throw "internal/postgres live tests failed" }

  Write-Host "Running API payout/webhook/settlement tests..."
  go test -count=1 ./internal/api -run "Payout|Webhook|Settle|TestListPayments_Subsecond|TestOwnerListUnbatchedPayoutItems|TestHandlePaymentSuccessWebhook"
  if ($LASTEXITCODE -ne 0) { throw "internal/api tests failed" }

  $sqlFiles = @(
    "scripts/sql/ledger_controls_test.sql",
    "scripts/sql/ledger_trigger_coverage_test.sql",
    "scripts/sql/rls_coverage_test.sql",
    "scripts/sql/verify_gate1_1_explain.sql"
  )
  foreach ($sql in $sqlFiles) {
    Write-Host "Executing $sql..."
    Get-Content -Raw $sql | docker compose -f docker-compose.test.yml exec -T postgres psql -U postgres -d pg_test -v ON_ERROR_STOP=1
    if ($LASTEXITCODE -ne 0) { throw "SQL suite failed: $sql" }
  }

  Write-Host "Live PostgreSQL 17 verification passed."
}
finally {
  if (-not $KeepContainer) {
    Write-Host "Tearing down disposable PostgreSQL 17 container..."
    docker compose -f docker-compose.test.yml down -v
  }
}
