# ADR 006: Payment Provider Abstraction & Gateway Integration — Provider Neutrality, Cashfree Adapter, Distributed Lease, Webhook Idempotency & Sandbox Strategy

## Status
Accepted
**Date:** 2026-09-24
**Decider:** Divakar (Solo Developer)

## Context
Following the unblocking of Cashfree Payment Gateway merchant credentials in Sandbox mode, and the pending business verification (Current Account, GST / Shops & Establishment registration) for production merchant onboarding, `pg-go` requires automated online rent collection.

Key architectural boundaries:
1. **Gateway Independence & Provider Abstraction**: Core business logic, dues management, and financial ledgers must never depend directly on a single third-party gateway API. A generic `PaymentProvider` interface normalizes order creation, payment fetching, refund verification, and webhook ingestion, allowing seamless switching or failover between Cashfree, Razorpay, PayU, and PhonePe.
2. **Sandbox Operation for Development & Day-0 Business Registration**: Development, end-to-end integration, and concurrency testing run against Cashfree's Sandbox environment. The codebase is fully provider-neutral and config-driven (`CASHFREE_ENV`), requiring zero application code modifications to switch environments. However, on the business side, Cashfree underwriting categorically rejects accommodation collections into personal savings accounts. Production activation requires an explicit **Day-0 Business Registration Workstream** (Udyam Registration on udyamregistration.gov.in -> Current Account opening in business name -> Cashfree merchant re-application) running in parallel with development, rather than waiting for physical PG launch.
3. **Multi-Due Double-Active Storage Guard**: In addition to `uq_active_intent_due` on `payment_intents(due_id)`, multi-due checkouts (where parent `due_id IS NULL`) are guarded at the database storage engine by `uq_active_intent_dues_item ON payment_intent_dues(due_id) WHERE status IN ('initiating', 'created')`, kept synchronized with parent intent state transitions via trigger `sync_payment_intent_dues_status`.
4. **Invoice-Based Collection, Not Subscription Mandates**: Tenants pay against monthly dues via links and reminders; no eNACH/UPI Autopay mandate complexity is introduced.
5. **Reuse Existing Foundation**: The platform already possesses `dues` (representing monthly invoices with `period_start`, `period_end`, `due_date`, `status`), `payment_intents` (table created in migration 004), `cmd/billing-cycle` (monthly due generator), and `cmd/reminder` (T-3/due/overdue notification cron).
6. **Single Beneficial Owner Model**: The platform operator is the sole beneficial owner of all properties. Rent payments are direct merchant receivables into the operator's merchant account. **No third-party fund pooling occurs**, eliminating RBI Payment Aggregator (PA) / escrow regulatory requirements under the 2020 PA-PG Master Direction.

## Decisions

### 1. Single-Owner Collection & Phased Settlement Ledger
- **Direct Receivables**: All gateway payments land directly in the single merchant bank account.
- **Phase 1 (Manual Settlement)**: `payout_records` tracks property-level operational distributions and accounting balances. The operator transfers funds to property/capital accounts via manual bank transfer (NEFT/IMPS) and records the UTR reference in `payout_records`.
- **Phase 2 (Automated Payouts)**: When scaled, an automated adapter invoking Cashfree Payouts swaps in behind the same ledger interface without schema migrations or UI redesign.
- **Integrity**: `platform_fee_paise` defaults to `0` with constraint `CHECK (net_amount_paise = gross_amount_paise - platform_fee_paise)`.

### 2. Line-Item Settlement Audit Trail (`payout_record_items`)
- To prevent disputes regarding which payments comprise an operational payout, each `payout_records` row is backed by `payout_record_items` linking specific `due_id` and `payment_id` rows.
- Uniqueness is enforced via `CONSTRAINT uq_payout_item_payment UNIQUE (payment_id)`, preventing any payment from being double-settled across multiple payout batches.

### 3. Distributed In-Flight Order Lease (`payment_in_flight_lock`)
- To prevent a tenant double-tapping "Pay Now" or opening multiple browser tabs from creating duplicate Cashfree orders for the same due, a 30-second distributed lease table `payment_in_flight_lock` (mirroring `kyc_in_flight_lock`) serializes intent creation across Cloud Run instances.

