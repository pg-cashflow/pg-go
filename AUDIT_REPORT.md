# pg-go Security Audit Report

**Auditor:** Antigravity (AI agent)  
**Date:** 2026-09-05  
**Remediation review:** 2026-09-15  
**Scope:** Full repo — all subsystems per `docs/security audit.md`  
**Go version:** 1.26.6

---

## Remediation status (2026-09-15)

| ID        | Original severity | Status      | Evidence                                                                                                                                                                                                                                           |
| --------- | ----------------- | ----------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| H1        | High              | **Fixed**   | Webhook refuses unauthenticated POSTs when `IntentStore` is wired (`internal/api/handlers_pay.go` CashfreeWebhook). `ValidateForRealDeployment` requires `CASHFREE_WEBHOOK_SECRET` whenever Cashfree keys are set (`internal/config/validate.go`). |
| H2        | High              | **Fixed**   | `respondErr` + `ClientError` (`internal/api/apierr.go`). CI grep in `.github/workflows/security-lint.yml`. Remaining 4xx paths use typed sentinels (`typedClientErr`, `paymentClientErr`, `joinHTTPError`).                                        |
| M1        | Medium            | **Fixed**   | Placeholder / short HMAC secrets rejected in `ValidateForRealDeployment` regardless of `APP_ENV`.                                                                                                                                                  |
| M2        | Medium            | **Fixed**   | Per-IP token bucket on OTP and Firebase (`internal/api/router.go`, `internal/api/ratelimit.go`). Per-phone limits remain in `auth.Service`.                                                                                                        |
| M3        | Medium            | **Fixed**   | GIN formatter redacts `/p/` (`redactingLogFormatter` in `internal/api/router.go`). Covered by `TestRedactingLogFormatter`.                                                                                                                         |
| M4        | Medium            | **Fixed**   | `gin.SetMode(gin.ReleaseMode)` when `APP_ENV=production` (`cmd/server/main.go`).                                                                                                                                                                   |
| L1        | Low               | **Fixed**   | `ValidateWarnings` logs webhook-secret fallback.                                                                                                                                                                                                   |
| L2        | Low               | **Fixed**   | `AUTO_MIGRATE` disallowed in production via `ValidateForRealDeployment`.                                                                                                                                                                           |
| L3        | Low               | **Fixed**   | Sandbox warning in `ValidateWarnings`.                                                                                                                                                                                                             |
| L4        | Info              | **Fixed**   | Tenant Aadhaar handler verified (`handlers_tenant.go:151-229`). Binds strictly to JWT context (`tenantFromContext`), zero raw QR/PII logging, overwrite guarded (`GuardOverwrite`), verified by `handlers_tenant_aadhaar_test.go`. |
| L5        | Low               | **Fixed**   | Owner CSV import (`handlers_owner.go:634-642`) uses typed validation error messages from `csv.ParseWithMeta` without exposing internal DB schemas or paths. |
| Owner JWT | High (contract)   | **Fixed**   | `users.token_version` (migration `009_users_token_version.sql`), JWT claim, live middleware check, `POST /api/auth/revoke-sessions`.                                                                                                               |

---

## Summary

| Severity | Count |
| -------- | ----- |
| Critical | 0     |
| High     | 2     |
| Medium   | 4     |
| Low/Info | 5     |

Original findings below are retained for history. Prefer the remediation table above for current status.

No critical findings. Two high findings with clear remediation paths. Crypto core and RBAC are solid.

---

## Critical — None

---

## High

### H1 — Webhook fails open when `CASHFREE_WEBHOOK_SECRET` is empty outside production

**File:** `internal/api/handlers_pay.go:374-376`

```go
if h.CashfreeSecret == "" {
    c.Status(http.StatusOK)
    return
}
```

**What's wrong:** When `CashfreeSecret` is blank the entire HMAC gate is bypassed. The config layer (`config/config.go:72-74`) falls back `CashfreeWebhookSecret = CashfreeSecretKey`, which protects when Cashfree keys are set. However the production-only guard at `config.go:91` means a staging/dev server with real Cashfree keys and `APP_ENV=development` has an open, unauthenticated webhook.

