# Ticket T2: Webhook Replay Parity (G2) — Decision D1

- **Track:** T2
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/api/webhook_replay_parity_test.go`, `internal/api/handlers_webhooks.go`, `internal/config/config.go`, `internal/config/config_test.go`

## Acceptance Criteria
- [x] Red tests first in `internal/api/webhook_replay_parity_test.go`:
  - Payments: stale timestamp refused (401), tolerance <= 0 behaves safely as 300.
  - Payouts: stale timestamp refused (401), tolerance <= 0 behaves safely as 300.
  - Settlement: stale timestamp refused (401), tolerance <= 0 behaves safely as 300.
  - KYC (Decision D1): stale timestamp suppresses processing (returns 200, logs warning, skips database mutation), tolerance <= 0 behaves safely as 300.
- [x] Configuration fail-closed validation:
  - `config.Load` rejects `WEBHOOK_TIMESTAMP_TOLERANCE_SEC <= 0` at startup with an explicit error.
  - Tested in `internal/config/config_test.go:TestLoadRejectsNonPositiveWebhookTimestampTolerance`.
- [x] All 4 webhook handlers reject or suppress stale replays in tests.
