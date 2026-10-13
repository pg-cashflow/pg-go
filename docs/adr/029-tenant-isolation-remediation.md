# ADR-029: Tenant Isolation Architecture, Fail-Closed Controls, and Money-Path RLS Extension

**Status:** Accepted  
**Date:** 2026-10-13  
**Deciders:** Core Engineering Team  
**Builds on:** ADR-018, ADR-019, ADR-020, ADR-028  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification (Karpathy Method)  

---

## 1. Context

Static code audits and verification identified key isolation boundaries and security vulnerabilities:

1. **Isolation Split Across Tables:**
   - Database schema inspection reveals 69 property-scoped tables with `property_id`.
   - 13 tables enforce Row-Level Security (RLS) via PostgreSQL policies targeting `pgapp_app`.
   - 56 tables do not enforce database RLS and rely strictly on application-layer `WHERE property_id = $x` filtering.
   - Most request-serving repositories receive `maintPool` (`pgapp_maint`), which operates under `USING (true)`.

2. **Isolation Test Limitations:**
   - Isolation unit tests (`tenant_isolation_test.go`, `scope_authz_test.go`) executed against in-memory stub stores.
   - These tests verify handler authorization logic. They do not verify SQL query predicates or database-level isolation.

3. **Calendar Token Revocation Failure Mode (Finding 1):**
   - Calendar token lookup in `OwnerCalendarICS` previously failed open when database errors occurred or rows were absent.
   - Database insert failures during token generation in `OwnerGetCalendarToken` were ignored.
   - Revoked tokens were never tested against subsequent feed requests.

4. **Webhook Dues Snapshot Error Swallowing (Finding 3):**
   - In `handlers_pay_webhooks.go`, errors from `txIntentRepo.GetDuesSnapshot` were swallowed by `if err != nil || len(snapshot) == 0`.
   - Transient database errors on multi-due intents caused erroneous single-due fallback, leading to incorrect ledger allocation.

---

## 2. Architecture Decision

We evaluated two architectural strategies:
- **Option A:** Retain current dual-pool split. Enforce isolation using application checks, CI ratchet, and live cross-property repository tests.
- **Option B:** Route all request-path repositories through `ScopedDB`. Extend PostgreSQL RLS across all 56 property-scoped tables.

### Selected Strategy: Hybrid Staged Model

We select a **hybrid staged model**: **Option B** for high-risk money paths and sensitive data, combined with **Option A** for auxiliary and high-throughput background paths.

### D1. Option B for Money Paths and Sensitive Tables
1. Target the highest-risk tables first:
   - `bank_accounts`
   - `bank_transactions`
   - `payout_batches` / `payout_items`
   - `financial_journal_entries`
   - `property_calendar_tokens`
   - `tenants` sensitive identity fields
2. Enforce database Row-Level Security (RLS) on these tables with strict fail-closed policies for `pgapp_app`.
3. Route customer-facing request flows through `ScopedDB`, setting `app.current_property_id` in transaction batches.

### D2. Webhook and Asynchronous Job Context Binding
1. Webhooks and background tasks do not have interactive user sessions.
2. Webhooks must resolve and verify the parent entity (`payment_intent`, `order_id`) before execution.
3. Once verified, workers must explicitly bind the property ID to the request context using `requestscope.WithPropertyID(ctx, resolvedPropertyID)` before executing queries.

### D3. Option A for Auxiliary Tables with CI Ratchet
1. Tables not handling financial transactions retain application-layer property predicates.
2. The CI ratchet `scripts/sql/rls_property_scope_ratchet.sql` remains active. It blocks new unclassified tables from entering the repository without explicit security sign-off.
3. Every maintenance-wired repository must include live cross-property integration tests asserting cross-tenant access rejection.

### D4. Remediation of Calendar Token Revocation (Finding 1)
1. **Fail-Closed Verification:**
   - In `OwnerCalendarICS`, token verification checks `property_calendar_tokens`.
   - If the token row does not exist, return HTTP 401 Unauthorized (`"calendar token not found or invalid"`).
   - If the database query returns an unexpected error, return HTTP 503 Service Unavailable (`"database error checking calendar token status"`).
   - If `revoked_at` is set, return HTTP 401 Unauthorized (`"calendar token revoked"`).
2. **Atomic Token Registration:**
   - In `OwnerGetCalendarToken`, token insertion into `property_calendar_tokens` is mandatory.
   - If the insert fails or storage is unavailable, return HTTP 500 Internal Server Error without issuing the signed token.
3. **Pluggable Architecture:**
   - Introduce `CalendarTokenStore` interface on `Deps` to support hermetic testing and database decoupling.

### D5. Remediation of Webhook Dues Snapshot Handling (Finding 3)
1. In `handlers_pay_webhooks.go`, separate database errors from empty result sets in `GetDuesSnapshot`.
2. If `err != nil`, immediately return `fmt.Errorf("get dues snapshot: %w", err)` to abort the transaction.
3. Aborting the transaction returns HTTP 500 to the payment gateway, triggering standard gateway retry backoff.
4. Fall back to single-due allocation only when `err == nil && len(snapshot) == 0`.

---

## 3. Consequences

### Positive
- Revoked and unrecorded calendar tokens cannot access property schedule feeds.
- Transient database errors during webhook processing trigger gateway retries instead of misallocating tenant dues.
- High-risk financial records are protected by database-enforced row isolation.
- Calendar token issuance requires database persistence.

### Trade-offs
- Webhook handlers must perform an initial untrusted lookup to determine `property_id` before acquiring scoped transaction connections.
- Test suites for maintenance repositories require multi-property database fixtures to verify isolation invariants.

---

## 4. Verification Matrix

| Target | Description | Status |
| :--- | :--- | :--- |
| `internal/api/deps.go` | Added `CalendarTokenStore` interface | VERIFIED |
| `internal/api/router.go` | Added `CalendarTokenStore` to `Deps` struct | VERIFIED |
| `internal/api/handlers_owner_dashboard.go` | Implemented fail-closed calendar token generation, verification, and revocation | VERIFIED |
| `internal/api/handlers_owner_dashboard_test.go` | Added tests for revoked tokens (401), unrecorded tokens (401), DB errors (503), insert failures (500) | VERIFIED |
| `internal/api/handlers_pay_webhooks.go` | Fixed `GetDuesSnapshot` error handling to prevent silent multi-due misallocation | VERIFIED |
| `internal/api/handlers_pay_test.go` | Verified webhook test suite operates cleanly | VERIFIED |