**Exploit scenario:** Developer deploys to a cloud VM with real Cashfree sandbox keys and `APP_ENV=development`. No `CASHFREE_WEBHOOK_SECRET` set. Attacker POSTs a crafted `PAYMENT_SUCCESS_WEBHOOK` body with no signature. The due is settled without payment. (The amount check at line 406 limits financial damage but the due is still marked paid.)

**Fix in `handlers_pay.go`:**

```go
if h.CashfreeSecret == "" {
    if h.IntentStore != nil {
        c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment gateway not configured"})
        return
    }
    c.Status(http.StatusOK)
    return
}
```

**Fix in `config.go`:** require the secret whenever Cashfree is enabled, not just in production:

```go
if cashfreeOn && strings.TrimSpace(cfg.CashfreeWebhookSecret) == "" {
    return nil, fmt.Errorf("CASHFREE_WEBHOOK_SECRET is required when Cashfree is enabled")
}
```

---

### H2 — `err.Error()` leaked directly to HTTP clients across 50+ call sites

**Files:** `handlers_owner.go:88,165,196,219,249`, `handlers_gamification.go` (~20 sites), `handlers_join.go:143,175`, `handlers_pay.go:210,294`

**What's wrong:** Raw `err.Error()` is serialized into JSON responses across the handler layer, including on `http.StatusInternalServerError`. These strings can contain PostgreSQL constraint names, pgx driver details, internal table/column names, and file paths.

**Concrete example:** `handlers_owner.go:88` — `CreateTenant` on internal error can return:
`"ERROR: duplicate key value violates unique constraint \"tenants_phone_key\" (SQLSTATE 23505)"` — revealing table name and constraint name to the client.

**Exploit scenario:** Attacker crafts requests to trigger DB errors, reads responses to map the internal schema, then uses that information to craft targeted IDOR or injection attacks.

**Fix:** One helper replaces all sites:

```go
func apiError(c *gin.Context, status int, err error) {
    msg := "internal error"
    if status < 500 {
        msg = err.Error() // 4xx business-logic messages are generally safe
    }
    // typed domain errors (payment.ErrDuplicateTxn, etc.) are safe at any status
    c.JSON(status, gin.H{"error": msg})
}
```

---

## Medium

### M1 — No startup guard against placeholder secrets in production

**File:** `internal/config/config.go:78-86`

Config refuses empty secrets but does **not** check that they differ from `.env.example` placeholder values (`change-me-to-a-long-random-string`, `change-me-otp-hmac-secret`, `change-me-magic-link-hmac`). A copy-paste deploy passes startup.

**Exploit scenario:** Public repo + `JWT_SECRET=change-me-to-a-long-random-string` in production → attacker forges valid JWTs with `role=owner` for any `property_id`.

**Fix in `config.Load()`:**

```go
if cfg.AppEnv == "production" {
    for _, s := range []string{cfg.JWTSecret, cfg.OTPHMACSecret, cfg.MagicLinkHMACSecret} {
        if strings.HasPrefix(s, "change-me") || len(s) < 32 {
            return nil, fmt.Errorf("all HMAC/JWT secrets must be >= 32 chars and not placeholder values in production")
        }
    }
}
```

---

### M2 — OTP rate limiting is per-phone only; no per-IP limit

**File:** `internal/auth/service.go:81-88`

`RequestOTP` limits to 3 OTPs per 10 minutes per phone. No IP-level rate limit exists anywhere (`grep -rn "RateLimit|Limiter" internal/` — zero results).

**Exploit scenario:** Attacker with rotating IPs targets arbitrary phone numbers at 3 SMS/10 min per source indefinitely. Real cost to the operator (SIM charges) and harassment to target numbers.

**Fix:** Per-IP token-bucket middleware on the OTP route. `golang.org/x/time/rate` is already a transitive dep:

```go
r.POST("/auth/otp/request", ipRateLimitMiddleware(5, time.Minute), h.RequestOTP)
```

---

### M3 — Magic link raw token written to server access logs

**File:** `internal/api/router.go:61` (gin.Logger), `router.go:122` (`/p/:token`)

