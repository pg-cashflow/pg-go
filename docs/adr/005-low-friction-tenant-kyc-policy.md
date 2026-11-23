# ADR-005: Low-friction tenant KYC policy

## Status
Accepted — 2026-11-22. Supersedes ADR-004 where the two conflict.

## Context
PG Cashflow needs a verifiable tenant identity workflow without adding a full-screen onboarding gate or requiring every tenant to have a mobile number linked to Aadhaar. The previous DigiLocker-first decision is not the current product direction.

## Decision

1. Use **Cashfree document OCR** to extract and prefill identity data. OCR output is evidence for review; it must not auto-approve a tenant.
2. Support **UIDAI Secure QR** where the uploaded document contains a valid QR payload. Verify the signature locally against the configured UIDAI public key. A missing or invalid key must fail closed for QR verification.
3. Keep **manual owner review** as the approval step. A reviewer must see the submitted document and OCR/QR evidence, and record an explicit decision.
4. Keep **DigiLocker disabled by default**. Enable it only after a separate product, privacy, provider-contract, and end-to-end review. The backend environment default and frontend capability handling must both fail closed.
5. Keep **Aadhaar OTP disabled in PG Cashflow**. Existing endpoints return `410 Gone`. This is a product and accessibility decision, not a claim that Cashfree or UIDAI has discontinued the service.
6. Do not block read-only tenant access to room details, dues, payment history, or the dashboard while KYC is pending. Require verified KYC at the financial action boundary before payment dispatch.
7. Before verification, obtain explicit consent. Preserve the existing data-minimization controls: do not log raw Aadhaar values; purge raw source bytes after the approved workflow; retain only the minimum masked identifier and keyed deduplication material required by the approved retention policy. Consent revocation must atomically clear retained Aadhaar-derived personal data.

## Operational requirements

- KYC callbacks must be authenticated against the raw request bytes and processed idempotently. Do not return success when the KYC service is unavailable and the event has not been persisted or processed.
- A reviewer must not approve solely from OCR output.
- Secure QR certificate rotation must have an owned operational check and a current official test fixture.
- KYC status changes must be audited and must not directly mutate payment or ledger state outside the authorized service path.
- Production enablement requires sandbox and live verification evidence, webhook replay tests, consent-revocation tests, and confirmation that payment dispatch rejects an unverified tenant.

## Consequences

The normal tenant journey has fewer redirects and does not prevent a tenant from viewing their account while identity review is pending. Some tenants need manual review. This is an intentional trade-off for accessibility, low friction, and a clear human approval boundary.
