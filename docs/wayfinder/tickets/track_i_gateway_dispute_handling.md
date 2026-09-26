# Ticket 8 (Track I): Gateway Dispute & Chargeback Webhook Fail-Safe Handling

- **Type**: `wayfinder:task`
- **Status**: In Progress
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Objective & Threat Model

While card-based chargebacks are rare in Indian PG accommodation (where UPI and Netbanking dominate), payment gateways (Cashfree) deliver dispute webhooks (`PAYMENT_DISPUTE_CREATED_WEBHOOK`, `DISPUTE_CREATED_WEBHOOK`, `DISPUTE_STATUS_UPDATE_WEBHOOK`) whenever a payer initiates a retrieval, unauthorized claim, or chargeback.

### Risks
1. **Silent Ingestion / Unhandled Drops**: Dispute webhooks landing as unhandled default cases without persistent tracking or alerting could leave property owners unaware of pending dispute deadlines (e.g. 7-day evidence windows), resulting in default forfeiture.
2. **Premature Ledger Mutation (Corrupt Balance Invariant)**: Automatically reversing rent revenue or resetting settled dues to `pending` upon dispute notification violates double-entry integrity. A dispute is a contested claim, not a settled refund. If the owner submits valid lease/check-in evidence and wins the dispute, an automated debit corrupts accounting records and misrepresents tenant balances.
3. **Gateway Retry Storms**: Returning 4xx or 5xx on non-actionable dispute webhooks causes gateways to spam webhook retries, consuming server resources.

## Architectural Invariants & Remediation

1. **Typed Parsing**:
   - Add `DisputeWebhook` to `internal/cashfree/webhook.go` to parse dispute ID, order ID, CF payment ID, dispute type, status, dispute amount (in pure integer paise), reason description, and respond-by deadline.
2. **Fail-Safe Audit & Loud Alerting**:
   - Ingest every dispute webhook into `webhook_events` with status `dispute_action_required`.
   - Log critical operator alert via `slog.Error("CASHFREE PAYMENT DISPUTE RECEIVED: OPERATOR ACTION REQUIRED", ...)`.
3. **Owner Notification**:
   - Add `EvtPaymentDisputed` in `internal/domain/event.go`.
   - Wire `internal/notification/resolver.go` to route `EvtPaymentDisputed` directly to property owners with deep-link `/owner/payments`.
4. **Ledger Immutability Guarantee**:
   - Strictly prohibit automated double-entry journal reversal on dispute receipt.
   - Settle state remains unaltered until the operator reviews the claim and records a manual adjustment or formal refund.
5. **Idempotent HTTP 200 Acknowledgment**:
   - Return HTTP 200 OK after cryptographic verification and persistent recording.
