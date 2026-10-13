# Baseline Test & Coverage Report (Ticket T0)

- **Date:** 2026-10-13
- **Branch:** `feat/close-test-gaps`
- **Go Version:** `go version go1.26.6 windows/amd64`
- **Database:** PostgreSQL 17 (`postgresql-x64-17`) on `localhost:5432` (`pg_test`)

## 1. Static Analysis (`go vet`)
- **Command:** `go vet ./...`
- **Result:** Exit 0 (0 warnings, 0 errors).

## 2. Skip Audit (`Gate 01`)
- **Command:** `go run scripts/audit_skips.go <test_output_log>`
- **Result:** Exit 0 (`Gate 01 Success: 0 tests skipped across entire test suite`).

## 3. Measured Per-Package Statement Coverage

| Package | Status | Duration | Statement Coverage |
| :--- | :--- | :--- | :--- |
| `cmd/billing-cycle` | PASS | 14.893s | 0.0% |
| `cmd/cashfree-poll` | PASS | 12.125s | 0.0% |
| `cmd/daily-rollup` | PASS | 5.214s | 0.0% |
| `cmd/digilocker-reconcile` | PASS | 5.009s | 0.0% |
| `cmd/financial-summary` | PASS | 14.575s | 0.0% |
| `cmd/gamification-cycle` | PASS | 8.924s | 0.0% |
| `cmd/kpi-snapshot` | PASS | 11.527s | 0.0% |
| `cmd/kyc-expiry` | PASS | 9.744s | 0.0% |
| `cmd/migrate` | PASS | 11.524s | 0.0% |
| `cmd/reminder` | PASS | 11.166s | 0.0% |
| `cmd/sandbox-smoke` | PASS | 7.015s | 0.0% |
| `cmd/search-reindex` | PASS | 1.928s | 0.0% |
| `cmd/server` | PASS | 18.326s | 0.0% |
| `internal/aadhaar` | PASS | 2.028s | 84.0% |
| `internal/api` | PASS | 72.895s | 65.8% |
| `internal/apierr` | PASS | 3.572s | 100.0% |
| `internal/attendance` | PASS | 1.604s | 38.5% |
| `internal/auth` | PASS | 23.417s | 67.6% |
| `internal/billing` | PASS | 2.824s | 47.1% |
| `internal/cashfree` | PASS | 4.209s | 64.9% |
| `internal/collector` | PASS | 2.795s | 28.4% |
| `internal/config` | PASS | 1.842s | 72.2% |
| `internal/crypto` | PASS | 1.838s | 78.0% |
| `internal/csv` | PASS | 2.309s | 87.0% |
| `internal/domain` | PASS | 2.231s | 32.3% |
| `internal/events` | PASS | 1.656s | 86.4% |
| `internal/finance` | PASS | 17.372s | 77.5% |
| `internal/gamification` | PASS | 4.191s | 43.0% |
| `internal/intelligence` | PASS | 3.143s | 78.6% |
| `internal/jobs` | PASS | 164.507s | 64.8% |
| `internal/join` | PASS | 13.519s | 46.5% |
| `internal/kyc` | PASS | 1.937s | 58.0% |
| `internal/localization` | PASS | 4.712s | 44.0% |
| `internal/magiclink` | PASS | 2.896s | 66.1% |
| `internal/mailer` | PASS | 3.346s | 61.9% |
| `internal/notification` | PASS | 3.466s | 18.8% |
| `internal/payment` | PASS | 3.271s | 53.4% |
| `internal/postgres` | PASS | 100.150s | 38.9% |
| `internal/push` | PASS | 3.593s | 48.5% |
| `internal/qr` | PASS | 1.998s | 76.5% |
| `internal/requestscope` | PASS | 3.873s | 100.0% |
| `internal/roi` | PASS | 4.482s | 53.2% |
| `internal/search` | PASS | 2.578s | 75.7% |
| `internal/sms` | PASS | 6.703s | 73.0% |
| `internal/tenant` | PASS | 14.216s | 26.8% |
| `internal/testutil` | PASS | 2.686s | 100.0% |
| `internal/timeutil` | PASS | 1.853s | 100.0% |
| `internal/web` | PASS | 7.551s | 69.2% |
