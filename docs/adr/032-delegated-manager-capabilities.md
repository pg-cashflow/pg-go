# ADR-032: Delegated Manager Capabilities and Tenant Module Toggles (M1b)

**Status:** Accepted
**Date:** 2026-10-14
**Deciders:** Sasi
**Builds on:** ADR-009 (payout approval), ADR-018/029 (RLS), Track C.2/C.3 (maker-checker, step-up)
**Migration number:** next free number in `migrations/` when the migration is written. ADRs do not reserve numbers.

## Context

- No manager permission system exists. `/manager/*` is a fixed route group that also admits owners. Several owner endpoints are not reachable by managers.
- The only switches are `property_settings.active_modules` (gamification, kyc, payouts, accounting, calendar) and a few flags. They are per module, not per role.
- The owner must be able to act as superuser and hand specific powers to a manager, with every change recorded.
- Manager lifecycle is incomplete: `POST /managers` only. No list, deactivate or reset.
- Money-out and ledger-control actions already have maker-checker and step-up. Delegation must not weaken them.

## Decision

### D1. Capability table (Option B)

`manager_capabilities(property_id, user_id, capability, granted_by, granted_at, revoked_at)` with RLS on `property_id`. A separate `capability_defaults(property_id, capability)` holds the per-property template applied to a new manager.

### D2. Named capabilities (initial)

`tenants.view`, `dues.view`, `dues.mark_cash_paid`, `expenses.post`, `expenses.void`, `attendance.mark`, `inspections.write`, `meters.write`, `kitchen.headcount`, `complaints.respond`, `notifications.send`.

### D3. Never delegable (owner only, enforced in code, not in the table)

Payout create/approve/export, period lock and unlock, bank accounts, deposit settlement approval, manager create/deactivate, capability changes, settlement discrepancy resolution, step-up reauth. The table rejects these names at write time.

### D4. Enforcement

`RequireCapability(cap)` middleware replaces the fixed manager checks. Owner always passes. Missing grant: 403. Database error: 503 (fail closed). Grants are cached for at most 30 seconds, and a change clears the cache entry.

### D5. Audit and lifecycle

Every grant, revoke, create, deactivate and reset writes an `events` row (`manager.capability.changed`, `manager.deactivated`). Endpoints: `GET /owner/managers`, `PUT /owner/managers/:id/capabilities`, `POST /owner/managers/:id/deactivate`, `POST /owner/managers/:id/reactivate`, `POST /owner/managers/:id/reset-credentials`, `GET /manager/me/capabilities` (so the UI can hide controls; hiding is cosmetic only).

### D6. Tenant toggles

Extend `property_settings.active_modules` with `referrals`, `leaderboard`, `meal_rsvp`, `menu_poll`. A disabled module returns 404 on its routes. **Not toggleable:** KYC consent and payment visibility (legal and money matters). Default on for new properties. Every change writes an audit event.

### D7. Solo owner

When a property has no active manager, the UI shows one merged "Today" view. No manager screens, no capability prompts.

## Options Considered

| Option | Complexity | Granularity | Auditability |
|---|---|---|---|
| A. Flags in `property_settings` JSONB | Low | Per property only | Weak (one JSON blob) |
| **B. `manager_capabilities` table (selected)** | Medium | Per manager, per capability, with property defaults | Row per grant |
| C. Full RBAC with custom roles | High | Highest | High |

C is more than one owner and 3-4 staff need. A cannot express "manager X may post expenses, manager Y may not".

## Trade-off Analysis

B costs one table, one middleware and an authz matrix. It buys per-manager control and a clean audit trail. The matrix test is the real cost and the real protection: it is how the "no loopholes" standard is kept.

## Consequences

- Easier: adding a capability (one constant, one route tag); explaining to the owner who can do what.
- Harder: every new manager route needs a capability tag or an explicit "owner only" tag, or CI fails.
- Revisit: multi-owner (maker-checker on capability changes); time-boxed grants (expire after N days); a request-access flow.

## Action Items

1. [x] Migration 070: `manager_capabilities`, RLS, check constraint rejecting non-delegable names.
2. [x] `RequireCapability` on `/manager` routes. Matrix test covers owner, manager with grant, and manager without grant. A nil store skips enforcement so existing suites stay green. `cmd/server` always sets the store.
3. [x] Lifecycle endpoints, audit events, cache invalidation test.
4. [ ] Owner screen with presets ("Front desk", "Kitchen and maintenance", "Accounts-lite") and a manager "request access" action that creates an owner notification.
5. [ ] Isolation tests: manager of property A cannot read or change anything in property B.

---

## Addendum (2026-10-22): Fail-Closed Authorization and Migration 084

**Standard:** ASD-STE100  
**Method:** First-Principles Verification  

### 1. Problem Description
- `CapabilityRepo.HasActive` previously checked if zero capability rows existed for a manager.
- When zero rows existed, `HasActive` returned `true`. This permitted unconfigured managers full access.
- `CapabilityRepo.ListActive` fabricated a default list of 8 capabilities when zero rows existed.
- `OwnerCreateManager` ignored error returns from `Grant()` calls.
- These conditions violated the fail-closed security invariant.

### 2. Resolution
1. **Migration 084:** 
   - Backfills all 13 delegable capabilities for active managers with zero capability rows.
   - Runs idempotently with `ON CONFLICT DO NOTHING`.
2. **CapabilityRepo Hardening:**
   - Removed all zero-row fallback logic from `HasActive` and `ListActive`.
   - If a manager has zero rows, `HasActive` returns `false` and `ListActive` returns an empty slice.
3. **API Handler Error Propagation:**
   - `OwnerCreateManager` checks errors returned by `CapabilityStore.Grant()`.
   - If a grant fails, the handler aborts and returns an error.
4. **Test Verification:**
   - Table-driven unit tests in `internal/postgres/capability_repo_test.go` assert fail-closed behavior.

