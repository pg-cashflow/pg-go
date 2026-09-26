# Track J (Ticket 9): Ledger Outbox Dead-Letter Active Operator Alerting

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Objective

In `internal/finance/ledger_worker.go`, `ProcessSingleEvent` processes transactional outbox events (`departure_settlement_mirror`). When an event exceeded `MaxAttempts`, it recorded `failed_at` in PostgreSQL, but there was no active operator escalation. The only logging was a generic failure message in `ProcessBatch` that treated dead letters identically to transient retries, allowing ledger desynchronization to go unnoticed if an unmirrored departure permanently stalled.

## Implementation Details

1. **Domain-Specific Alerter Interface & Mailer Integration**:
   - Created `internal/finance/alert.go` defining `DeadLetterNotifier` interface:
     ```go
     type DeadLetterNotifier interface {
         NotifyDeadLetter(ctx context.Context, evt *domain.LedgerOutboxEvent, failureErr string) error
     }
     ```
   - Implemented `EmailDeadLetterNotifier` wrapping `mailer.Mailer` directly (`n.mailer.Send(...)`) with structured HTML templates containing all forensic metadata: Event ID, Type, Property ID, Source ID, Idempotency Key, Timestamp, and Terminal Error.
   - Purpose-built in `finance` without polluting or conflating `sms.MailAlert` (which was purpose-built for SMS gateway failover).

2. **Loud Escalation & Dispatch in Worker**:
   - Updated `LedgerOutboxWorker` in `internal/finance/ledger_worker.go` with optional `alerter` dependency via backward-compatible variadic constructor `NewLedgerOutboxWorker(..., alerter ...DeadLetterNotifier)` and `SetAlerter`.
   - In `ProcessSingleEvent`, once failure state is committed to PostgreSQL and `isDeadLetter` is true:
     - Emits loud `slog.Error("CRITICAL: ledger outbox event exceeded max attempts - financial books desynchronized", ...)` with event, source, and property context.
     - Actively calls `w.alerter.NotifyDeadLetter(ctx, evt, dispatchErr.Error())`.

3. **Verification**:
   - `internal/finance/alert_test.go`: `TestEmailDeadLetterNotifier_Send` and `TestEmailDeadLetterNotifier_ErrorsAndNil` verifying email payload generation, recipient targeting, and graceful handling of nil receivers/mailers.
   - `internal/finance/ledger_worker_test.go`: Enhanced `TestLiveLedgerOutboxWorkerAndReconciliation` with `mockAlerter`, proving zero alerts fire on retryable failures (attempt 1 of 2) and exactly 1 alert fires when reaching terminal dead-letter status (attempt 2 of 2).
