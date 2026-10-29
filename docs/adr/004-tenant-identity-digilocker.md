# ADR 004: Tenant Identity Onboarding via Cashfree Secure ID (DigiLocker Direct API) & Secure QR Fallback

## Status
Superseded in part by Migration 077 (Aadhaar OTP + Smart OCR Manual Review + UIDAI Secure QR)
*Note: DigiLocker direct redirection has been retired in favor of Cashfree Offline Aadhaar OTP, UIDAI Secure QR offline parsing, and single-document Smart OCR with manual review queue (Migration 077).*

## Context
Tenants must verify identity (Aadhaar) during onboarding before room allocation. Calling UIDAI directly as an AUA/KUA is legally closed to a startup.
- Raw Offline Aadhaar OTP to UIDAI is discontinued for non-regulated entities.
- DigiLocker provides a UIDAI-backed, government-hosted authentication interface where tenants log in and consent directly.
- For tenants with no linked mobile number or where DigiLocker is unavailable, an offline fallback is required.

## Decisions

1. **Direct API Integration (Not KYC Studio)**:
   - Our Go backend orchestrates the DigiLocker redirect lifecycle directly (`Verify Account` -> `Create DigiLocker Link` -> redirect -> `Get Status` -> `Get Document`).
   - Preserves complete data ownership, enforcing strict DPDP data minimization and custom audit log hash-chaining that hosted widgets cannot support.

2. **Offline Fallback: Aadhaar Secure QR Only (Ruling Out Offline XML)**:
   - **Aadhaar Secure QR**: Decodable completely offline. Contains demographic data, photo, and a 2048-bit RSA-SHA256 digital signature from UIDAI. Supported out of the box via `internal/aadhaar/secureqr.go` and `AADHAAR_QR_PUBLIC_KEY_PEM`.
    - **Certificate Lifecycle & Canary Structural Limit**: UIDAI signing certificates rotate periodically. The public key is loaded dynamically from configuration (`AADHAAR_QR_PUBLIC_KEY_PEM`), not compiled in. A non-skippable canary test runs in scheduled CI with an authorized test fixture.
      - **Inherent Automated Detection Blind Spot**: Because Secure QR verification is entirely offline by design, there is no live UIDAI endpoint to poll. A static fixture + static PEM can pass in CI indefinitely while silently drifting away from what UIDAI is currently signing in the wild.
      - **Quarterly Ops Runbook**: Automated CI structurally cannot detect UIDAI certificate rotation. Ops policy mandates a quarterly recurring ticket / calendar reminder: an operations engineer must re-fetch the current official UIDAI public signing certificate from UIDAI's portal, re-generate a fresh test fixture QR from a newly issued mAadhaar/e-Aadhaar, and commit the updated fixture + cert to verify against current UIDAI signatures.
    - **Payload Format**: Implementation targets Secure QR v2 format (post-2022 image/data specifications).
    - **Offline XML Share-Code (Ruled Out)**: Downloading offline XML from `myaadhaar.uidai.gov.in` requires an OTP to the linked mobile, defeating the purpose for users with no linked mobile. Ruled out as an offline solution.
    - **Assurance Tiers**:
      - `digilocker`: **Tier 1 (High Assurance)** — Live government-attested pull.
      - `qr`: **Tier 2 (Medium Assurance)** — Offline cryptographic verification of a static card/PDF snapshot.

3. **Pending Row Lifecycle, Concurrency & Deadlock Prevention**:
   - The unique index `idx_kyc_one_active_per_tenant` allows one active row (`WHERE status IN ('pending', 'verified')`).
   - **TTL & Expiry**: Pending verifications have a 1-hour expiration window.
   - **Atomic Supersede via Tenant Row Lock**: Initiating a new verification executes `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE` to serialize attempts per tenant. Any stale `pending` row (or lapsed `verified` row where `expires_at <= NOW()`) is updated to `status = 'expired'` before inserting the new row.
   - **Network-Before-Transaction Invariant & Accepted Trade-off**: The Cashfree API call (`CreateDigiLockerLink`) occurs *before* opening the DB transaction so no locks are held across network I/O.
     - **Accepted Trade-off (Orphaned Vendor Session)**: If the Cashfree call succeeds but the subsequent DB transaction aborts (e.g. lock timeout, database partition, commit error), an orphaned session exists on Cashfree's servers with no corresponding database record. This is explicitly accepted because: (1) Cashfree links auto-expire after their short TTL (~10 min), (2) the tenant's retry generates a brand-new `vendor_reference_id` and fresh link, and (3) holding row locks across external HTTP requests poses a severe connection-pool exhaustion and deadlock risk that far outweighs an orphaned vendor session.

4. **Webhook & Callback Idempotency**:
   - Cashfree webhook deliveries can be duplicated.
   - The handler relies on `vendor_reference_id UNIQUE` with an explicit upsert-or-noop pattern: if the event is already applied, return `200 OK` without triggering duplicate audit events or state transitions.

