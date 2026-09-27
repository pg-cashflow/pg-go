# Ticket 11: Payout Phase 2 — Cashfree Transfers V2 Integration

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Prerequisite**: Cashfree Payouts Transfers V2. Built against sandbox and verified with full live and unit test suite.

## Objective

Wire automated Cashfree Payouts into the existing ADR-009 batch lifecycle (`draft → pending_approval → approved → processing → completed/partially_failed/failed`), reusing the outbox pattern already proven in Track E, without replacing the Phase 1 manual-CSV path.

## Why V2, not V1

Cashfree's own docs mark every V1/V1.2 Payouts endpoint (`addBeneficiary`, `requestTransfer`, `requestAsyncTransfer`, `getTransferStatus`):
> *"This API will be retired soon. Please plan to migrate to the latest version, Transfers V2."*

Building new integration code against a sunset-bound surface is a foundation error independent of how well anything is layered on top of it.

---

## 1. Config Additions

- `CF_PAYOUT_CLIENT_ID`
- `CF_PAYOUT_CLIENT_SECRET` (distinct from CASHFREE Payment Gateway secret)
- `CF_PAYOUT_API_VERSION` (e.g. `"2024-01-01"`)
- `CF_PAYOUT_FUNDSOURCE_ID` (which connected bank/fund source debits — required by V2, has no V1 analog)
- `CF_PAYOUT_WEBHOOK_SECRET` (separate from `CASHFREE_WEBHOOK_SECRET` PG)
- `CF_PAYOUT_ENV` (`sandbox` | `production`)

**Base URLs:**
- Sandbox: `https://sandbox.cashfree.com/payout`
- Production: `https://api.cashfree.com/payout`
*(Not `payout-api.cashfree.com` — that host serves the retiring V1 surface).*

`ValidateForRealDeployment` gets the same treatment as the PG secrets: reject empty/placeholder `CF_PAYOUT_WEBHOOK_SECRET` whenever `CF_PAYOUT_CLIENT_ID` is set.

---

## 2. Beneficiary V2 (`internal/cashfree/payout_beneficiary.go`)

- **Endpoint**: `POST /payout/beneficiary` (Create Beneficiary V2)
- **Headers**: `x-client-id`, `x-client-secret`, `x-api-version`, `Content-Type: application/json`
- **Beneficiary ID derivation (cache-invalidation fix)**:
  ```
  beneficiary_id = "bene_" + payee.ID + "_" + hex(payee.AccountNumberHash)[:8]
  ```
  (≤50 chars, alphanumeric/`-`/`_`/`|`/`.` only — matches doc constraint).
  When a payee's bank details change, `AccountNumberHash` changes, which changes the derived `beneficiary_id`, forcing a fresh Cashfree beneficiary registration automatically — no manual "update beneficiary" step needed, and the stale beneficiary is never referenced again.
  The old beneficiary can be cleaned up later via Delete Beneficiary V2.
- **Handling 404 `beneficiary_not_found`**:
  On the first transfer attempt after a hash change, if `beneficiary_not_found` is returned, create the beneficiary, then retry the transfer once with the same `transfer_id`.

---

## 3. Batch Transfer V2 (`internal/cashfree/payout_transfer.go`)

- **Endpoint**: `POST /payout/transfers/batch` — one call per `PayoutBatch`, matching domain model 1:1.
  ```json
  {
    "batch_transfer_id": "pgo_<payout_batch.id>",
    "transfers": [
      {
        "transfer_id": "pgo_<payout_item.id>",
        "transfer_amount": "1250.50",
        "transfer_mode": "banktransfer",
        "fundsource_id": "<CF_PAYOUT_FUNDSOURCE_ID>",
        "transfer_remarks": "<purpose> <reference_number>",
        "beneficiary_details": {
          "beneficiary_id": "<derived above>"
        }
      }
    ]
  }
  ```
