# Ticket 30: Track R.14 — Gate 14: Privacy, DPDP PII Masking & Log Leak Guards

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: None
- **Tier**: PR Gate (< 10 min) + Weekly Audit

## Objective

Ensure strict compliance with India's Digital Personal Data Protection (DPDP) Act and UIDAI Aadhaar regulations.
Verify that application logs, HTTP error responses, and database queries never expose unmasked 12-digit Aadhaar numbers or raw biometric data.
Assert that tenant phone numbers and bank account numbers are masked in non-administrative views.

## Regulatory Invariants

1. **Zero Raw Aadhaar Storage**: The 12-digit Aadhaar number must never be stored in plain text anywhere in PostgreSQL.
2. **Zero Aadhaar Logging**: Application logs (`slog`), tracing spans, and error strings must never contain a 12-digit number matching Verhoeff checksum.
3. **Fail-Closed PII Masking**: API endpoints returning tenant data must display masked phone numbers (`******1234`) unless caller has verified administrative scope.
4. **DPDP Revocation Cascade**: When a tenant revokes consent, all cached KYC files and PII artifacts must be wiped atomically.

## Karpathy Test Discipline

1. **Look at data first:**
   Inspect log outputs from `internal/aadhaar`, `internal/kyc`, and `internal/api`.
   Check database tables `kyc_documents` and `tenants`.

2. **Check state at initialization:**
   Configure a test log sink capturing all structured log lines emitted during test runs.

3. **Overfit one example:**
   Send an Aadhaar verification payload containing test Aadhaar `999999999999`.
   Assert that neither response nor logs contain the raw 12 digits.
   Assert only the masked representation `XXXXXXXX9999` appears.

4. **Compare with dumb baseline:**
   Run regex scanners over all generated log files and memory buffers:
   - Aadhaar Regex: `\b[2-9]{1}[0-9]{3}[0-9]{4}[0-9]{4}\b`
   - Phone Regex: `\b[6-9][0-9]{9}\b`

5. **Fix seeds:**
   Deterministic test identities.

6. **Change one thing at a time:**
   Test log leak guards first.
   Test API response masking second.
   Test consent revocation cascade third.

## STE-100 Implementation Steps

1. Create test file `internal/kyc/privacy_pii_test.go`.
2. Configure custom `slog.Handler` capturing all log records into an in-memory buffer.
3. Execute all KYC and tenant lifecycle flows.
4. Scan the in-memory log buffer with the Aadhaar regex pattern:
   `\b[2-9]{1}[0-9]{3}[0-9]{4}[0-9]{4}\b`.
5. Assert that zero matches are found.
6. Verify API responses:
   - Send `GET /api/tenant/me` as a tenant.
   - Assert `aadhaar_number` is absent or masked.
   - Assert `phone` is formatted as masked string.
7. Test Consent Revocation:
   - Call `RevokeConsent(ctx, tenantID)`.
   - Assert database row transitions to `revoked`.
   - Assert document storage references are cleared from PostgreSQL.

## Acceptance Criteria

- Zero raw 12-digit Aadhaar numbers appear in any application log output.
- All tenant-facing endpoints return masked identifiers.
- Revocation of consent cascades to immediate PII eradication.
- Tests execute clean in under 5 seconds.