5. **`KYC_HMAC_SECRET` Rotation Runbook & Known Limitation**:
   - Each verification records `hash_key_version SMALLINT NOT NULL DEFAULT 1`.
   - When the secret is rotated, `hash_key_version` advances to $N+1$.
   - **Policy**: Rotation uses a two-phase runbook:
     1. New verifications write hashes with version $N+1$.
     2. A background backfill re-computes `identity_hash` for existing active `verified` records using the new key, updating `hash_key_version = N+1`.
     3. Deduplication lookups match on `(identity_hash, hash_key_version)`.
     4. **Known Limitation (Rotation Blind Spot)**: Because deduplication matching is scoped to `(identity_hash, hash_key_version)`, dedup coverage is temporarily incomplete across an active rotation window until the Phase 2 backfill completes and reconciles key versions. This is an accepted trade-off of keyed zero-knowledge hashing.

6. **Schema & Compliance Controls (Migration 016)**:
   - **Nullable Pending Rows**: `masked_uid` and `identity_hash` are NULL during initiation and populated only upon verification:
     `CHECK (status <> 'verified' OR (verified_at IS NOT NULL AND expires_at IS NOT NULL AND identity_hash IS NOT NULL AND masked_uid IS NOT NULL))`.
   - **DPDP Rule 8 Revocation**: On consent withdrawal, `masked_uid` and `identity_hash` are set to NULL. The row skeleton (`id`, `vendor_reference_id`, `status = 'revoked'`, `revoked_at`) is retained for compliance audit, and active identity in tenant profile is cleared.
   - **Audit Log Immutability & Hash Chain**: `kyc_audit_log` has `ON DELETE RESTRICT`, a Postgres trigger rejecting `UPDATE`/`DELETE`, and a cryptographic hash chain (`prev_hash`). Lookups for `prev_hash` strictly order by `id DESC` (not timestamps) to guarantee monotonicity within transactions.
   - **Data Minimization (No Raw XML in v1)**: Signed XML or photo links are not retained. Only verified metadata (`masked_uid`, `verified_at`, `expires_at`) is persisted.
   - **`tenants.aadhaar_last4` Denormalization & Lapsed PII Clearance Policy**:
     - *Initial Design*: Initially proposed reading masked UID strictly from `kyc_verification WHERE status = 'verified'` to eliminate two-places-to-sync risks.
     - *Reversal Rationale*: Existing query surfaces (`tenantResponse` in `handlers_owner.go`, tenant profile in `handlers_tenant.go`, `property_repo.go`) directly serialize `t.AadhaarLast4` from `tenants`. Refactoring all legacy tenant queries and frontend consumers to join `kyc_verification` was deliberately scoped out of Phase 2 to prevent breaking existing owner/tenant listings.
     - *Synchronization & DPDP Revocation Invariant*: `CompleteVerificationTx` atomically copies `masked_uid` into `tenants.aadhaar_last4`. Crucially, `RevokeConsent` executes an atomic cascade under `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE` that simultaneously nulls `kyc_verification.masked_uid`, `kyc_verification.identity_hash`, AND `tenants.aadhaar_last4`, ensuring zero residual PII in `tenants`.
     - *Lapsed Verification Policy & Automated Reaper*: When a tenant's verification lapses (`expires_at <= NOW()`), the active profile display `tenants.aadhaar_last4` is cleared to `NULL` (both when re-initiating and via automated daily cron execution of `cmd/kyc-expiry` / `internal/jobs/kyc_expiry.go`). A tenant whose identity proof has lapsed cannot masquerade as currently verified.
   - **Symmetric Duplicate Collision Handling (`duplicate_detected`)**:
     - `CompleteVerificationTx` queries `(identity_hash, hash_key_version)` across all active verified tenants prior to commit.
     - If a collision is found, flagging is **strictly symmetric**: both the incoming verification row AND the pre-existing colliding verification row(s) are updated to `duplicate_detected = TRUE`.
     - Audit entries (`action = 'duplicate_detected'`) are appended to both tenants' immutable audit chains.
     - In accordance with the flag-don't-block security principle, verification is not rejected, but surfaced as flagged in audit logs and owner views for manual human review.
     - False-positive clearance will be handled via dedicated API handler (`POST /api/owner/tenants/:id/kyc/clear-duplicate`) with audited reason logging.