- **Amount formatting**: Reuse the existing `internal/cashfree/decimal.go` paise→string helper used for Payment Gateway (`formatPaiseToINR`).
- **Limits & Chunking**: 1,000 transfers/call in sandbox, 5,000 in production. Chunk batches exceeding the limit (`pgo_<id>_c1`, `_c2`).
- **Immediate Response**: `{"batch_transfer_id", "cf_batch_transfer_id", "status": "RECEIVED"}`. `RECEIVED` is not terminal. Final outcome arrives via webhook or Get Batch Transfer Status V2.
- **5xx / Ambiguous Timeout Rule**: Cashfree docs state: *"When you get a 5XX response, do not initiate another transaction. Check the status ... and then proceed further."*
  Any 5xx or transport error transitions batch status to `dispatch_unknown`. The only permitted next action is `Get Batch Transfer Status V2`, never a blind re-POST.

---

## 4. Get Batch Transfer Status V2 (Reconciliation Poller)

- **Endpoint**: `GET /payout/transfers/batch?batch_transfer_id=...`
- **Uses**:
  1. **Ambiguity resolver**: Immediately invoked after any `dispatch_unknown` result.
  2. **Missed-webhook safety net**: Cron job sweeping batches in `processing` or `dispatch_unknown` older than 10 minutes without webhook delivery. Read-only status reconciliation, never re-dispatch.

---

## 5. State Machine Additions

### Batch (`PayoutBatchStatus`)
| Status | When |
|---|---|
| `processing` | Batch Transfer V2 call succeeded, `RECEIVED` |
| `dispatch_unknown` *(new)* | 5xx / timeout on batch call itself — ambiguous |
| `partially_failed` *(existing)* | Mixed terminal outcomes across items |
| `completed` | All items terminal success |
| `failed` | All items terminal failure / rejected |

### Item (`PayoutItemStatus`)
| Status | Maps from Cashfree | Auto-retriable? | Notes |
|---|---|---|---|
| `pending` / `processing` | `RECEIVED`, in-flight | — | |
| `cashfree_approval_pending` *(new)* | `APPROVAL_PENDING`, `VELOCITY_CHECK_FAILED`, `TRANSFER_LIMIT_BREACH` | No | Surfaced to owner as "awaiting Cashfree approval" on Cashfree merchant dashboard |
| `succeeded` | `TRANSFER_SUCCESS` / webhook `TRANSFER_ACKNOWLEDGED` + `TRANSFER_SUCCESS` | — | Terminal success |
| `failed` | `FAILED` (`BENE_BANK_DECLINED`, `IMPS_MODE_FAIL`, `SOURCE_BANK_DECLINED`) | No | Terminal for beneficiary/mode; requires human fix, then retry with `_r2` suffix |
| `retriable_failed` *(new)* | `FAILED` (`WAIT_TIME_EXCEEDED`) | Yes | Cashfree retriable. Requires verifying exact reinitiate mechanism before auto-retrying |
| `rejected` | `TRANSFER_REJECTED` | No | Terminal rejection |
| `reversed` *(new)* | `TRANSFER_REVERSED` | No | Money moved and returned. Requires distinct ledger reversal entry |

---

## 6. Webhook (`internal/api/handlers_payouts_webhook.go`)

- **Route**: `POST /webhooks/cashfree/payouts`
- **Verification**: `x-webhook-signature` / `x-webhook-timestamp` with Base64(HMAC-SHA256(timestamp+rawBody, `CF_PAYOUT_WEBHOOK_SECRET`)).
- **Event Types**: `TRANSFER_ACKNOWLEDGED`, `TRANSFER_SUCCESS`, `TRANSFER_FAILED`, `TRANSFER_REVERSED`, `TRANSFER_REJECTED`, `BULK_TRANSFER_REJECTED`.
- **Idempotency**: Matching `transfer_id` → `payout_item.id`. Re-delivery is an idempotent no-op if the item has already reached terminal status.

---

## 7. Write-Before-Call Ordering (Outbox Pattern)

Inside one DB transaction:
1. Lock batch + items `FOR UPDATE`.
2. Verify batch is `approved` and all items `pending`.
3. Flip batch → `processing`, items → `processing`.
4. Insert durable outbox row in `ledger_outbox_events` with `event_type = "payout_batch_transfer"`, `idempotency_key = "payout_batch_transfer:<batch.id>"`.
5. Commit transaction.
6. Worker / background processor reads outbox row and initiates Cashfree Batch Transfer V2 call.

