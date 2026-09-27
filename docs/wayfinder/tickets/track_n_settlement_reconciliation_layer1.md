# Ticket 13: Track N — Stream 3 Layer 1: Cashfree Settlement Ingress & Order-to-Intent Reconciliation

- **Type**: `wayfinder:task`
- **Status**: Resolved (Fully Implemented, Verified, and Tested)
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Prerequisite**: Track L (Payouts Transfers V2) & Track M (Staff Attendance & Wage Engine) committed and verified.

---

## 1. Objective

Implement **Stream 3 Layer 1 (Cashfree Settlement Ingress & Order-to-Intent Reconciliation)**:
1. Ingest Cashfree Payment Gateway settlement data via three unified ingress channels:
   - **Real-time webhooks** (`SETTLEMENT_SUCCESS`, `SETTLEMENT_FAILED`, `SETTLEMENT_REVERSED`)
   - **On-demand order-level API polling** (`GET /pg/orders/{order_id}/settlements`)
   - **Manual Settlement Summary CSV upload** (`POST /api/owner/settlements/import-csv`) as an operational fallback.
2. Reconcile settled funds against internal `payment_intents` and `payments` to verify gross amounts, quantify gateway MDR deductions (`service_charge`), GST (`service_tax`), and net adjustments (`adjustment` for refunds/chargebacks/disputes), and capture bank UTRs.
3. Unify journal posting into a single shared engine (`ProcessSettlement` in `internal/finance/settlement.go`), moving funds from `gateway_clearing` to `bank` with explicit recognition of `payment_processing_expense` and `gateway_adjustment`, maintaining $\sum\text{Debits} == \sum\text{Credits}$ without float drift.
4. Guarantee full audit trail and exception queue parity across all three ingress sources via `gateway_settlements`, recording failed or unbalanced batches as `reconciliation_status = 'discrepancy'` for human-gated operator review.

---

## 2. Grounded Cashfree Ingress Payloads (`2025-01-01` Ground Truth)

Directly verified against Cashfree's `2025-01-01` Payment Gateway API specification:

### A. Batch Settlement Webhook (`SETTLEMENT_SUCCESS`)
When Cashfree executes a settlement payout batch to the merchant's bank account, it delivers `SETTLEMENT_SUCCESS`:

```json
{
  "data": {
    "settlement": {
      "settlement_id": 738,
      "status": "SUCCESS",
      "amount_settled": 97.94,
      "utr": "1644822317781212",
      "settled_on": "2025-02-14T12:35:19+05:30",
      "settlement_type": "STANDARD",
      "payment_amount": 100,
      "service_charge": 1.75,
      "service_tax": 0.31,
      "adjustment": 0.00,
      "settlement_initiated_on": "2025-02-14T12:35:17+05:30"
    }
  },
  "event_time": "2025-02-14T12:35:20+05:30",
  "type": "SETTLEMENT_SUCCESS"
}
```

**Key Webhook Nuances:**
- `adjustment`: Sum of refunds, chargebacks, disputes, or manual adjustments netted against this settlement batch.
- Timestamp fields:
  - `settled_on`: Bank execution timestamp.
  - `settlement_initiated_on`: Batch initiation timestamp.
  *(Note: Webhooks do not carry a `transfer_time` field; that field exists exclusively on the order-level response).*
- `settlement_id` & `utr`: Can arrive as integer or string. Deserializer coerces both safely into canonical strings.
- Monetary values: Returned as float rupees (e.g. `97.94`, `100`, `1.75`, `0.31`, `0.00`). Parsed via `cashfree.ParseRupeesToPaise` (exact string-split arithmetic) into integer paise.

### B. Order-Level Settlement Endpoint (`GET /pg/orders/{order_id}/settlements`)
- **Endpoint**: `GET https://api.cashfree.com/pg/orders/{order_id}/settlements`
- **Headers**:
  - `x-client-id`: `<CASHFREE_PG_APP_ID>`
  - `x-client-secret`: `<CASHFREE_PG_SECRET_KEY>`
  - `x-api-version`: `2025-01-01`

**Response Structure Under `2025-01-01` (Flat Entity):**
```json
[
  {
    "cf_payment_id": "6183934088",
    "cf_settlement_id": "312048765",
    "settlement_currency": "INR",
    "order_id": "order_due_12345",
    "order_amount": 5500.00,
    "settlement_amount": 5393.80,
    "payment_time": "2025-01-15T10:24:37+05:30",
    "service_charge": 90.00,
    "service_tax": 16.20,
    "adjustment": 0.00,
    "settlement_id": "312048765",
    "transfer_id": "CB0312048765",
    "transfer_time": "2025-01-16T09:15:22+05:30",
    "status": "SUCCESS"
  }
]
```