7. **Webhook Resilience & Permanent vs. Transient Error Classification**:
   - **HMAC Verification over Raw Wire Bytes**: Webhook payload raw bytes are captured and signed before JSON unmarshaling.
   - **Upstream Error Classification**:
     - *Permanent Errors*: 4xx client errors (e.g. 400 bad ref, 404 session expired, 422 unprocessable) or corrupt/incomplete demographic responses from Cashfree are marked permanent. `ProcessDigiLockerCompletion` terminates the verification as `failed` in the DB via `FailVerificationTx` with an audited reason and returns `nil`. The webhook handler acknowledges with `200 OK`, preventing Cashfree retry storms and avoiding stuck `pending` records.
     - *Transient Errors*: 5xx server errors, network connection drops, timeouts, or 429 rate limits return errors, prompting the webhook handler to respond `500 Internal Server Error` so Cashfree retries delivery with exponential backoff.
   - **Stale Pending TTL & Retry Window Safety**: Pending sessions are reaped after 24 hours (`pendingTTL = 24 * time.Hour`), comfortably exceeding Cashfree's maximum webhook retry backoff schedule (~24h) to avoid race conditions where a late completion webhook lands on an already-expired record.
   - **In-Transaction TOCTOU Race Guard**: `CompleteVerificationTx` enforces `SELECT ... FOR UPDATE` and checks `existing.Status == 'pending'` before writing `status = 'verified'` (guarded by `WHERE id = $1 AND status = 'pending'`). If a tenant re-initiates or revokes consent while the upstream document fetch is in-flight, `CompleteVerificationTx` aborts with `ErrVerificationNotPending`, and `ProcessDigiLockerCompletion` gracefully returns `nil` (200 OK), ensuring the superseded/revoked state is never overwritten.

## Consequences
- Single unified verification state machine handling both live DigiLocker and offline Secure QR.
- No third-party dependency for QR verification (handled in-process via Go crypto).
- Compliant with DPDP Act 2023 with tamper-evident audit ledger.

## Amendment (2026-10): Blocking Onboarding Gate

**Standard: ASD-STE100.** This section uses simple words and short sentences.

Section "Tenant Identity Verification (ADR-004 KYC)" in CONTRACT.md previously said
KYC does not block dashboard access ("Non-Blocking Onboarding"). This amendment
changes that decision for newly allocated tenants.

**Decision.** A tenant must pass the KYC onboarding gate before they can open the
tenant dashboard or any other tenant route. The gate is separate from the existing
`RequireKYCBeforePay` property setting, which still controls payment access only.
The dashboard gate applies to every property, with no opt-out, because identity
checks now happen as part of onboarding itself, not as a later payment condition.

**Why.** First-principles reasoning: the tenant relationship starts with "who is
this person," not "can they pay." Gating the dashboard, not just payment, moves the
trust check to the start of the relationship, where it belongs, and removes an
entire class of UI states (an unverified tenant browsing dues, community, and
rewards screens) that the product no longer needs to support or test.

**What "pass the gate" means.** A tenant passes once `kyc_verification.status` is
one of:
- `verified` — an automatic method (DigiLocker, Aadhaar OTP, or Secure QR)
  completed. These are near-instant (DigiLocker: a redirect and consent screen;
  Aadhaar OTP: two round trips, no redirect at all), so the gate does not block on
  anything outside the tenant's own next action.
- `pending_manual_review` — the tenant uploaded a document. The remaining step
  (owner review) is outside the tenant's control. Making them wait at a blank gate
  for an owner to act would strand them with no path forward, so the gate treats
  "I did my part" as enough. `TenantKYCCard` shows the review-pending state once
  past the gate, so the tenant still sees exactly where they stand.

Any other status (`unverified`, `pending` mid-flow with no action taken yet,
`failed`, `rejected`, `expired`, `revoked`) keeps the gate closed.

**Where it is enforced.**
1. **Server (source of truth).** `GET /me/onboarding` (`internal/join/onboarding_state.go`)
   returns a new `stage: "kyc_required"` whenever the gate is not passed, computed
   from `Handlers.KYCOnboardingGatePassed` (`internal/api/middleware_kyc.go`), which
   reads live KYC status the same way `IsKYCVerified` already does — no stale JWT
   claims, no caching.
2. **Client (feels instant).** `TenantKYCOnboardingGate`
   (`src/components/shared/TenantKYCOnboardingGate.tsx`) wraps every `/tenant/*`
   route. It checks `GET /tenant/kyc/status` directly (no extra round trip through
   `/me/onboarding`) and renders a full-screen verification screen — both DigiLocker
   and Aadhaar OTP presented as equal, upfront choices, exactly as `TenantKYCCard`
   already offers them — instead of the dashboard, until the gate passes.

The client check is a UX convenience, not a security boundary: it must stay fast
and frictionless, but it is not what makes the gate correct. Every existing
tenant-scoped API route keeps its own authorization as before; a tenant who
bypasses the client gate (e.g. a direct API call) still cannot do anything the
`RequireKYCVerified` / `RequireKYCIfPolicyEnforced` middleware, or the route's own
business rules, do not already allow.

**What did not change.** `RequireKYCBeforePay`, the duplicate-detection flow, the
DPDP consent and revocation rules, the DigiLocker/OTP/QR/upload verification logic
itself, the webhook and reconciliation paths, and the audit log are all untouched.
A tenant whose KYC later expires or is revoked is not retroactively locked out of
the dashboard by this amendment — expiry/revocation handling remains exactly what
ADR-004 section 6 already specifies; this amendment governs the one-time onboarding
gate only, not ongoing session validity.

