# ADR-036: Biometric Authentication (PWA vs Native Wrapper)

**Status:** Accepted  
**Date:** 2026-10-16  
**Deciders:** Lead Architect / Project Owner  
**Language Standard:** ASD-STE100  
**Builds on:** ADR-032 (manager capabilities), `docs/specs/auth-frontend-backend-contract.md`

---

## 1. Context

The project requirements specify biometric and face authentication for fast client re-entry without re-entering an SMS OTP.

From first principles, biometric sensors (`BiometricPrompt` on Android, `LocalAuthentication` on iOS) execute hardware-bound checks on the physical mobile device. Raw biometric data never leaves the handset.

However, browser security boundaries introduce an architectural constraint:
1. A standard Progressive Web App (PWA) executing in Safari or Chrome cannot hook app-resume events to device biometrics.
2. WebAuthn in browsers supports public-key authentication ceremonies during login. It does not provide an ambient "unlock the app on launch" hook.
3. True "unlock on app resume" requires access to native mobile operating system APIs.

---

## 2. Decision

The client architecture adopts a **Two-Phase Strategy**:

### Phase 1: Pure PWA Baseline (Web Standard)
- The frontend client (`pg-react`) remains a Progressive Web App.
- Persistent session state relies on the secure `pg_refresh_token` cookie (`HttpOnly`, `SameSite=Strict`, `Secure`).
- Silent token rotation occurs automatically at minute 12 of the 15-minute access token lifespan via `POST /api/auth/refresh`.
- Fast re-entry in Phase 1 relies on session persistence without requiring manual OTP input while the 30-day refresh token remains valid.

### Phase 2: Native Shell Wrapper (Capacitor) for Biometrics
- When native app-resume biometric gating is required, `pg-react` is packaged using a thin **Capacitor** native wrapper.
- The Capacitor wrapper invokes native `BiometricPrompt` (Android) and `LocalAuthentication` (iOS).
- Successful biometric authentication unlocks the local key store containing the refresh token.
- **Backend Impact:** Zero. The backend continues to expose standard `POST /api/auth/refresh` endpoints.

---

## 3. Consequences

### Positive
- **Zero Backend Complexity:** The backend needs no WebAuthn challenge tables, public key cryptography endpoints, or attestation parsers.
- **High User Snappiness:** Users on native shells unlock the app in under 300 ms using device biometrics.
- **Standard Token Replay Defense:** The existing refresh token family rotation and replay revocation protect all sessions.

### Negative
- Pure browser PWA users cannot experience app-resume biometric prompts until wrapped in the native shell.

---

## 4. Action Items

1. Preserve backend `POST /api/auth/refresh` without modifications.
2. Maintain `pg-react` as a clean PWA core that can run standalone or inside a Capacitor mobile shell.
3. Add `@capacitor/core` and `@capacitor-community/biometric-auth` when building the native distribution package.

---

## 5. Implementation status (2026-10-17)

**Backend (`internal/auth`, `internal/api`):** unchanged, as decided — Action Item 1 holds. Two latent gaps found during the auth-soundness review were closed in this pass, both additive and backward-compatible:

- `internal/auth/middleware.go`: the 30s tenant/token-version revocation cache was already correct, but its security-latency tradeoff was undocumented in code. Now has an explicit comment at both `tenantCacheTTL` and `tokenVersionCacheTTL` stating the window and why it's acceptable for this domain (non-payment reads; payout approval sits behind separate step-up OTP).
- `internal/api/handlers_owner_dashboard.go`: `GET /owner/dashboard/summary` now returns `payout_email_missing` (bool), sourced from the owner's live user record. This turns the one-time pre-launch SQL audit in `auth-frontend-backend-contract.md` §6 into a standing signal the owner dashboard can surface as a banner, so the gap doesn't silently regress as new owners onboard post-launch. Covered by `TestOwnerDashboardSummary_PayoutEmailMissing`.

**Frontend (`web/app.js`, the only web client present in this repository):** this reference/demo PWA client was found to violate contract §3.1.6 — it persisted the access token to `localStorage` and never called `POST /api/auth/refresh` at all, so Phase 1 "fast re-entry" was not actually implemented here. Fixed:

- Access token is now memory-only (not seeded from or written to `localStorage`).
- `refreshSilently()` added: calls `POST /api/auth/refresh` with the HttpOnly cookie (`credentials: "include"`), scheduled at minute 12 per §3.2, called once at boot to restore a session from the refresh cookie without replaying Firebase login, and on failure clears session rather than leaving a stale one.
- A `window.pgBiometricGate` hook point is called, if present, before every silent refresh — this is where a Capacitor wrapper's native `BiometricPrompt`/`LocalAuthentication` result would gate app-resume. Absent (pure PWA), refresh proceeds unmodified, preserving Phase 1 behavior exactly.
- `logout()` now calls `POST /auth/logout` (`credentials: "include"`) before clearing local state. The backend handler (`AuthLogout`, `internal/api/handlers_public.go`) already revoked the refresh token family and cleared the cookie — the gap was entirely client-side: the demo client never called it, so "logging out" only cleared browser-local state while the refresh token family stayed active in Postgres for up to 30 days. Fails open on a network error (clears local session anyway) so a disconnected client isn't stuck mid-logout.

**Phase 2 (Capacitor wrapper) — cannot be implemented end-to-end from this repository.** This repo contains no `pg-react` project — only this vanilla-JS `web/` demo client and the Go backend. There is no native shell project to add `@capacitor/core` / `@capacitor-community/biometric-auth` to. `window.pgBiometricGate` above is the integration seam a real Capacitor wrapper would call into; building the wrapper itself requires the actual `pg-react` repository, which is out of scope here. Action Item 3 stays open until that repository is available.
