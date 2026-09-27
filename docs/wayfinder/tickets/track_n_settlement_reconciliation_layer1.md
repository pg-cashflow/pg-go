# Ticket 13: Track N — Stream 3 Layer 1: Cashfree Settlement Ingress & Order-to-Intent Reconciliation

- **Type**: `wayfinder:task`
- **Status**: Draft (Updated with Cashfree Ground Truth Corrections)
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Prerequisite**: Track L (Payouts Transfers V2) & Track M (Staff Attendance & Wage Engine) committed and verified.

---

## 1. Objective

Implement **Stream 3 Layer 1 (Cashfree Settlement Ingress & Order-to-Intent Reconciliation)**:
1. Ingest Cashfree Payment Gateway settlement data via both **real-time webhooks** (`SETTLEMENT_SUCCESS`, `SETTLEMENT_FAILED`, `SETTLEMENT_REVERSED`) and **on-demand order-level API polling** (`GET /pg/orders/{order_id}/settlements`).
2. Reconcile settled funds against internal `payment_intents` and `payments` to verify gross amounts, quantify gateway MDR deductions (`service_charge`), GST (`service_tax`), and net adjustments (`adjustment` for refunds/chargebacks/disputes), and capture bank UTRs.
3. Post balanced double-entry financial journals moving funds from `gateway_clearing` to `bank` with explicit recognition of `payment_processing_expense` and `gateway_adjustment`, maintaining $\sum\text{Debits} == \sum\text{Credits}$ without float drift.
4. Provide human-gated exception handling for reconciliation discrepancies (timing lags, MDR mismatches, missing orders, unallocated adjustments).

---

## 2. Grounded Cashfree Settlement Payload Analysis (`2025-01-01` Ground Truth)

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
  - `settled_on`: Bank acknowledgment/execution timestamp.
  - `settlement_initiated_on`: Batch initiation timestamp.
  *(Note: Webhooks have no `transfer_time` field; that field exists exclusively on the order-level response).*
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

**Forward Compatibility Note (`2026-01-01` Nested Entity):**
In `2026-01-01`, Cashfree nested these properties under `order_details`, `payment_details`, and `settlement_details`. Our deserializer will inspect both flat top-level fields and nested sub-objects to remain forward-resilient.

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
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_gateway_settlement_record UNIQUE (cf_settlement_id, COALESCE(order_id, ''), COALESCE(cf_payment_id, ''))
);

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

## 5. Architecture & Go Types

### A. Ingress Go Models (`internal/cashfree/settlement.go`)

```go
// OrderSettlementRecord represents an order-level settlement from GET /pg/orders/{order_id}/settlements
type OrderSettlementRecord struct {
    CFSettlementID      string    `json:"cf_settlement_id"`
    CFPaymentID         string    `json:"cf_payment_id"`
    OrderID             string    `json:"order_id"`
    GrossAmountPaise    int64     `json:"gross_amount_paise"`
    NetAmountPaise      int64     `json:"net_amount_paise"`
    ServiceChargePaise  int64     `json:"service_charge_paise"`
    ServiceTaxPaise     int64     `json:"service_tax_paise"`
    AdjustmentPaise     int64     `json:"adjustment_paise"`
    UTR                 string    `json:"transfer_id"`
    TransferTime        time.Time `json:"transfer_time"`
    Status              string    `json:"status"`
}

// SettlementWebhookPayload represents SETTLEMENT_SUCCESS from Cashfree webhook
type SettlementWebhookPayload struct {
    Data struct {
        Settlement struct {
            SettlementID          any     `json:"settlement_id"` // string or int64
            Status                string  `json:"status"`
            AmountSettled         float64 `json:"amount_settled"`
            UTR                   any     `json:"utr"`           // string or int64
            SettledOn             string  `json:"settled_on"`
            SettlementType        string  `json:"settlement_type"`
            PaymentAmount         float64 `json:"payment_amount"`
            ServiceCharge         float64 `json:"service_charge"`
            ServiceTax            float64 `json:"service_tax"`
            Adjustment            float64 `json:"adjustment"`
            SettlementInitiatedOn string  `json:"settlement_initiated_on"`
        } `json:"settlement"`
    } `json:"data"`
    EventTime string `json:"event_time"`
    Type      string `json:"type"`
}
```

### B. Reconciliation Engine (`internal/finance/settlement_recon.go`)
- **Matching Algorithm**:
  1. Look up `payment_intent` by `order_id = rec.OrderID` (or search `payments` by `cf_payment_id = rec.CFPaymentID`).
  2. If intent not found:
     - Set `reconciliation_status = 'unmatched'`, `discrepancy_reason = 'intent_not_found'`. Zero ledger mutation.
  3. Validate Gross Amount:
     - Assert `rec.GrossAmountPaise == intent.AmountPaise`.
     - If mismatch: set `reconciliation_status = 'discrepancy'`, `discrepancy_reason = 'intent_amount_mismatch'`. Zero ledger mutation.
  4. Validate Invariant Equation:
     - Assert `rec.GrossAmountPaise == rec.NetAmountPaise + rec.ServiceChargePaise + rec.ServiceTaxPaise + rec.AdjustmentPaise`.
     - If mismatch: set `reconciliation_status = 'discrepancy'`, `discrepancy_reason = 'arithmetic_imbalance'`. Zero ledger mutation.
  5. Validate Associated Internal Payment:
     - Locate `payments` record for this intent/order, link `payment_id`.
  6. Atomic Double-Entry Posting:
     - On `matched`, insert balanced journal entry (`AcctBank`, `AcctPaymentProcessingExpense`, `AcctGatewayAdjustment`, `AcctGatewayClearing`).
     - Update `reconciliation_status = 'matched'` and link `journal_entry_id`.

---

## 6. Verification Plan

1. **Unit Tests (`internal/cashfree/settlement_test.go`)**:
   - Verify parsing of `2025-01-01` flat JSON schema with `transfer_time` and `adjustment`.
   - Verify parsing of webhook payload with `settled_on`, `settlement_initiated_on`, and `adjustment`.
   - Verify numeric vs string coercion for `settlement_id` and `utr`.
   - Verify exact integer paise conversions for decimal fees and adjustments.
2. **Reconciliation Invariant Tests (`internal/finance/settlement_recon_test.go`)**:
   - Verify conservation with non-zero adjustments ($\text{Gross} == \text{Net} + \text{Fee} + \text{Tax} + \text{Adjustment}$).
   - Verify fail-closed behavior on intentional 1-paise arithmetic imbalances.
   - Verify idempotent reprocessing of already-settled records.
3. **HTTP & Live DB Tests (`internal/api/handlers_settlement_test.go`, `internal/postgres/settlement_repo_live_test.go`)**:
   - Verify webhook ingress signature validation and idempotent deduplication.
   - Verify IDOR isolation across properties on owner settlement query endpoints.
   - Full test suite sweep: `go test -count=1 ./internal/...`.
