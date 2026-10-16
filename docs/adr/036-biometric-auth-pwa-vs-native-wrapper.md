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
