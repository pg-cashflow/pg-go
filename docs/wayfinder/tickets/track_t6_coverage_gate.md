# Ticket T6: Coverage Gate (G4)

- **Track:** T6
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `scripts/check-coverage.go`, `docs/wayfinder/BASELINE_COVERAGE.md`

## Acceptance Criteria
- [x] Add coverage regression floors for 12 packages based on T0 measured baseline minus 1 point:
  - `internal/billing`: 46.1% (baseline 47.1%)
  - `internal/cashfree`: 63.9% (baseline 64.9%)
  - `internal/kyc`: 57.0% (baseline 58.0%)
  - `internal/domain`: 31.3% (baseline 32.3%)
  - `internal/crypto`: 77.0% (baseline 78.0%)
  - `internal/collector`: 27.4% (baseline 28.4%)
  - `internal/csv`: 86.0% (baseline 87.0%)
  - `internal/aadhaar`: 83.0% (baseline 84.0%)
  - `internal/magiclink`: 65.1% (baseline 66.1%)
  - `internal/join`: 45.5% (baseline 46.5%)
  - `internal/search`: 74.7% (baseline 75.7%)
  - `internal/attendance`: 37.5% (baseline 38.5%)
- [x] Raise `internal/payment` floor from 51.5% to 52.4% (stepping towards 85%).
- [x] `go run scripts/check-coverage.go -profile coverage.out -require-all` passes with exit code 0.
