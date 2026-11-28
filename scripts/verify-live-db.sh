#!/usr/bin/env bash
set -euo pipefail

PORT="${PG_TEST_PORT:-54329}"
export PG_TEST_PORT="$PORT"
DB_URL="postgres://postgres:password@localhost:${PORT}/pg_test?sslmode=disable"

export TEST_DATABASE_URL="$DB_URL"
export DATABASE_URL="$DB_URL"
export REQUIRE_DB="1"
export JWT_SECRET="${JWT_SECRET:-ci_test_jwt_secret_must_be_32_characters_long!!}"
export OTP_HMAC_SECRET="${OTP_HMAC_SECRET:-ci_test_otp_hmac_secret_32_chars_long!!}"
export MAGIC_LINK_HMAC_SECRET="${MAGIC_LINK_HMAC_SECRET:-ci_test_magic_link_secret_32_chars!}"
export KYC_IDENTITY_SECRET="${KYC_IDENTITY_SECRET:-ci_test_kyc_identity_secret_32_chars!}"

cleanup() {
  docker compose -f docker-compose.test.yml down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker compose -f docker-compose.test.yml up -d --wait
go run ./cmd/migrate -url "$DB_URL"
go test -count=1 ./internal/postgres/...
go test -count=1 ./internal/api -run "Payout|Webhook|Settle|TestListPayments_Subsecond|TestOwnerListUnbatchedPayoutItems|TestHandlePaymentSuccessWebhook"

for sql in \
  scripts/sql/ledger_controls_test.sql \
  scripts/sql/ledger_trigger_coverage_test.sql \
  scripts/sql/rls_coverage_test.sql \
  scripts/sql/verify_gate1_1_explain.sql; do
  echo "Executing $sql..."
  docker compose -f docker-compose.test.yml exec -T postgres psql -U postgres -d pg_test -v ON_ERROR_STOP=1 < "$sql"
done
