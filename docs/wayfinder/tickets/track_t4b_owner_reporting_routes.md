# Ticket T4b: Owner Reporting Routes (G3)

- **Track:** T4b
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/api/handlers_owner_reporting_test.go`

## Acceptance Criteria
- [x] Comprehensive test coverage for all owner reporting routes:
  1. `FinanceSummary`: returns operating and collections summary scoped to property, totals match plain sum baseline.
  2. `FinanceLedger`: returns ledger entries scoped strictly to caller's property.
  3. `ListAdvances`: returns manager advances scoped to property, matching advance baseline.
  4. `FinanceVarianceBridge`: verifies variance bridge response shape for caller's property.
  5. `ListExpenseImports`: verifies import suggestions response shape for caller's property.
  6. `OwnerROI*`: verifies ROI report, recovery, break-even, and scenario simulation endpoints scoped to caller's property.
  7. `InsightsSummary`: verifies leakage events and recommendations scoped to property, matching leakage sum baseline.
  8. `OwnerCOI`: verifies Cost of Inaction computed strictly on caller's leakage.
  9. `OwnerForecast`: verifies cash flow forecast scoped to caller's property.
  10. `Reconciliation`: verifies reconciliation summary scoped to caller's property matching collections baseline.
  11. `ListEvents`: verifies audit events scoped strictly to caller's property.