Gin's default logger writes the full request URI. Every `GET /p/<raw-token>` writes the live 72h payment credential to logs. If logs ship to any aggregator the token is accessible to anyone with log-read access.

**Fix:** Custom log formatter that redacts `/p/` path tokens:

```go
r.Use(gin.LoggerWithConfig(gin.LoggerConfig{
    Formatter: func(p gin.LogFormatterParams) string {
        path := p.Path
        if strings.HasPrefix(path, "/p/") {
            path = "/p/[REDACTED]"
        }
        return fmt.Sprintf("[GIN] %v | %3d | %s\n", p.TimeStamp.Format(time.RFC3339), p.StatusCode, path)
    },
}))
```

---

### M4 — Gin debug mode logs `Authorization: Bearer <token>` headers

**File:** `internal/api/router.go:61`

With `GIN_MODE=debug` (default unless `APP_ENV=release`), Gin logs all headers including `Authorization`. Bearer JWTs in logs = 30-day session credentials in any log aggregator.

**Fix in `main.go`:**

```go
if cfg.AppEnv == "production" {
    gin.SetMode(gin.ReleaseMode)
}
```

---

## Low / Info

### L1 — `CashfreeWebhookSecret` silently falls back to `CashfreeSecretKey`

**File:** `internal/config/config.go:72-74`

If the Cashfree API secret is rotated without updating `CASHFREE_WEBHOOK_SECRET`, webhook HMAC silently uses the wrong key. Add a startup `logger.Warn("CASHFREE_WEBHOOK_SECRET not set; falling back to CASHFREE_SECRET_KEY")`.

### L2 — `AUTO_MIGRATE=1` has no production guard

**File:** `cmd/server/main.go:47`

Documented as dev-only but nothing prevents it in production. Add:

```go
if os.Getenv("AUTO_MIGRATE") == "1" && cfg.AppEnv == "production" {
    log.Fatal("AUTO_MIGRATE=1 disallowed in production; use cmd/migrate")
}
```

### L3 — No startup warning for `CASHFREE_ENV=sandbox` with live keys

**File:** `internal/config/config.go:68`

Sandbox default is correct. A startup `logger.Warn("Cashfree sandbox mode — payments not real")` reduces risk of accidentally shipping sandbox mode.

### L4 — `TenantAadhaar` HTTP handler not reviewed

**File:** `internal/api/handlers_tenant.go`

`aadhaar/` package logic is fully verified (Step 5). The HTTP handler was not read. Verify: (a) IDOR guard matches JWT tenant ID, (b) raw QR strings not logged.

### L5 — CSV parse error leaks internal message at `handlers_owner.go:629`

Low risk (owner-only route). Use sanitized message for consistency with other routes.

---

## Step-by-Step Checklist

### Step 2 — Payments (Cashfree)

| Check                                          | Result        | Citation                                                          |
| ---------------------------------------------- | ------------- | ----------------------------------------------------------------- |
| HMAC uses `hmac.Equal` (constant-time)         | PASS          | `cashfree/client.go:164`                                          |
| Webhook idempotent (duplicate event safe)      | PASS          | `handlers_pay.go:410-415`: `GetByCFPaymentID` + `ErrDuplicateTxn` |
| Amount validated server-side vs. stored intent | PASS          | `handlers_pay.go:406-408`                                         |
| `CASHFREE_ENV` env-driven, not hardcoded       | PASS          | `cashfree/client.go:27-32`                                        |
| No sandbox/production URL confusion            | PASS          | `baseURL()` deterministic on `Env` string                         |
| No TDR/surcharge code path                     | PASS          | Amount = `intent.AmountPaise` (DB-stored), no fee logic           |
| Webhook not behind JWT middleware              | PASS          | `router.go:126` — unauthenticated group                           |
| API errors fail safe (no silent success)       | PASS          | `handlers_pay.go:418-424`                                         |
| Empty secret fails open                        | **HIGH (H1)** | `handlers_pay.go:374-376`                                         |

### Step 3 — OTP / SMS

