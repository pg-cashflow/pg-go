# Ticket S.01: Post-Audit Minor Findings and Technical Debt Checklist

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Standard**: ASD-STE100
- **Scope**: `pg-go` and `pg-react`

---

## 1. Summary

This ticket documents the five verified findings from the full-stack code audit across `pg-go` and `pg-react`. All five items are minor code cleanups and performance optimizations. Zero security vulnerabilities or architectural breaking changes were found.

| ID | Component | File & Line | Priority | Description |
|:---|:---|:---|:---|:---|
| **FIND-01** | `pg-go` | [service.go:1-3, 716-717](file:///c:/Users/divak/Downloads/pg-go/internal/kyc/service.go#L1-L3) | P3 (Low) | Remove stale docstrings referring to dead DigiLocker webhooks |
| **FIND-02** | `pg-react` | [context.tsx:66](file:///c:/Users/divak/Downloads/pg-react/src/theme/context.tsx#L66) | P3 (Low) | Remove unused `data-theme` attribute on `document.documentElement` |
| **FIND-03** | `pg-react` | [index.html:9-14](file:///c:/Users/divak/Downloads/pg-react/index.html#L9-L14) | P2 (Medium) | Split unconditional multi-script Indic fonts into dynamic font loader |
| **FIND-04** | `pg-react` | [theme-init.js:1-17](file:///c:/Users/divak/Downloads/pg-react/public/theme-init.js#L1-L17) | P2 (Medium) | Set `document.documentElement.lang` pre-paint from `pg_locale` |
| **FIND-05** | `pg-go` | [jwt.go:77-82](file:///c:/Users/divak/Downloads/pg-go/internal/auth/jwt.go#L77-L82) | P3 (Low) | Migrate test calls from deprecated `IssueToken` (30d) to `IssueAccessToken` |

---

## 2. Technical Findings and Remediation Specifications

### FIND-01: Stale DigiLocker Webhook Comments in KYC Service

- **Repository**: `pg-go`
- **Target File**: [internal/kyc/service.go](file:///c:/Users/divak/Downloads/pg-go/internal/kyc/service.go#L1-L9)
- **Root Cause**:
  The KYC module originally planned for DigiLocker webhooks. The architecture refactored to synchronous UIDAI OTP (`InitiateAadhaarOTP`, `CompleteAadhaarOTP`), Cashfree Bharat OCR (`VerifyAadhaarDocument`), and Offline Secure QR (`VerifySecureQR`). The package comment and method comment still reference DigiLocker and webhook completion (`ProcessDigiLockerCompletion`), which does not exist.
- **Lines Affected**:
  - `internal/kyc/service.go:1-3`:
    ```go
    // Package kyc orchestrates the Aadhaar identity verification lifecycle:
    // consent recording, DigiLocker flow initiation, completion via Cashfree webhook,
    // offline Secure QR fallback, DPDP consent revocation, and duplicate-flag clearance.
    ```
  - `internal/kyc/service.go:716-717`:
    ```go
    // GetVerificationByVendorRefID is exposed so the webhook handler can perform
    // the idempotency check before invoking ProcessDigiLockerCompletion.
    ```
- **Remediation**:
  Update package and method comments to describe the active Aadhaar OTP, Smart OCR, and Secure QR methods. Delete references to DigiLocker and `ProcessDigiLockerCompletion`.

---

### FIND-02: Unused `data-theme` Attribute on Root Element

- **Repository**: `pg-react`
- **Target File**: [src/theme/context.tsx](file:///c:/Users/divak/Downloads/pg-react/src/theme/context.tsx#L64-L70)
- **Root Cause**:
  `applyToDocument()` executes:
  ```ts
  document.documentElement.classList.toggle("dark", resolved === "dark");
  document.documentElement.setAttribute("data-theme", resolved);
  document.documentElement.style.colorScheme = resolved;
  ```
  Neither Tailwind CSS configuration nor CSS files reference `[data-theme]`. The application controls all dark-mode styling via the `dark` class. Furthermore, `public/theme-init.js` does not set `data-theme` before first paint, creating an inconsistency between initial load and hydrated state.
- **Remediation**:
  Delete `document.documentElement.setAttribute("data-theme", resolved);` from line 66 of [context.tsx](file:///c:/Users/divak/Downloads/pg-react/src/theme/context.tsx#L66).

---

### FIND-03: Unconditional Multi-Script Font Preloading

- **Repository**: `pg-react`
- **Target File**: [index.html](file:///c:/Users/divak/Downloads/pg-react/index.html#L9-L14)
- **Root Cause**:
  `index.html` contains an early preload for five font families in one Google Fonts request:
  - `IBM Plex Mono` (400, 500)
  - `IBM Plex Sans` (400, 500, 600, 700)
  - `Noto Sans Telugu` (400, 500, 600, 700)
  - `Noto Sans Tamil` (400, 500, 600, 700)
  - `Noto Sans Kannada` (400, 500, 600, 700)
  All users download font descriptors for all three Indic scripts on initial page load, even when using English.
- **Remediation**:
  1. Keep only Latin/English core fonts (`IBM Plex Sans`, `IBM Plex Mono`) in [index.html](file:///c:/Users/divak/Downloads/pg-react/index.html).
  2. Load Indic font families (`Noto Sans Telugu`, `Noto Sans Tamil`, `Noto Sans Kannada`) dynamically inside [src/i18n/provider.tsx](file:///c:/Users/divak/Downloads/pg-react/src/i18n/provider.tsx) when the client selects or initializes with an Indic locale.

---

### FIND-04: Document `lang` Attribute Delayed Until React Hydration

- **Repository**: `pg-react`
- **Target Files**:
  - [index.html](file:///c:/Users/divak/Downloads/pg-react/index.html#L2)
  - [public/theme-init.js](file:///c:/Users/divak/Downloads/pg-react/public/theme-init.js#L1-L17)
  - [src/i18n/provider.tsx](file:///c:/Users/divak/Downloads/pg-react/src/i18n/provider.tsx#L186)
- **Root Cause**:
  `index.html` declares `<html lang="en-IN">`. When a user has selected `te-IN`, `ta-IN`, or `kn-IN`, the document language remains `en-IN` until `provider.tsx` mounts and runs `useEffect(() => { document.documentElement.lang = locale; }, [locale])`.
  Screen readers and browser text shapers encounter an incorrect document language during initial paint.
- **Remediation**:
  Update `public/theme-init.js` to read stored locale and set `document.documentElement.lang` synchronously before first paint:
  ```js
  var storedLocale = localStorage.getItem('pg_locale');
  if (storedLocale && ['en-IN', 'te-IN', 'ta-IN', 'kn-IN'].indexOf(storedLocale) !== -1) {
    document.documentElement.lang = storedLocale;
  }
  ```

---

### FIND-05: Deprecated `IssueToken` (30-day JWT) Test Migration

- **Repository**: `pg-go`
- **Target Files**:
  - [internal/auth/jwt.go](file:///c:/Users/divak/Downloads/pg-go/internal/auth/jwt.go#L77-L82)
  - [internal/auth/middleware_test.go](file:///c:/Users/divak/Downloads/pg-go/internal/auth/middleware_test.go#L42)
  - [internal/api/feature_live_test.go](file:///c:/Users/divak/Downloads/pg-go/internal/api/feature_live_test.go#L79)
  - [internal/api/feature_finance_test.go](file:///c:/Users/divak/Downloads/pg-go/internal/api/feature_finance_test.go#L54)
- **Root Cause**:
  `IssueToken` issues a 30-day token and is marked `Deprecated`:
  ```go
  // Deprecated: Use IssueAccessToken or IssueAccessTokenWithFamily.
  // 30-day access tokens bypass the refresh-token family revocation model.
  ```
  Production code uses `IssueAccessToken` (15m TTL) exclusively. However, older unit and integration tests still instantiate claims with `IssueToken(secret, user)`.
- **Remediation**:
  1. Replace calls to `auth.IssueToken` in test suites with `auth.IssueAccessToken`.
  2. For test cases that explicitly require extended TTLs, pass explicit durations using test helper utilities.
  3. Delete or unexport `IssueToken` once all internal test callers migrate.

---

## 3. Verification Plan

### Automated Test Commands

1. **Backend Verification (`pg-go`)**:
   ```bash
   cd c:\Users\divak\Downloads\pg-go
   go test ./internal/kyc/... -v
   go test ./internal/auth/... -v
   go test ./internal/api/... -v
   ```

2. **Frontend Verification (`pg-react`)**:
   ```bash
   cd c:\Users\divak\Downloads\pg-react
   npm run test
   npm run build
   ```
