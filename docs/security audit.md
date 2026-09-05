# pg-go — End-to-End Security & Bug Audit Playbook

**Purpose:** Hand this file to your local coding agent (Claude Code, Antigravity, etc.)
and have it execute every section below against the real `pg-go` repo on disk.
This is NOT a request to describe what the code probably does — every finding
must cite a real file:line and, where a command is given, real command output.

**Ground rule for the agent:** If you cannot verify something (tool not
installed, file not found, ambiguous behavior), say so explicitly in the
report. Do not infer a pass. A missing check is a finding, not a null result.

**Output required:** A single `AUDIT_REPORT.md` at the repo root with findings
grouped by severity (Critical / High / Medium / Low / Info), each with:
`file:line`, what's wrong, why it matters (concrete exploit scenario, not
generic), and a suggested fix. End with a table of what was NOT checked and why
(tool unavailable, out of scope, etc.) — an honest gap list beats a false
all-clear.

---

## Step 0 — Environment setup (run once)

```powershell
# Go's official vulnerability scanner (checks go.mod/go.sum against known CVEs)
go install golang.org/x/vuln/cmd/govulncheck@latest

# Static security analyzer for Go (SQLi, weak crypto, command injection, etc.)
go install github.com/securego/gosec/v2/cmd/gosec@latest

# Secret scanner — checks working tree AND full git history for leaked credentials
# (install via https://github.com/gitleaks/gitleaks/releases if not present)
gitleaks version
```

If any tool fails to install (network/proxy issues), note it in the report's
gap list and skip to the manual grep-based checks for that category — don't
silently drop the category.

---

## Step 1 — Automated tooling pass

Run each of these from the repo root and paste FULL output (not summarized)
into `AUDIT_REPORT.md` under an "Automated Tooling" section.

```powershell
go vet ./...
govulncheck ./...
gosec ./... 2>&1 | Tee-Object -FilePath gosec_output.txt
gitleaks detect --source . -v --no-git=false 2>&1 | Tee-Object -FilePath gitleaks_output.txt
go build ./... 2>&1
```

**Report requirement:** For every `gosec` finding, don't just list it —
open the file, read the surrounding function, and state whether it's a real
issue or a false positive (with reasoning). Gosec has a meaningful false-positive
rate; a report that pastes raw output without triage is not a review.

---

## Step 2 — Payments (Cashfree) — highest severity category, review first

Locate and open every file under `internal/cashfree/` and any webhook handler
in `internal/api/` that references Cashfree.

Check and report on each of these explicitly (yes/no + file:line):

- [ ] Webhook signature is verified using `hmac.Equal` (constant-time), NOT `==`
      or `bytes.Equal` misuse. Search: `grep -rn "CASHFREE_WEBHOOK_SECRET\|hmac\." internal/cashfree internal/api`
- [ ] Webhook processing is idempotent — replaying the same webhook payload
      twice must not double-credit a payment or double-fire a due-cleared event.
      Look for a check against an `order_id`/`event_id` already processed
      before mutating state.
- [ ] Payment amount is validated server-side against the expected due amount
      — not trusted purely from the webhook/client payload.
- [ ] `CASHFREE_ENV` (sandbox vs. production) is read from config, never
      hardcoded, and there's no code path where a sandbox key could be used
      against the production endpoint or vice versa.
- [ ] TDR/surcharge: confirm the amount charged to the tenant matches the due
      amount exactly (owner-absorbs-TDR policy) — no code path adds a
      convenience fee.
- [ ] Webhook endpoint is NOT behind the same JWT auth middleware as normal
      API routes (Cashfree calls it directly, unauthenticated by your app —
      confirm the signature check is the *only* and *sufficient* gate).
- [ ] Errors from Cashfree's API (timeouts, 5xx) fail safe — a failed payment
      confirmation should not be silently treated as success anywhere.

---

## Step 3 — OTP / SMS

Files: OTP generation/verification code (wherever `GenerateOTP`,
`HashAndVerifyOTP` are implemented — not just their tests), `internal/sms/`.

- [ ] OTP comparison uses constant-time comparison, not `==` on the hash/string.
- [ ] OTP has a server-enforced expiry (check the actual verify function, not
      just that a `created_at` column exists).
- [ ] Rate limiting exists on OTP-send endpoints per phone number AND per IP —
      absence of this = free SMS-bombing vector against your Device A/B
      gateway (real cost to you, real annoyance to the target number).
      Search: `grep -rn "RateLimit\|rate_limit\|Limiter" internal/`
- [ ] OTP length/entropy is adequate (4-digit OTPs with no rate limiting are
      brute-forceable in seconds).
- [ ] SMS gateway API keys (`SMS_PRIMARY_API_KEY`, `SMS_FALLBACK_API_KEY`) are
      never logged, even at debug level. Search: `grep -rn "SMS_.*_API_KEY\|log\..*[Pp]hone" internal/`

---

## Step 4 — Magic links

Files: `internal/magiclink/`.

- [ ] Token is generated via `crypto/rand`, NOT `math/rand`.
      Search: `grep -rn "math/rand" internal/magiclink` — any hit here is a
      Critical finding (predictable tokens = account takeover).
- [ ] Token is single-use (invalidated/marked-used after first successful
      redemption) — confirm by reading the redeem handler, not assuming.