### 4. Webhook Signature Verification & Transactional Deduplication
- **HMAC Verification**: The public gateway endpoint (`POST /webhooks/cashfree`) enforces constant-time signature verification using `hmac.Equal` via `cashfree.VerifyWebhookHMAC` against `CASHFREE_WEBHOOK_SECRET` over raw body bytes + `x-webhook-timestamp`.
- **Deduplication Ledger**: Cashfree PG webhooks provide `cf_payment_id` and `order_id` rather than a generic top-level UUID event ID. Webhook deliveries are deduplicated via `processed_webhook_events` using composite key `cashfree:pg:<cf_payment_id>`.
- **Atomic Transaction Boundary**: Inserting the dedup key and executing `GatewaySettle` occur within the **same atomic database transaction**. Any settlement error rolls back the dedup insertion, ensuring Cashfree delivery retries are never swallowed.

### 5. Triple-Path Serialization on `dues`
All three settlement paths are serialized on the same database row lock:
1. Owner Cash Settlement (`MarkCashPaid`)
2. Owner UTR Screenshot Proof Approval (`ConfirmPaymentReport` → `ManualMatch`)
3. Gateway Webhook Capture (`GatewaySettle`)

All three paths execute `getDueForUpdate` (`SELECT ... FOR UPDATE` on `dues`) inside `runInTx`.

### 6. Zero-Loss Race-Loser Gateway Settlement & Event Semantics
- **The Race-Loser Money Resolution**: If cash or manual UTR approval wins the row-lock race and marks a due `paid`, a concurrent Cashfree gateway capture has already moved real money. `GatewaySettle` must **never** abort or discard the payment write.
- When `GatewaySettle` encounters an already closed due (`due.Status IN ('paid', 'waived')`), it persists the `Payment` row (`matched_by = 'cashfree'`) and converts 100% of the amount into a tenant credit balance via `addTenantCredit(ctx, tenants, due.TenantID, amountPaise)`.
- **Distinct Event Semantics**: Emits a dedicated `domain.EvtOverpaymentCredited` event carrying `DueID`, `PaymentID`, `AmountPaise`, and `Reason: "due_already_closed"`.
- It explicitly **avoids** emitting `EvtPaymentMatched` or `EvtDuePaidOnTime/Late`, preventing accidental gamification streak increments, duplicate reward point awards, or confusing "rent paid" push notifications.

### 7. PCI DSS Compliance Scope (SAQ-A)
- **Hosted Checkout Scope**: `pg-go` exclusively integrates via Cashfree's hosted checkout session (`payment_session_id`). Card, netbanking, and UPI credentials never enter, touch, or transit `pg-go` servers or database infrastructure.
- **Compliance Footprint**: As a merchant utilizing solely hosted checkout with redirect/embedded iframe where origin redirection occurs securely on Cashfree's PCI-DSS Level 1 certified servers, `pg-go` qualifies for Self-Assessment Questionnaire A (SAQ-A). This materially minimizes regulatory overhead and eliminates PCI audit exposure.

### 8. Dead-Letter Webhook Isolation & Ingestion Resilience
- **Strict Response Policy**: In accordance with payment gateway best practices, the webhook handler returns HTTP 200 on all dead-letter, malformed schema, or unparseable payloads after persisting the raw payload and headers in `webhook_events`. Returning 4xx/5xx on unparseable payloads is strictly prohibited as it triggers automated exponential retry storms from Cashfree that DoS the application.
- **Timestamp Tolerance**: A configurable 300-second drift window (`WEBHOOK_TIMESTAMP_TOLERANCE_SEC`) prevents replay attacks while tolerating normal network latency and clock jitter.

