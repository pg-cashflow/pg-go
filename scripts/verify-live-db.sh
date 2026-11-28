#!/usr/bin/env bash
set -euo pipefail

PORT="${PG_TEST_PORT:-54329}"
export PG_TEST_PORT="$PORT"
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-pg_go_live_verify_$$}"
DB_URL="postgres://postgres:password@localhost:${PORT}/pg_test?sslmode=disable"

export TEST_DATABASE_URL="$DB_URL"
export DATABASE_URL="$DB_URL"
export REQUIRE_DB="1"
export JWT_SECRET="${JWT_SECRET:-ci_test_jwt_secret_must_be_32_characters_long!!}"
export OTP_HMAC_SECRET="${OTP_HMAC_SECRET:-ci_test_otp_hmac_secret_32_chars_long!!}"
export MAGIC_LINK_HMAC_SECRET="${MAGIC_LINK_HMAC_SECRET:-ci_test_magic_link_secret_32_chars!}"
export KYC_IDENTITY_SECRET="${KYC_IDENTITY_SECRET:-ci_test_kyc_identity_secret_32_chars!}"
export PAYOUT_CHECKSUM_SECRET="${PAYOUT_CHECKSUM_SECRET:-ci_test_payout_checksum_secret_32_chars!}"

compose() {
  docker compose -p "$COMPOSE_PROJECT_NAME" -f docker-compose.test.yml "$@"
}

cleanup() {
  compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Preflight teardown guarantees a clean disposable database even if a prior run was interrupted.
compose down -v --remove-orphans >/dev/null 2>&1 || true
compose up -d --wait

go run ./cmd/migrate -url "$DB_URL"
go test -count=1 ./internal/postgres/...
go test -count=1 ./internal/api -run "Payout|Webhook|Settle|TestListPayments_Subsecond|TestOwnerListUnbatchedPayoutItems|TestHandlePaymentSuccessWebhook"
go test -count=1 ./internal/finance -run "TestLive|TestCrashSafety|TestB15|TestB17|TestPhysicalCrashSafety|TestMoneyMath|TestConcurrent"
go test -count=1 ./internal/jobs -run "TestGate07|TestGate12|TestCashfreePoll|TestPaymentIntentQuarantine"

if [ "${RUN_FULL_SUITE:-0}" = "1" ]; then
  go test -count=1 ./...
fi

for sql in \
  scripts/sql/ledger_controls_test.sql \
  scripts/sql/ledger_trigger_coverage_test.sql \
  scripts/sql/rls_coverage_test.sql \
  scripts/sql/rls_property_scope_ratchet.sql \
  scripts/sql/verify_gate1_1_explain.sql; do
  echo "Executing $sql..."
  compose exec -T postgres psql -U postgres -d pg_test -v ON_ERROR_STOP=1 < "$sql"
done