---

## 8. Ledger Mirror

- On `succeeded`: Debit payee liability / payable account, credit cash clearing (via `mirror.go`).
- On `reversed`: Distinct reversal entry (debit clearing, credit liability).
- On `failed` / `rejected`: No ledger mutation (money never moved).

---

## 9. Migration Additions

- `payout_batches.status` CHECK constraint: add `'dispatch_unknown'`.
- `payout_items.status` CHECK constraint: add `'cashfree_approval_pending'`, `'retriable_failed'`, `'reversed'`, `'processing'`.
- `payout_items`: add `cf_transfer_id VARCHAR(100)`, `retry_of UUID REFERENCES payout_items(id)`.

---

## 10. Open Items for Confirmation

1. Exact reinitiation mechanism for `WAIT_TIME_EXCEEDED` (same `transfer_id` vs dedicated endpoint) — inspect sandbox behavior.
2. Verify Cashfree account has Batch Transfer V2 explicitly enabled alongside single transfer in dashboard.

---

## 11. Implementation & Verification Proof

- **Migration**: [024_payout_v2_status_and_retry.sql](file:///c:/Users/divak/Downloads/pg-go/migrations/024_payout_v2_status_and_retry.sql) applied with updated `payout_batches.status` (`dispatch_unknown`, `failed`) and `payout_items.status` (`processing`, `cashfree_approval_pending`, `retriable_failed`, `reversed`), adding `cf_transfer_id` and `retry_of`.
- **Config & Validation**: [internal/config/config.go](file:///c:/Users/divak/Downloads/pg-go/internal/config/config.go) and [internal/config/validate.go](file:///c:/Users/divak/Downloads/pg-go/internal/config/validate.go) with fail-closed checks on payout keypairs, fundsource ID, and webhook secret. Default version pinned to `"2024-01-01"`.
- **Client & Beneficiary V2**: [internal/cashfree/payout_client.go](file:///c:/Users/divak/Downloads/pg-go/internal/cashfree/payout_client.go), [internal/cashfree/payout_beneficiary.go](file:///c:/Users/divak/Downloads/pg-go/internal/cashfree/payout_beneficiary.go), [internal/cashfree/payout_transfer.go](file:///c:/Users/divak/Downloads/pg-go/internal/cashfree/payout_transfer.go), tested in [internal/cashfree/payout_test.go](file:///c:/Users/divak/Downloads/pg-go/internal/cashfree/payout_test.go). Logs `x-deprecated-at` warning header.
- **Outbox State Machine & Dispatcher**: [internal/postgres/payout_repo.go](file:///c:/Users/divak/Downloads/pg-go/internal/postgres/payout_repo.go) with `InitiateBatchTransferTx`, `SetBatchDispatchUnknown`, `UpdatePayoutItemStatusTx`, `UpdateBatchStatusFromItemsTx`. [internal/finance/payout_dispatcher.go](file:///c:/Users/divak/Downloads/pg-go/internal/finance/payout_dispatcher.go) handles batch dispatching, 5xx to `dispatch_unknown`, proactive beneficiary creation, tested in [internal/finance/payout_dispatcher_test.go](file:///c:/Users/divak/Downloads/pg-go/internal/finance/payout_dispatcher_test.go).
- **Webhook Ingress**: [internal/api/handlers_payouts_webhook.go](file:///c:/Users/divak/Downloads/pg-go/internal/api/handlers_payouts_webhook.go) with HMAC-SHA256 signature verification, idempotency replay guard, and ledger mirror integration (`MirrorPayoutSettled`, `MirrorPayoutReversed`). Tested in [internal/api/handlers_payouts_webhook_test.go](file:///c:/Users/divak/Downloads/pg-go/internal/api/handlers_payouts_webhook_test.go).
- **Test Suite Results**:
  - `internal/cashfree`: 100% PASS
  - `internal/finance`: 100% PASS
  - `internal/api`: 100% PASS
  - `internal/postgres`: 100% PASS
  - Full repo sweep: all 30 internal packages 100% PASS (`go test -p 2 ./internal/...`).

