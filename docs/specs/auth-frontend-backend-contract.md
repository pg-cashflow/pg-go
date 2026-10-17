# Authentication and Session Contract Specification

**Status:** Approved Architectural Contract  
**Date:** 2026-10-16  
**Language Standard:** ASD-STE100  
**Scope:** Backend (`cmd/server`, `internal/auth`, `internal/api`) and Frontend Client Applications (Mobile / Web)

---

## 1. Architectural Decisions: Channel and Route Scope

### 1.1 Single Authentication Front Door
- The system uses **Firebase Phone Auth** as the single entry point for user login.
- The public unauthenticated routes `POST /api/auth/otp/request` and `POST /api/auth/otp/verify` are **permanently disabled**.
- **Rationale:**
  1. `internal/app/channels.go` initializes `sms.NoopGateway{}` unconditionally. The backend contains no SMS provider driver.
  2. Firebase Phone Auth manages carrier delivery, fraud detection, and Indian telecom regulations (TRAI DLT) directly.
  3. Disabling custom OTP login removes an insecure entry point that bypasses device attestation.

### 1.2 Step-Up Authentication Channel Scope
- **SMS is permanently out of scope** for step-up authentication.
- Step-up authentication uses **Email via SMTP exclusively**.
- All step-up verification codes (`payout_approval`) transmit to the verified email address of the authenticated owner.

### 1.3 KYC Verification Boundary
- KYC verification does not block user authentication.
- All users with valid Firebase credentials can execute `POST /api/auth/firebase` and receive a session token.
- Unverified tenants can access their tenant portal and view KYC submission screens (`/tenant/kyc/*`).
- KYC status gates sensitive operations only:
  1. Tenant rent payment is blocked if the property sets `require_kyc_before_pay = true`.
  2. Room allocation completes only after identity verification.
  3. Owner payout approvals require verified owner identity.
- Client applications must route unverified tenants to the dedicated KYC submission workflow.

---

## 2. Token Architecture Specification

| Token Type | Lifespan | Format | Storage Location | Transport Mechanism |
|---|---|---|---|---|
| **Access Token** | 15 minutes | JWT (HMAC-SHA256) | Client Memory | `Authorization: Bearer <token>` |
| **Refresh Token** | 30 days | 256-bit Random Hex | `HttpOnly` Cookie (`pg_refresh_token`) | Automatic browser/client cookie transport |

### 2.1 Refresh Token Rotation Rules
- Every call to `POST /api/auth/refresh` revokes the old refresh token and issues a new refresh token.
- Refresh tokens belong to a cryptographic family.
- If a revoked refresh token is presented again (replay attack), the backend revokes the complete token family immediately (`auth.ErrReplayDetected`).
- The backend clears the cookie and returns HTTP 401 with code `AUTH_ACCESS_REVOKED`.

### 2.2 Refresh Token CSRF Risk Profile and Accepted Risk
- The endpoint `POST /api/auth/refresh` relies on the `pg_refresh_token` cookie.
- The cookie uses `SameSite=Strict`, `HttpOnly`, and `Secure` (in production) attributes.
- Modern web browsers do not transmit `SameSite=Strict` cookies during cross-site requests.
- If an unauthorized cross-site request executes on a non-compliant browser:
  1. The attacker cannot read the response body or the new JWT because browser same-origin policies block access.
  2. The server rotates the token family.
  3. The legitimate client encounters an unexpected rotation, causing an `auth.ErrReplayDetected` revocation on next use.
- The security impact is limited to denial-of-service (forced session logout). The threat cannot extract credentials or escalate privileges.
- This behavior is an accepted architectural risk. Dedicated CSRF double-submit tokens are not required.

---

## 3. Frontend Client Specifications

### 3.1 Login Flow
1. The client completes phone authentication using the Firebase SDK.
2. The client receives a Firebase ID token.
3. The client calls `POST /api/auth/firebase` with the payload:
   ```json
   {
     "id_token": "<firebase_id_token>",
     "invite_code": "<optional_code>"
   }
   ```
4. The backend responds with:
   ```json
   {
     "token": "<access_token>",
     "user": { ... }
   }
   ```
5. The backend sets the `pg_refresh_token` cookie automatically (`Path=/api/auth`, `HttpOnly`, `SameSite=Strict`, `Secure`).
6. The client holds the access token in volatile memory only. The client must never write the access token to persistent storage (e.g., LocalStorage).

