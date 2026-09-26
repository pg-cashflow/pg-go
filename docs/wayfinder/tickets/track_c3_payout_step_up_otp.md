# Ticket 4b: Payout Batch Cryptographic Step-Up Reauth & OTP Verification (Track C.3)

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Objective

Remediate non-empty string placeholder in Track C.2 (`handlers_payouts.go` solo-owner batch approval) with real cryptographic step-up re-authentication:
1. **Trigger Endpoint**: Implement `POST /owner/payouts/batches/:id/approve/request-otp` to dispatch a dedicated approval OTP to the solo owner's registered phone before approval.
2. **Purpose-Specific SMS Copy**: Disallow ambiguous "login" copy on financial dispatches. Parameterize OTP messages by purpose:
   - Login: `"Your login OTP is %s. Valid for %d minutes."`
   - Payout Approval: `"Your payout approval code is %s. Valid for %d minutes."` (or `"Your payout approval code for batch %s is %s..."`)
3. **Freshness-Gated Token Verification**:
   - **OTP Step-Up**: Verify OTP against unexpired, unused, un-locked HMAC record in `otp_requests` via `auth.Service.VerifyStepUpOTP` (or `VerifyOTPAndIssueToken`). Marks record as used to prevent replay.
   - **Firebase ID Token Step-Up**: Check `auth_time` claim within the verified Firebase token. Reject any token whose `auth_time` is older than 5 minutes ($\le 300\text{s}$) to prevent client-side silent ambient token reuse.
4. **Fail-Closed Enforcement**: Reject arbitrary strings (e.g. `"CONFIRM"`), expired OTPs, locked OTPs, mismatched phone numbers, and stale tokens with `401 Unauthorized`.

---

## Architectural Context & Findings

In Track C.2, dual control was enforced for multi-owner properties (`batch.CreatedBy != uid`). However, for solo-owner properties (or when co-owners are removed), a single-owner bypass was permitted where `strings.TrimSpace(body.ReauthConfirmation) == ""` was the only check. This allowed any arbitrary string (such as `"CONFIRM"`) to pass without proving human consent or cryptographic identity.

### Three Refinements Incorporated

1. **Missing Trigger Step**:
   - `auth.Service.VerifyOTPAndIssueToken` looks up `LatestUnused(ctx, phone)`.
   - Without an explicit trigger endpoint, an owner approving a batch cannot receive an approval code.
   - We introduce `POST /owner/payouts/batches/:id/approve/request-otp`, which verifies property ownership, batch status (`domain.BatchDraft`), loads the owner's registered phone from `UserStore`, and triggers an OTP via `auth.Service.RequestOTPWithPurpose(ctx, phone, "payout_approval")`.

2. **Purpose-Specific SMS Copy**:
   - `RequestOTP` previously hardcoded `"Your login OTP is %s"`.
   - Reusing login copy for financial authorization creates serious phishing risks and user confusion.
   - Parameterize SMS formatting by purpose: `"Your payout approval code is %s. Valid for 5 minutes."`

3. **Firebase Freshness Check (`auth_time`)**:
   - Firebase ID tokens have a 1-hour expiration and can be refreshed silently in the background by client SDKs without prompting the user.
   - For true step-up authentication, a valid ID token is insufficient unless the user *actively re-authenticated* recently.
   - We extract `auth_time` from the token claims and enforce `time.Since(authTime) <= 5 * time.Minute`. If `auth_time` exceeds 5 minutes, return `401 Unauthorized` instructing the client to perform re-authentication.

---

## Technical Specifications

### 1. `internal/auth/service.go` & `firebase.go`
- Parameterize `RequestOTPWithPurpose(ctx context.Context, phone, purpose string) error`.
  - Maintain `RequestOTP(ctx, phone)` as a backward-compatible wrapper calling `RequestOTPWithPurpose(ctx, phone, "login")`.
  - In `RequestOTPWithPurpose`, generate purpose-specific copy.
- Add `VerifyStepUpOTP(ctx context.Context, phone, otp string) error`:
  - Validates `LatestUnused`, checks TTL (`ErrOTPExpired`), checks max attempts (`ErrOTPLocked`), verifies HMAC, and marks used (`s.otp.MarkUsed`) to block replay.
  - Unlike `VerifyOTPAndIssueToken`, does not require issuing a new session JWT or touching login metadata.
