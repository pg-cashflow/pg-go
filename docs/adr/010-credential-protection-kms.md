# ADR 010: Credential Protection, Payee Data Security & KMS Envelope Encryption

## Status
Accepted
**Date:** 2026-09-24
**Decider:** Divakar (Solo Developer)

## Context
Handling tenant and payee financial information (bank account numbers, IFSC codes, UPI IDs) and sensitive tokens requires strict security controls to prevent exposure in application logs, database backups, and data snapshots.

## Decisions

### 1. Phased Payee Storage Strategy
- **Phase A (Initial PG Launch)**: Payout sheets are generated based on departing tenant direct requests without persistent unencrypted bank account numbers. Payout instructions are reviewed and approved on an as-needed basis.
- **Phase B (Automated Payouts & Persistent Payees)**: Bank account numbers in `payout_payees` must use AES-256-GCM envelope encryption backed by GCP Cloud Key Management Service (Cloud KMS) as Key Encryption Key (KEK) and local Data Encryption Keys (DEKs). Plaintext account numbers are never persisted to disk or backups.

### 2. Zero Gateway Credentials in Source
- All gateway credentials (`APP_ID`, `SECRET_KEY`, `WEBHOOK_SECRET`) are strictly injected at runtime via environment variables or secret managers (`.env`, Cloud Secret Manager).
- No production keys are committed to Git.

### 3. Signed Short-Lived Media URLs
- Evidence photos and payment screenshots stored in object storage (Google Cloud Storage / S3) are private and accessible exclusively via short-lived (15-minute) authenticated signed URLs.

## Consequences
- Compliance with data protection standards.
- Zero plaintext account numbers in database backups.
- Minimal threat surface for credential theft.
