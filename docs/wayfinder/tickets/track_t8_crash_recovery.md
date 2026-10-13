# Ticket T8: Crash and Recovery (G7)

- **Track:** T8
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/jobs/crash_recovery_test.go`, `.github/workflows/nightly-crash-recovery.yml`

## Acceptance Criteria
- [x] Kill `cmd/billing-cycle` mid-batch via subprocess test, rerun, and assert:
  - No duplicate rent dues created.
  - Financial ledger balanced (`sum(debit) == sum(credit)`).
- [x] Stop the outbox worker mid-event and assert recovery:
  - Interrupted event remains pending with zero journal lines written.
  - Recovery run succeeds and marks event processed.
  - Worker retry preserves idempotency with zero duplicate journal lines.
- [x] Add nightly crash and recovery GitHub Action workflow (`.github/workflows/nightly-crash-recovery.yml`).
