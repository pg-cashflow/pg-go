# ADR 005: Owner Payment Enablement — Collection Mode Switch & Screenshot Proof Verification

## Status
Superseded by Day-0 Business Registration Workstream & ADR 006
**Date:** 2026-09-25

> [!WARNING]
> **SUPERSEDED NOTICE**: The earlier consideration of onboarding as an "Individual Merchant with T+2 settlement into a personal savings account" has been **formally rejected** by Cashfree underwriting policy for PG/hostel commercial accommodation collections. Production gateway onboarding requires an explicit **Day-0 Business Registration Workstream** (online Sole Proprietorship registration via Udyam at `udyamregistration.gov.in` + dedicated Current Account opened in the business entity name) running in parallel with sandbox development.
> This ADR is preserved for historical context regarding offline P2P UPI screenshot verification (`manual_proof`) fallback during initial pre-gateway onboarding.

## Context
Cashfree Payment Gateway underwriting categorically rejects accommodation and PG/hostel collections into personal savings accounts without business registration. This blocks automated online payment collection through personal merchant profiles, but does not block direct tenant-to-owner P2P payments (UPI).

To enable operations immediately while working through merchant re-classification:
1. Tenants pay the property owner's UPI ID directly outside the platform.
2. Tenants upload proof of payment (screenshot) against their due.
3. The platform assists data entry and validates proof against common fraud patterns (e.g. image reuse).
4. Once an owner's gateway merchant account is active, an admin can switch collection mode to automated gateway checkout.

Payment screenshots are trivially fabricated and cannot be cryptographically verified. Automatic status transitions based purely on uploaded images create systemic fraud risk.

## Decisions

1. **Explicit Collection Mode State Machine**:
   - `properties.payment_collection_mode` VARCHAR(20) DEFAULT `'manual_proof'` with check constraint (`'manual_proof'`, `'gateway'`).
   - Admin-gated activation: Flipping to `'gateway'` is an explicit admin action. Columns `gateway_enabled_at` and `gateway_sub_merchant_id` are provisioned on `properties` as forward-compatibility placeholders for when Cashfree PG is unlocked, but are not actively used in Phase 1.

2. **Leverage Existing `payment_reports` Architecture**:
   - The platform already possesses `payment_reports` (`POST /api/tenant/dues/:id/reports` and `POST /api/owner/payment-reports/:id/confirm`).
   - Enhance `payment_reports` with:
     - `image_hash CHAR(64)`: SHA-256 of uploaded receipt image.
     - `is_duplicate BOOLEAN NOT NULL DEFAULT false`: **Flag, not hard-block.** If an image hash matches a prior submission, the report is accepted into `pending_review` with `is_duplicate = true` and a review badge for the owner to eyeball. This avoids customer friction on legitimate re-uploads (e.g. fixing a typo in reported amount).
     - `ocr_amount`, `ocr_utr`, `ocr_txn_date`, and `ocr_confidence`: populated asynchronously or via an OCR sidecar to pre-fill owner confirmation.

3. **Owner Confirmation as Sole Truth Anchor**:
   - OCR and image hashing pre-fill confirmation inputs and flag anomalies; they never mark a due paid automatically.
   - Owner confirmation (`POST /owner/payment-reports/:id/confirm`) triggers `payment.ManualMatch` and marks the due settled.

4. **Multi-Owner Settlement Roadmap (Phase 3)**:
   - Future automated marketplace payouts will use Cashfree Easy Split (vendor onboarding per owner, split settlement), which requires an approved Payment Gateway merchant account.

## Consequences
- Immediate unblocking: Tenants can onboard and pay owners today without waiting on Cashfree Payment Gateway approval.
- Fraud defense: Hash checking detects recycled receipts while allowing legitimate tenant re-submissions.
- Low-risk rollout: Additive schema migration on already tested `payment_reports` table.