| Check                                   | Result           | Citation                                                |
| --------------------------------------- | ---------------- | ------------------------------------------------------- |
| OTP comparison constant-time            | PASS             | `auth/otp.go:31-33`: `hmac.Equal`                       |
| Server-enforced OTP expiry              | PASS             | `auth/service.go:124-126`                               |
| Rate limit per phone                    | PASS             | `auth/service.go:81-88`: 3/10min                        |
| Rate limit per IP                       | **MISSING (M2)** | No IP limiting anywhere                                 |
| OTP entropy (6 digits + 5-attempt lock) | PASS             | `auth/otp.go:16`: `crypto/rand`                         |
| SMS API keys not logged                 | PASS             | grep confirms clean                                     |
| OTP attempt lockout                     | PASS             | `auth/service.go:127-129`: `ErrOTPLocked` at 5 attempts |

### Step 4 — Magic Links

| Check                             | Result          | Citation                                          |
| --------------------------------- | --------------- | ------------------------------------------------- |
| Token uses `crypto/rand`          | PASS            | `domain/token.go:30`                              |
| `math/rand` not used in magiclink | PASS            | grep: no results                                  |
| Token is single-use               | PASS            | `magiclink/service.go:90-92`                      |
| 72h TTL                           | PASS            | `domain/token.go:14`                              |
| Token leaks into logs             | **MEDIUM (M3)** | `router.go:61` — gin.Logger logs `/p/<raw-token>` |
| Token comparison constant-time    | PASS            | `magiclink/token.go:21-22`: `hmac.Equal`          |

### Step 5 — Aadhaar QR

| Check                           | Result | Citation                                              |
| ------------------------------- | ------ | ----------------------------------------------------- |
| Empty key → fail-closed         | PASS   | `aadhaar/secureqr.go:101-104`                         |
| Correct RSA-SHA256 verification | PASS   | `aadhaar/secureqr.go:133-135`: `rsa.VerifyPKCS1v15`   |
| Any error fails closed          | PASS   | Both `verifySecureQR` tries must pass                 |
| Raw Aadhaar not logged          | PASS   | No logging in aadhaar package; only `UIDLast4` stored |
| `init()` sets no default trust  | PASS   | `aadhaar/secureqr.go:31-33`: `securePub = nil`        |

### Step 6 — RBAC / Multi-tenancy

| Check                                             | Result | Citation                                                                                |
| ------------------------------------------------- | ------ | --------------------------------------------------------------------------------------- |
| All 23 owner handlers call `propertyIDFromClaims` | PASS   | Manually verified every handler in `handlers_owner.go` and `handlers_pay.go`            |
| `:id` path params cross-checked vs. JWT           | PASS   | `UpdateTenant:132`, `WaiveDue:410`, `ManualMatch:438`, `ConfirmPaymentReport:284`, etc. |
| `RequireTenant` live DB status check              | PASS   | `auth/middleware.go:76-88`                                                              |
| Manager cannot reach owner routes                 | PASS   | Separate `RequireOwner` vs `RequireManagerOrOwner` groups                               |
| Tenant IDOR on `/tenant/dues/:id`                 | PASS   | `handlers_pay.go:97`                                                                    |

### Step 7 — Injection & Input Handling

| Check                                             | Result | Citation                  |
| ------------------------------------------------- | ------ | ------------------------- |
| No raw SQL string building                        | PASS   | grep: no results          |
| No string concatenation with SQL                  | PASS   | grep: no results          |
| Image: size limit + magic-byte content-type check | PASS   | `handlers_pay.go:146-163` |
| CSV: size limit                                   | PASS   | `handlers_owner.go:618`   |

### Step 8 — Secrets & Config

| Check                                          | Result           | Citation                         |
| ---------------------------------------------- | ---------------- | -------------------------------- |
| Startup refuses empty `JWT_SECRET`             | PASS             | `config/config.go:78-79`         |
| Startup refuses empty `OTP_HMAC_SECRET`        | PASS             | `config/config.go:81-83`         |
| Startup refuses empty `MAGIC_LINK_HMAC_SECRET` | PASS             | `config/config.go:84-86`         |
| Placeholder secret guard in production         | **MISSING (M1)** | No check for "change-me" values  |
| `InsecureSkipVerify` anywhere                  | PASS             | grep: no results                 |
| Firebase SA JSON not logged                    | PASS             | `main.go:114` — path string only |