- [ ] Token has a short expiry appropriate to its purpose.
- [ ] The link's token doesn't leak into server access logs via query-string
      logging middleware (check `router.go` / logging middleware for whether
      full URLs including query params are logged).

---

## Step 5 — Aadhaar QR verification

Files: `internal/aadhaar/`.

- [ ] When `AADHAAR_QR_PUBLIC_KEY_PEM` is empty, the code actually rejects
      (fail-closed) numeric Secure QR payloads rather than accepting them
      unverified. This is explicitly called out as the intended behavior in
      your own `.env.example` comment — verify the code matches the comment.
- [ ] Signature verification uses the correct UIDAI public key format and
      fails closed on any parse/verify error (no silent `err != nil` swallow
      that falls through to "verified").
- [ ] Raw Aadhaar numbers are not logged or stored beyond what's functionally
      necessary (check for `log.Println`/`fmt.Printf` near Aadhaar parsing code).

---

## Step 6 — RBAC / multi-tenancy (extends the auth review already done)

- [ ] Every handler under `internal/api/handlers_owner.go` and similar
      owner-scoped files calls `propertyIDFromClaims(c)` and filters the DB
      query by it — grep for any `SELECT`/repo call in an owner handler that
      does NOT reference the extracted property ID:
      `grep -n "func.*Owner" internal/api/handlers_owner.go` then manually
      check each function body.
- [ ] Any route taking a `:id` or `:propertyId` path/query param cross-checks
      it against the JWT's own `property_id`/`tenant_id` rather than trusting
      the param directly (classic IDOR). List every such route and confirm.
- [ ] `RequireTenant`'s vacated-check (confirmed solid in the auth review) has
      an equivalent for any other tenant-lifecycle state introduced since
      (e.g., "banned", "suspended" if such states exist in the domain model).

---

## Step 7 — Injection & input handling

```powershell
# Raw SQL string building — should return nothing outside of migration files
rg -n "fmt.Sprintf\(.*(SELECT|INSERT|UPDATE|DELETE)" --type go internal/
rg -n "\"SELECT .*\" \+" --type go internal/
```

- [ ] Any hit above is reviewed for whether user input reaches the
      interpolated string. GORM/parameterized query builders are fine;
      string concatenation with request data is not.
- [ ] Any file-upload handling (property photos, documents) validates
      content-type and size server-side, not just via frontend `accept=`.

---

## Step 8 — Secrets & config

```powershell
rg -n "change-me|TODO|FIXME|XXX|hack" --type go .
rg -n "InsecureSkipVerify" --type go .
rg -n "os.Getenv\(\"JWT_SECRET\"\)|os.Getenv\(\"OTP_HMAC_SECRET\"\)" --type go .
```

- [ ] Confirm there's a startup check that refuses to boot in `APP_ENV=production`
      if `JWT_SECRET`/`OTP_HMAC_SECRET`/`MAGIC_LINK_HMAC_SECRET` are empty or
      equal to the placeholder values from `.env.example`. If no such check
      exists, that's a Medium/High finding — a production deploy with default
      secrets is a silent catastrophic misconfiguration.
- [ ] `InsecureSkipVerify: true` anywhere in an HTTP client (Cashfree, SMS
      gateway, Firebase) is a Critical finding — MITM-able TLS.
- [ ] `secrets/firebase-sa.json` contents never appear in any log statement.

---

## Step 9 — CORS & transport

- [ ] `CORS_ALLOWED_ORIGINS` parsing in `router.go`/`main.go`: confirm it's an
      exact allowlist match, not a substring/wildcard match that could admit
      an unintended origin.
- [ ] No `Access-Control-Allow-Origin: *` combined with
      `Access-Control-Allow-Credentials: true` anywhere (browsers reject this
      combo for good reason, but some Gin CORS configs generate it by mistake).
- [ ] `APP_ENV=production` path enforces HTTPS / HSTS at whatever layer is
      appropriate (reverse proxy config counts — note where enforcement lives).

---

## Step 10 — Logging & error responses

```powershell
rg -n "c\.JSON\(.*err\.Error\(\)\)" --type go internal/api
rg -n "log\.(Print|Fatal).*phone|log\.(Print|Fatal).*[Aa]adhaar|log\.(Print|Fatal).*[Oo][Tt][Pp]" --type go .
```

- [ ] API error responses returned to clients don't leak raw internal error
      strings (DB constraint names, stack traces, file paths) — map to
      generic client-safe messages.
- [ ] No PII (phone, Aadhaar, OTP, tokens) appears in log statements at any
      level.

---

## Step 11 — Dependency & build health

```powershell
go list -u -m all
go mod tidy -diff   # Go 1.22+: shows whether go.mod/go.sum are out of sync without modifying files
```

- [ ] Flag any dependency with a known-outdated major version, especially
      `firebase-admin`, `gin-gonic/gin`, `jackc/pgx`, and any JWT library —
      these are exactly the packages where CVEs have real history.

---

## Final report structure required

```markdown
# AUDIT_REPORT.md

## Critical
- [file:line] ... / exploit scenario / fix

## High
...

## Medium
...

## Low / Info
...

## Automated tool output
(full govulncheck / gosec / gitleaks output, unedited)

## Not checked / gaps
- e.g. "gosec failed to install due to network restriction — Step 1 gosec
  section is manual-grep-only, flagged as reduced confidence"
```

Do not close any checklist item above with an unverified "looks fine" — every
box needs a file:line citation or an explicit "not applicable, because X."