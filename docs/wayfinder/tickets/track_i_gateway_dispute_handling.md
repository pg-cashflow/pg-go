# Ticket 8 (Track I): Gateway Dispute & Chargeback Webhook Fail-Safe Handling

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Objective & Threat Model

While card-based chargebacks are rare in Indian PG accommodation (where UPI and Netbanking dominate), payment gateways (Cashfree) deliver dispute webhooks (`PAYMENT_DISPUTE_CREATED_WEBHOOK`, `DISPUTE_CREATED_WEBHOOK`, `DISPUTE_STATUS_UPDATE_WEBHOOK`) whenever a payer initiates a retrieval, unauthorized claim, or chargeback.

### Risks
1. **Silent Ingestion / Unhandled Drops**: Dispute webhooks landing as unhandled default cases without persistent tracking or alerting could leave property owners unaware of pending dispute deadlines (e.g. 7-day evidence windows), resulting in default forfeiture.
2. **Premature Ledger Mutation (Corrupt Balance Invariant)**: Automatically reversing rent revenue or resetting settled dues to `pending` upon dispute notification violates double-entry integrity. A dispute is a contested claim, not a settled refund. If the owner submits valid lease/check-in evidence and wins the dispute, an automated debit corrupts accounting records and misrepresents tenant balances.
3. **Gateway Retry Storms**: Returning 4xx or 5xx on non-actionable dispute webhooks causes gateways to spam webhook retries, consuming server resources.

## Architectural Invariants & Remediation

1. **Typed Parsing**:
   - `DisputeWebhook` in `internal/cashfree/webhook.go` parses dispute ID, order ID, CF payment ID, dispute type, status, dispute amount (in pure integer paise), reason description, and respond-by deadline. Verified in `internal/cashfree/webhook_test.go:129-152`.
2. **Fail-Safe Audit & Loud Alerting**:
   - Ingests every dispute webhook into `webhook_events` with status `dispute_action_required`.
   - Logs critical operator alert via `slog.Default().Error("CASHFREE PAYMENT DISPUTE RECEIVED: OPERATOR ACTION REQUIRED", ...)`.
3. **Owner Notification**:
   - `EvtPaymentDisputed` in `internal/domain/event.go`.
   - Wired in `internal/notification/resolver.go` to route `EvtPaymentDisputed` directly to property owners with deep-link `/owner/payments`.
   - Outbox event written with `RoleOwner`.
4. **Ledger Immutability Guarantee**:
   - Strictly prohibits automated double-entry journal reversal on dispute receipt.
   - Settle state remains unaltered until the operator reviews the claim and records a manual adjustment or formal refund.
5. **Idempotent HTTP 200 Acknowledgment**:
   - Returns HTTP 200 OK after cryptographic verification and persistent recording to halt retry loops.
6. **Operational Runbook**:
   - Documented in [Dispute & Chargeback Operational Runbook](file:///c:/Users/divak/Downloads/pg-go/docs/runbooks/dispute_chargeback_runbook.md).