### 9. Under-Lock Intent Reuse & Race Elimination
- **Check-Then-Act Guard**: To prevent concurrent checkouts (e.g. double-tapping pay or parallel browser tabs) from generating duplicate external Cashfree orders, the intent reuse check is executed *under top-down database locks* (`tenants` -> `dues` -> `payment_intents` -> `payment_intent_dues`).
- **10-Minute Validity Threshold**: If an active unexpired intent with $\ge 10$ minutes remaining already exists for the requested dues, its existing `payment_session_id` is reused. Only when no active intent exists or remaining validity is $< 10$ minutes are older intents superseded and a new Cashfree order token created.

### 10. Ledger Accounts & Derived Due Recomputation
- **Gateway Clearing**: Gateway collections Dr `gateway_clearing` (not directly `cash` or `bank`), balancing when Cashfree settles net funds into the SBI savings account.
- **Unapplied Receipts**: Unapplied duplicate or race-loser payments credit `unapplied_receipts` pending owner refund or manual re-allocation.
- **Derived Invariance**: Due statuses are derived strictly from allocations:
  `Net Paid = SUM(payment_allocations) − SUM(refund_allocations)`.
  This eliminates paise-drift accumulation and avoids corrupting the financial ledger.

### 11. Stuck Refund Reconciliation Poller
- A scheduled background poller mirrors `cmd/cashfree-poll` by querying Cashfree's refund-status API for any `gateway_refunds` rows older than 1 hour in non-terminal states (`initiated`, `pending`, `on_hold`), closing the window where an application crash during a refund call orphans the local ledger.

### 12. Payout Security & Compliance (Step 3 Horizon)
- **Envelope Encryption for Bank Details**: Bank account numbers in `payout_payees` will use AES-256-GCM envelope encryption backed by GCP Cloud KMS (KEK) rather than a static environment variable, ensuring zero plaintext exposure in database backups.
- **Evidence Photo Access Control**: `departure_deductions.evidence_photo_key` will strictly enforce authenticated short-lived signed URLs, accessible only by the verified property owner and the specific departing tenant or their guardian.
- **DPDP Act Parental Consent**: Minor tenants (< 18 years) require DigiLocker-verified guardian KYC and signed parental consent, to be validated against finalized Digital Personal Data Protection (DPDP) Rules prior to Step 3 deployment.
- **Payout Item Deduplication**: The `UNIQUE (payee_id, purpose, period_label, reference_number)` constraint intentionally acts as a safeguard against double payroll/vendor runs; distinct payouts to the same payee within a single period must deterministically provide unique `reference_number`s (e.g. `ADVANCE-2026-10` vs `SALARY-2026-10`).

### 13. Payment Provider Abstraction & Gateway Independence
- **Provider Interface (`PaymentProvider`)**: Isolates external gateway mechanics from the core payment collection and reconciliation services. Standardizes `CreateOrder`, `FetchPayment`, `FetchRefund`, `VerifyWebhook`, and `ParseWebhook`.
- **Normalized Event Envelope (`NormalizedWebhookEvent`)**: Domain services interact only with canonical event types (`PAYMENT_SUCCESS`, `PAYMENT_FAILED`, `REFUND_STATUS`, `AUTO_REFUND`) and standardized amounts in integer paise, preventing provider-specific payload changes from breaking the application.
- **Sandbox Operational Independence**: Allows running all development and pre-launch end-to-end testing against active Sandbox credentials, while switching to production credentials upon obtaining business documentation with zero code alterations.

## Consequences & Known Gaps

1. **Long-Term Multi-Owner Expansion Horizon**:
   If the platform expands in the future (on a 5–10 year horizon) to onboard independent third-party PG owners, this direct-receivables model ceases to apply. The platform must transition to Cashfree Easy Split (vendor sub-merchant accounts with split-at-capture) or obtain an RBI Payment Aggregator license before pooling third-party funds.
2. **Reversals on Overpayment Credit**:
   If Cashfree later issues a chargeback/refund on a payment that was already converted to tenant credit (or applied to a subsequent billing cycle), automated reversal is deferred in Phase 1. The admin manually balances the ledger via existing adjustment workflows.
3. **Auditability**:
   Every rupee collected is tracked either against an invoice due or as a tenant credit balance, with line-item traceability from tenant payment to owner payout.

