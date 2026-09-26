# Ticket 4a: Payout Batch Dual-Control & Maker-Checker Gating

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Objective

Remediate maker-checker absence in `internal/postgres/payout_repo.go` (`CreateBatchFromUnbatchedItems`) and `internal/api/handlers_payouts.go`:
1. Prevent batches from being auto-approved by the creator at creation time. Batches must be created in `domain.BatchDraft` status with `ApprovedBy: nil` and `ApprovedAt: nil`.
2. Implement dedicated approval endpoint `POST /owner/payouts/batches/:id/approve` requiring affirmative re-acknowledgment of `expected_item_count` and `expected_total_paise`.
3. Enforce dual control:
   - When $> 1$ active owner exists for the property, the approver MUST be distinct from the creator (`claims.UserID != batch.CreatedBy`).
   - When $\le 1$ active owner exists (solo owner, or second owner removed), require step-up affirmative re-authentication (`reauth_confirmation`) to prevent single-click execution or session hijacking.
4. Gate `GET /owner/payouts/batches/:id/export` strictly on `batch.Status == domain.BatchApproved` (rejecting draft batches with `409 Conflict`).

## Implementation Details

1. **Batch Lifecycle**: Updated `internal/postgres/payout_repo.go` (`CreateBatchFromUnbatchedItems`) so new batches start in `domain.BatchDraft` with `ApprovedBy: nil` and `ApprovedAt: nil`. Added `ApproveBatch(ctx, batchID, approverID)` updating status to `domain.BatchApproved`.
2. **Affirmative Verification & Dual-Control Gating**: Added `OwnerApprovePayoutBatch` (`POST /owner/payouts/batches/:id/approve`) in `internal/api/handlers_payouts.go`:
   - Enforces property match (`batch.PropertyID == pid`).
   - Rejects non-draft batches (`409 Conflict`).
   - Validates affirmative re-acknowledgment (`body.ExpectedItemCount == batch.ItemCount` and `body.ExpectedTotalPaise == batch.TotalAmountPaise`, returning `400 Bad Request` on mismatch).
   - In multi-owner properties (`len(activeOwners) > 1`), rejects creator approvals (`claims.UserID == batch.CreatedBy`) with `403 Forbidden` ("dual control required").
   - In solo-owner properties (or if second owner was removed), requires step-up reauthentication (`reauth_confirmation`), logging `approval_mode: "solo_owner_reauth"`.
3. **Export Gate**: Updated `OwnerExportPayoutBatch` (`GET /owner/payouts/batches/:id/export`) to strictly require `batch.Status == domain.BatchApproved`, rejecting draft batches with `409 Conflict`.
4. **Router Registration**: Registered `owner.POST("/payouts/batches/:id/approve", h.OwnerApprovePayoutBatch)` in `internal/api/router.go`.
5. **Tests**: Added full coverage in `internal/api/handlers_payouts_test.go`:
   - Draft export returns 409 Conflict.
   - Affirmative count/paise mismatch returns 400 Bad Request.
   - Missing solo-owner reauth returns 401 Unauthorized.
   - Solo-owner valid reauth approval returns 200 OK.
   - Post-approval export returns 200 OK with sanitized CSV and HMAC checksum header.
   - Multi-owner dual-control: maker approval rejected with 403 Forbidden.
   - Multi-owner dual-control: second owner approval succeeds (200 OK, `approval_mode: "dual_control"`).
   - Fallback test: when second owner is removed, creator can approve via step-up reauth without bricking the batch.