- In `internal/auth/firebase.go`:
  - Extract `auth_time` from `token.Claims["auth_time"]` or `token.AuthTime` into `FirebaseIdentity.AuthTime time.Time`.
- Add `VerifyFirebaseStepUp(ctx context.Context, idToken string, maxAge time.Duration) (FirebaseIdentity, error)`:
  - Verifies ID token via Firebase Admin SDK.
  - Asserts `time.Since(ident.AuthTime) <= maxAge`.
  - Returns `ErrStaleAuthToken` if `time.Since(ident.AuthTime) > maxAge`.

### 2. `internal/api/deps.go` & `router.go`
- Update `AuthService` interface with:
  - `RequestOTPWithPurpose(ctx context.Context, phone, purpose string) error`
  - `VerifyStepUpOTP(ctx context.Context, phone, otp string) error`
  - `VerifyFirebaseStepUp(ctx context.Context, idToken string, maxAge time.Duration) (auth.FirebaseIdentity, error)`
- Update `UserStore` interface in `api` to expose `GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)` (already implemented on `postgres.UserRepo`).
- Register route:
  - `owner.POST("/payouts/batches/:id/approve/request-otp", h.OwnerRequestPayoutBatchOTP)`

### 3. `internal/api/handlers_payouts.go`
- Implement `OwnerRequestPayoutBatchOTP`:
  - Validate `pid`, `uid`, and batch ID.
  - Verify batch belongs to property and is in `domain.BatchDraft`.
  - Fetch owner user record via `h.UserStore.GetByID(ctx, uid)` to retrieve registered phone.
  - If owner has no registered phone, reject `400 Bad Request` ("owner has no registered phone for OTP step-up").
  - Trigger `h.Auth.RequestOTPWithPurpose(ctx, user.Phone, "payout_approval")`.
  - Return `200 OK` with masked phone (e.g. `******1234`).
- Update `OwnerApprovePayoutBatch`:
  - In `solo_owner_reauth` branch:
    - If `body.OTP != ""`:
      - Fetch owner phone from `UserStore.GetByID(ctx, uid)`.
      - Call `h.Auth.VerifyStepUpOTP(ctx, user.Phone, body.OTP)`.
      - If invalid/expired/locked/replay, return `401 Unauthorized` with specific, sanitized error.
    - Else if `body.FirebaseIDToken != ""`:
      - Call `h.Auth.VerifyFirebaseStepUp(ctx, body.FirebaseIDToken, 5*time.Minute)`.
      - Check identity matches authenticated owner (`ident.UID == user.FirebaseUID` or `ident.Phone == user.Phone` or `ident.Email == user.Email`).
      - If stale or mismatch, return `401 Unauthorized`.
    - Else:
      - Reject `401 Unauthorized` ("step-up authentication required: valid otp or fresh firebase_id_token required").
      - Arbitrary placeholder strings are strictly disallowed.

---

## Verification Plan

### Test Scenarios
1. `POST /owner/payouts/batches/:id/approve/request-otp`:
   - Returns 200 and dispatches SMS with `"Your payout approval code is %s. Valid for %d minutes."`.
   - Rejects if batch is not in `domain.BatchDraft` (409 Conflict).
   - Rejects if batch does not belong to owner property (403 Forbidden).
2. `POST /owner/payouts/batches/:id/approve`:
   - **Solo Owner with Valid OTP**: Approves batch, marks OTP used (status 200).
   - **Solo Owner with Replay OTP**: Immediate second attempt with same OTP returns 401 Unauthorized.
   - **Solo Owner with Incorrect OTP**: Returns 401 Unauthorized, increments attempt counter.
   - **Solo Owner with Arbitrary String ("CONFIRM")**: Returns 401 Unauthorized.
   - **Solo Owner with Fresh Firebase ID Token ($\le 5\text{min}$)**: Approves batch (status 200).
   - **Solo Owner with Stale Firebase ID Token ($> 5\text{min}$)**: Returns 401 Unauthorized ("re-authentication expired").
   - **Multi-Owner Dual Control**: Still strictly requires a different owner ID, ignoring step-up tokens.
