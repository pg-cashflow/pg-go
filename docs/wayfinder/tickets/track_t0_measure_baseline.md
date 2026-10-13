# Ticket T0: Measure Baseline Test & Coverage Metrics

- **Track:** T0
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `docs/wayfinder/BASELINE_COVERAGE.md`

## Acceptance Criteria
- [x] Run full test suite with coverage (`go test -count=1 -coverprofile coverage.out ./...`).
- [x] Run `go vet ./...` (0 errors).
- [x] Run `scripts/audit_skips.go` (0 skips).
- [x] Record per-package coverage in `docs/wayfinder/BASELINE_COVERAGE.md`.
- [x] Unblocks Ticket T6.