**Key Endpoint Nuances:**
- `transfer_time`: Real field name in `2025-01-01` for the order-level settlement execution time (NOT `settled_at`).
- `adjustment`: Order-level deduction/addition if a partial refund or dispute was netted directly against the order.

### C. Cashfree Dashboard CSV Report Shapes
Cashfree provides two distinct CSV reports:
1. **Settlement Summary Export** (Dashboard > Payment Gateway > Settlements > Export):
   A batch-level summary with one row per settlement: `settlement_id`, `settlement_amount`, `gross_amount` (or `payment_amount`), `service_charge`, `service_tax`, `adjustment`, `transfer_utr`, `transfer_time`. This is the exact format parsed by `ParseAndImportSettlementCSV`.
2. **Settlement Recon Report** (Dashboard > Reports > Settlement Recon):
   A multi-tier transactional ledger report with event-level rows (`PAYMENT`, `REFUND`, `DISPUTE`, etc.) with `Sale Type: CREDIT / DEBIT`.
   *(Scope: Stream 3 Layer 1 implements the Settlement Summary CSV format, while multi-event recon summing is earmarked for Layer 3).*

---

## 3. Database Schema (Migration 026: `026_gateway_settlements_and_recon.sql`)

Create the durable ledger table for gateway settlements:

```sql
CREATE TABLE gateway_settlements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID REFERENCES properties(id) ON DELETE SET NULL,
    cf_settlement_id TEXT NOT NULL,
    order_id TEXT,                                      -- Nullable for aggregate batch webhooks
    cf_payment_id TEXT,                                 -- Gateway payment ID
    payment_intent_id UUID REFERENCES payment_intents(id) ON DELETE SET NULL,
    payment_id UUID REFERENCES payments(id) ON DELETE SET NULL,
    ingestion_source VARCHAR(20) NOT NULL DEFAULT 'webhook'
        CHECK (ingestion_source IN ('webhook', 'order_fetch', 'csv_import')),
    utr TEXT NOT NULL DEFAULT '',                       -- Bank reference / UTR / transfer_id
    currency VARCHAR(3) NOT NULL DEFAULT 'INR',
    gross_amount_paise BIGINT NOT NULL,                -- Payment amount collected from tenant
    service_charge_paise BIGINT NOT NULL DEFAULT 0,    -- Gateway TDR / processing fee
    service_tax_paise BIGINT NOT NULL DEFAULT 0,       -- GST on processing fee (18%)
    adjustment_paise BIGINT NOT NULL DEFAULT 0,        -- Refunds, chargebacks, dispute debits/credits netted
    net_amount_paise BIGINT NOT NULL,                  -- Actual amount credited to merchant bank
    settlement_status VARCHAR(30) NOT NULL,             -- SUCCESS, PENDING, FAILED, REVERSED
    settled_on TIMESTAMPTZ,                             -- Webhook batch bank execution timestamp
    settlement_initiated_on TIMESTAMPTZ,                -- Webhook batch initiation timestamp
    transfer_time TIMESTAMPTZ,                          -- Order-level transfer timestamp
    reconciliation_status VARCHAR(30) NOT NULL DEFAULT 'unmatched'
        CHECK (reconciliation_status IN ('matched', 'unmatched', 'discrepancy', 'manually_reconciled')),
    discrepancy_reason TEXT,                            -- arithmetic_imbalance, intent_not_found, amount_mismatch, payment_missing
    journal_entry_id UUID REFERENCES financial_journal_entries(id) ON DELETE SET NULL,
    resolution_notes TEXT,
    resolved_by UUID REFERENCES users(id) ON DELETE SET NULL,
    resolved_at TIMESTAMPTZ,
    raw_payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX uq_gateway_settlement_record 
    ON gateway_settlements (cf_settlement_id, COALESCE(order_id, ''), COALESCE(cf_payment_id, ''));

CREATE INDEX idx_gateway_settlements_prop_status 
    ON gateway_settlements (property_id, reconciliation_status);

CREATE INDEX idx_gateway_settlements_order_id 
    ON gateway_settlements (order_id) WHERE order_id IS NOT NULL;

CREATE INDEX idx_gateway_settlements_cf_settlement 
    ON gateway_settlements (cf_settlement_id);

CREATE INDEX idx_gateway_settlements_utr 
    ON gateway_settlements (utr) WHERE utr != '';
```

---

## 4. Double-Entry Balance Proof & Conservation Invariant

### A. Mathematical Conservation Law
Cashfree's net settlement formula accounts for processing fees and adjustments:
$$\text{GrossAmountPaise} == \text{NetAmountPaise} + \text{ServiceChargePaise} + \text{ServiceTaxPaise} + \text{AdjustmentPaise}$$

Equivalently:
$$\text{NetAmountPaise} = \text{GrossAmountPaise} - (\text{ServiceChargePaise} + \text{ServiceTaxPaise}) - \text{AdjustmentPaise}$$

