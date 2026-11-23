# M3 — KYC

**Current product policy (ADR-005):** OCR + UIDAI Secure QR + manual owner review. DigiLocker and Aadhaar OTP are disabled by default. KYC does not block read-only tenant access to room, dues, or payment history; verified identity is required at payment dispatch.


**Sprint:** 8 · **ADR:** ADR-004, ADR-011

## Gate 8

- [x] Tenant KYC routes and jobs: document OCR, Secure QR verification, manual review, consent, audit, and expiry. DigiLocker is opt-in and defaults off; Aadhaar OTP endpoints return 410 by PG Cashflow product policy.
- [ ] R9-F7 identity projection audit
