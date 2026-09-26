# Aadhaar & Identity Verification Data Handling Policy

**Version**: 1.0  
**Effective Date**: September 2026  
**Applicability**: PG Cashflow Platform (`pg-go`)  
**Regulatory Framework**: Digital Personal Data Protection Act (DPDP Act 2023) Rule 8, Aadhaar and Other Laws (Amendment) Act 2019, and UIDAI Guidelines for Offline Verification Seeking Entities (OVSE).

---

## 1. Purpose Limitation

The PG Cashflow platform collects and processes tenant identity information solely for:
1. Verifying the identity of occupants admitted to paying guest (PG) hostels.
2. Complying with state and municipal police verification mandates for residential accommodations.
3. Preventing financial impersonation in rental security deposit returns and refunds.

Data collected under this policy is strictly barred from being used for commercial profiling, marketing, credit scoring, or secondary processing.

---

## 2. Data Minimisation & Prohibited Storage

In strict compliance with UIDAI regulations:
- **Zero Raw Aadhaar Storage**: The platform **never** accepts, processes, or persists 12-digit unmasked Aadhaar numbers in any database table, log file, cache, or memory buffer.
- **Allowed Attributes**:
  - `aadhaar_last4`: Only the last 4 digits (e.g. `"1234"`), stored as non-confidential display identifier.
  - `masked_uid`: Formatted as `"XXXXXXXX1234"`.
  - `identity_hash`: Salted HMAC-SHA256 representation used exclusively for deduplication and duplicate-tenant detection.
  - `name`, `dob`, `gender`, `care_of`, and address: Demographic attributes required for municipal tenant registry.
  - `attested_photo_bytes`: Standard facial biometric photograph extracted from UIDAI-signed QR or DigiLocker package for on-site manager badge matching.

---

## 3. Verification Architecture

The platform supports two verified identity ingress pathways, both designed to minimize third-party data transit:

### 3.1 Cashfree Secure ID (DigiLocker Gateway)
- Tenant completes Aadhaar authentication directly within government-hosted DigiLocker portal.
- Cashfree Secure ID webhook returns asynchronous verification status (`VERIFICATION_COMPLETED` / `VERIFICATION_FAILED`).
- Network requests fetching demographic documents execute outside database transactions to prevent connection exhaustion.

### 3.2 Offline UIDAI Secure QR Verifier
- On-premise scanning of physical Aadhaar paper QR or e-Aadhaar PDF QR.
- Decompresses and validates RSA-2048 SHA-256 digital signature directly against UIDAI Root Certificate (`internal/aadhaar/secureqr.go`).
- Operates completely offline with zero external network calls or vendor exposure.

---

## 4. Consent Lifecycle & Revocation (DPDP Rule 8)

### 4.1 Explicit Prior Consent
No identity verification or DigiLocker link generation can proceed without positive, affirmative tenant consent:
- Consent verification timestamp recorded in `guardian_consent_verified_at` or tenant audit record.
- If consent is missing, operations fail closed with domain error `kyc.no_consent`.

### 4.2 Minor Protection & Guardian Verification
- Tenants below the age of majority (`is_minor = true`) require documented guardian consent and guardian KYC binding before room assignment.

### 4.3 Atomic Erasure on Revocation (`RevokeConsent`)
Under DPDP Rule 8, tenants retain the absolute right to revoke identity consent:
- Initiating consent revocation executes an atomic PostgreSQL transaction with `FOR UPDATE` row lock on the tenant record.
- The transaction simultaneously:
  1. Nullifies `masked_uid`, `identity_hash`, and `aadhaar_last4`.
  2. Nullifies `attested_photo_bytes` and resets `photo_stored = FALSE`.
  3. Inserts an immutable audit log entry into `tenant_audit_logs`.
- Following revocation, identity queries immediately return domain error `kyc.consent_revoked`.

---

## 5. Storage Security & Access Controls

1. **Isolation by Property (IDOR Protection)**:
   All identity verification records (`tenant_verifications`, `tenants`) are partitioned by `property_id`. All verification endpoints enforce that caller JWT claims match the resource's property boundary.
2. **In-Flight Lease Locking**:
   Concurrent verification requests for the same tenant are serialized using a 30-second distributed mutex lock lease (`TryAcquireInFlightLock`) backed by a 60-second cooldown on pending requests, preventing double-billing and replay attacks.
3. **Log Sanitization**:
   Application loggers strictly redact bearer tokens, identity hashes, and document payloads from stdout.