### Step 9 — CORS & Transport

| Check                             | Result | Citation                                              |
| --------------------------------- | ------ | ----------------------------------------------------- |
| CORS exact-match allowlist        | PASS   | `router.go:64`: gin-cors exact match                  |
| No `*` + `AllowCredentials: true` | PASS   | `router.go:67`: `AllowCredentials: false`             |
| HTTPS enforced in production      | INFO   | Delegated to reverse proxy — not verifiable from code |

### Step 10 — Logging & Error Responses

| Check                                | Result        | Citation                           |
| ------------------------------------ | ------------- | ---------------------------------- |
| No raw DB errors to clients          | **HIGH (H2)** | `err.Error()` at 50+ handler sites |
| No PII (phone, OTP, Aadhaar) in logs | PASS          | grep confirms clean                |

### Step 11 — Dependencies

| Check                        | Result | Citation                 |
| ---------------------------- | ------ | ------------------------ |
| `go vet ./...`               | PASS   | Exit 0, no output        |
| `govulncheck ./...`          | PASS   | 0 called vulnerabilities |
| gin v1.12.0                  | PASS   | Current stable           |
| golang-jwt/jwt/v5 v5.3.1     | PASS   | Current                  |
| jackc/pgx/v5 v5.10.0         | PASS   | Current                  |
| firebase-admin/go/v4 v4.21.0 | PASS   | Current                  |

---

## Automated Tool Output

### `go vet ./...`

```
Exit code: 0
(no output)
```

### `govulncheck ./...`

```
=== Symbol Results ===

No vulnerabilities found.

Your code is affected by 0 vulnerabilities.
This scan also found 0 vulnerabilities in packages you import and 4
vulnerabilities in modules you require, but your code doesn't appear to call
these vulnerabilities.
```

### `gosec ./...`

NOT CHECKED — not installed. All gosec rule categories manually grep-checked (SQLi, `math/rand`, `InsecureSkipVerify`, hardcoded credentials, file traversal). See checklist above.

### `gitleaks detect`

NOT CHECKED — not installed. Manual scan of tracked files found no hardcoded credentials.

---

## Not Checked / Gaps

| Area                                    | Reason                                                                                                                   |
| --------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `gosec` static analysis                 | Not installed. Manual grep-based equivalent performed. Reduced confidence vs. full tool scan.                            |
| `gitleaks` git history scan             | Not installed. Full git history not scanned. **Strongly recommend before any public repo exposure.**                     |
| `handlers_tenant.go` — `TenantAadhaar`  | `aadhaar/` package verified. HTTP handler itself not read. Verify IDOR guard + no raw QR logging. (L4)                   |
| `internal/gamification/` service        | Not reviewed. Verify reward redemption idempotency and cross-tenant isolation.                                           |
| `internal/postgres/` repository queries | Not read. `go vet` + `govulncheck` pass; no SQL string-building in handler/service layers. Repo-layer queries unaudited. |
| HTTPS/HSTS enforcement                  | Assumed at reverse proxy. Infrastructure config not in repo.                                                             |
| `go mod tidy -diff`                     | Requires network — not run.                                                                                              |
| Actual `.env` secret values             | Not read. `.env.example` used as config shape proxy.                                                                     |

---

## Recommended Remediation Order

1. **H2** — One `apiError()` helper eliminates all 50+ `err.Error()` leaks.
2. **M1** — Placeholder-secret guard in `config.Load()` for `APP_ENV=production`.
3. **H1** — Harden webhook empty-secret to reject (not silently accept) when Cashfree is configured.
4. **M2** — Per-IP token bucket on `POST /auth/otp/request`.
5. **M3/M4** — Structured logging with token/header redaction; `gin.SetMode(gin.ReleaseMode)`.
6. **Run `gitleaks` against full git history** — one-time, before any public exposure.
7. **L4** — Read and verify `TenantAadhaar` HTTP handler.