### 3.2 Silent Background Token Refresh
- The client must not wait for an HTTP 401 response to refresh tokens.
- The client must schedule a proactive refresh call at **minute 12** of the 15-minute access token lifespan.
- The client calls `POST /api/auth/refresh` with an empty body.
- If the refresh succeeds, the client updates the access token in memory.
- If the refresh returns HTTP 401 or HTTP 403, the client clears the in-memory session and redirects the user to the login screen.

### 3.3 Fast Re-Entry (Device Biometrics)
- The mobile client can protect application entry using local device biometrics (Fingerprint, Touch ID, Face ID).
- Biometric verification occurs strictly on the client device.
- The client never transmits biometric data to the backend.
- Upon successful biometric unlock, the client uses the existing refresh token cookie to call `POST /api/auth/refresh`.

### 3.4 Payout Approval Error Handling
- When an owner requests a payout approval code (`POST /owner/payouts/batches/:id/approve/request-otp`), the backend can return:
  * **HTTP 422 Unprocessable Entity:**
    ```json
    {
      "error": "no delivery channel available for payout approval; please register a verified email or phone",
      "code": "AUTH_NO_DELIVERY_CHANNEL"
    }
    ```
- **Client Action:** The client displays a blocking modal prompt: *"No verified email address is linked to your owner account. You must register a valid email address to approve payout batches."*

---

## 4. Backend Service Specifications

### 4.1 Tenant-Status Caching (`RequireTenant`)
- **Target:** `internal/auth/middleware.go`
- **Specification:** The middleware must cache the tenant record retrieved from `tenantRepo.GetByID` for **30 seconds**.
- **Key:** `tenant_id` (UUID).
- **Thread Safety:** The cache must use a `sync.RWMutex`.
- **Invalidation:** If tenant status becomes `vacated`, the backend invalidates the cache entry immediately.

### 4.2 OTP Resend Cooldown (`RequestStepUpOTPWithChannel`)
- **Target:** `internal/auth/service.go`
- **Specification:** The backend enforces a mandatory **30-second delay** between successive OTP requests for the same canonical phone number.
- **Error Response:** If an OTP request occurs within 30 seconds of the prior request, the backend returns HTTP 429 Too Many Requests:
  ```json
  {
    "error": "too many requests",
    "code": "AUTH_RATE_LIMITED",
    "retry_after_seconds": 24
  }
  ```

### 4.3 Role Identity Invariants & Email Scoping
1. **Managers (Phone Primary, Email Secondary):**
   - **Target:** `internal/api/handlers_gamification.go` -> `OwnerCreateManager`.
   - **Specification:** `phone` is mandatory (`binding:"required"`). `email` is optional (`omitempty`).
   - Managers do not approve payout batches (payout routes are strictly restricted to `owner`).
   - A manager without an email can perform all assigned manager duties (inspections, daily attendance, meter readings, expense logging).
2. **Owners (Email Mandatory for Money Movement):**
   - **Target:** `internal/api/handlers_payouts.go` -> `OwnerRequestPayoutBatchOTP`.
   - **Specification:** Payout approval step-up OTP requires a non-empty, valid email on the owner account.
   - If an owner account lacks an email, the backend rejects the payout OTP request with HTTP 422 `AUTH_NO_DELIVERY_CHANNEL`.

---

## 5. Scope Isolation: Magic Links

- Magic links (`GET /p/:token`) are exclusively unauthenticated payment checkout pages.
- Magic links do not issue access tokens.
- Magic links do not set session cookies.
- Magic links do not grant access to tenant dashboard endpoints (`/api/tenant/*`).
- Magic links remain valid for 72 hours even if a tenant vacates the property, allowing settlement of outstanding dues.

---

## 6. Pre-Rollout Data Audit and Remediation

Before enabling payout approval traffic in production, the operations team must execute this read-only audit query to ensure all property owners have delivery channels configured:

```sql
SELECT id, phone, role, property_id, email
FROM users
WHERE role = 'owner'
  AND (email IS NULL OR TRIM(email) = '');
```

### Remediation Protocol:
- If count equals 0: All owners possess verified emails; payout approval is unblocked.
- If count is greater than 0: Prompt affected owners to register an email address upon their next dashboard login before accessing `/owner/payouts/*`.