### B. Double-Entry Journal Specification
When posting the balanced settlement journal:
- **Debit**: `AcctBank` (`NetAmountPaise`) — cash credited to merchant account
- **Debit**: `AcctPaymentProcessingExpense` (`ServiceChargePaise + ServiceTaxPaise`) — gateway MDR fee + GST
- **If $\text{AdjustmentPaise} > 0$**:
  - **Debit**: `AcctGatewayAdjustment` (`AdjustmentPaise`) — represents funds retained by Cashfree to settle prior refunds or disputes
- **If $\text{AdjustmentPaise} < 0$**:
  - **Credit**: `AcctGatewayAdjustment` (`-\text{AdjustmentPaise}`) — represents net credit adjustment added by gateway
- **Credit**: `AcctGatewayClearing` (`GrossAmountPaise`) — clears the provisional tenant collection from `MirrorPayment`

**Proof of Equality**:
$$\sum \text{Debits} = \text{NetAmountPaise} + \text{ServiceChargePaise} + \text{ServiceTaxPaise} + \text{AdjustmentPaise} = \text{GrossAmountPaise} = \sum \text{Credits}$$
Balance holds strictly for all cases, including settlements with refunds or chargeback adjustments.

---

## 5. Architectural Contracts & Layer Responsibilities

### A. Pure Accounting Posting (`ProcessSettlement` in `internal/finance/settlement.go`)
- `ProcessSettlement` remains a pure accounting function.
- It validates the conservation equation $\text{Gross} == \text{Net} + \text{Fee} + \text{Tax} + \text{Adjustment}$.
- If balanced: posts deterministic journal lines (`source_id = "cashfree_settlement:<settlement_id>"`) and returns `nil`.
- If unbalanced or invalid: returns typed errors (`ErrSettlementUnbalanced`, `ErrInvalidSettlementData`).
- **No side-effects on error**: It never writes partial or speculative rows to the database.

### B. Ingress Caller Responsibilities (Discrepancy Persistence & Parity)
The caller layer (`WebhookHandler`, `OrderReconciler`, or `CSVImportHandler`) owns database persistence:
1. **Durable Ingestion Row**: Before or alongside posting, the caller creates/updates the record in `gateway_settlements` with `ingestion_source` (`webhook`, `order_fetch`, or `csv_import`).
2. **Success Path**: On successful journal posting, the caller links `journal_entry_id` and sets `reconciliation_status = 'matched'`.
3. **Discrepancy Path**: When `ProcessSettlement` or intent matching fails, the caller catches the error and persists:
   - `reconciliation_status = 'discrepancy'`
   - `discrepancy_reason = err.Error()`
   - `journal_entry_id = NULL`
   This guarantees that no failed settlement vanishes into logs; every rejected or imbalanced settlement immediately surfaces in the property owner's exception queue.

### C. CSV Column Mapping & Alias Support
In `ParseAndImportSettlementCSV`, the parser maps columns flexibly:
- `settlement_id`: `settlement_id`, `id`
- `gross_amount`: `gross_amount`, `total_transaction_amount`, `payment_amount`
- `settlement_amount`: `settlement_amount`, `amount_settled`
- `service_charge`: `service_charge`, `fee`
- `service_tax`: `service_tax`, `tax`
- `adjustment`: `adjustment`, `other_deductions` (defaults to 0 if column absent)
- `transfer_utr`: `transfer_utr`, `settlement_utr`, `utr`
- `transfer_time`: `transfer_time`, `settlement_date`

---

## 6. Verification Plan

1. **Unit Tests (`internal/cashfree/settlement_test.go`)**:
   - Verify parsing of `2025-01-01` flat JSON schema with `transfer_time` and `adjustment`.
   - Verify parsing of webhook payload with `settled_on`, `settlement_initiated_on`, and `adjustment`.
   - Verify numeric vs string coercion for `settlement_id` and `utr`.
   - Verify exact integer paise conversions for decimal fees and adjustments.
2. **Reconciliation Invariant Tests (`internal/finance/settlement_recon_test.go`)**:
   - Verify conservation with non-zero adjustments ($\text{Gross} == \text{Net} + \text{Fee} + \text{Tax} + \text{Adjustment}$).
   - Verify caller discrepancy capture on intentional 1-paise arithmetic imbalances.
   - Verify idempotent reprocessing of already-settled records (CSV vs Webhook race).
3. **HTTP & Live DB Tests (`internal/api/handlers_settlement_test.go`, `internal/postgres/settlement_repo_live_test.go`)**:
   - Verify webhook ingress signature validation and idempotent deduplication.
   - Verify CSV upload endpoint (`POST /api/owner/settlements/import-csv`) populates `gateway_settlements` with `ingestion_source = 'csv_import'`.
   - Verify IDOR isolation across properties on owner settlement query endpoints.
   - Full test suite sweep: `go test -count=1 ./internal/...`.
