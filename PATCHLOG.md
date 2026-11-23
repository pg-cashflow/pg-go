# PG Cashflow Backend — Patch 1

Date: 2026-11-22

This patch is applied to the extracted working copy of the uploaded backend repository. It is not a database deployment and does not change any live environment.

## Changes

- Add a read-only platform-operations SELECT policy for `payout_payees` (`migrations/121_platform_ops_payout_payees_read.sql`).
- Enforce owner manager-refund delegation and per-request limit inside refund approval request creation. Preserve the current user -> capability -> property policy lock order.
- Wire `single_owner_refund_threshold_paise` through owner onboarding settings GET/PATCH and repository persistence.
- Default DigiLocker off in backend configuration and both `.env.example` and `deploy/.env.example`.
- Add ADR-005 for OCR + UIDAI Secure QR + manual review, and mark the previous DigiLocker-first ADR as superseded.
- Correct unsupported comments that stated Cashfree had discontinued Aadhaar OTP. PG Cashflow disables OTP by product policy.
- Return HTTP 503 when a signed KYC webhook cannot be processed because the KYC service is unavailable. Add a regression test.
- Include the end-to-end audit report under `docs/audits/`.

## Verification completed

- Go formatting: `gofmt` reports no formatting issues in changed Go files.
- Frontend static verification across the paired repository: 1,035 locale keys; 100% parity for te-IN, ta-IN, kn-IN; 91 exact backend/frontend error-code matches; palette, typography, policy parity, company metadata checks pass; company tests 7/7 pass.

## Verification not completed

- Go build/tests: repository requires Go 1.26.0; the available local toolchain is Go 1.23.2 and downloading 1.26.0 failed because this environment could not resolve the Go proxy.
- TypeScript, Vite, Vitest, and browser E2E: `node_modules` is not installed and package cache is empty.
- PostgreSQL migration/RLS tests using `pgapp_app`, `pgapp_worker`, and maintenance roles.
- Cashfree sandbox, webhook delivery/replay, refund recovery, payout batch reconciliation, and bank evidence.

## Open release blockers

- Payment-intent `dispatch_unknown` / `superseded` recovery can release an uncertain provider order. The quarantine table has no live application workflow. This is intentionally not changed without a provider reconciliation/backfill design.
- `refund_approval_requests` has no forced property RLS. Repository method and transaction scoping must be refactored together before adding the policy.
- Migration 121 and refund policy changes require live-role integration verification.

**Deployment remains on hold.** Do not apply migrations or deploy these changes without a reviewed migration plan, backup, staging validation, and the missing test evidence.
