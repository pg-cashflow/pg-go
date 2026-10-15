---
name: Auth Module 3-Roles
overview: "Ground-truth SDLC plan: P0-A session fix (verified bug), P0-B slot invites + occupancy state machine + frozen terms, P0-C typed onboarding + KYC tier router + auto-activate, P1 waitlist commands + referral payout. HEAD migration 070."
todos:
  - id: p0-a-session
    content: "finishLogin allow pending_allocation; JoinProfile IssueSession + token/user; table-driven tests; CONTRACT message"
    status: completed
  - id: p0-b-schema
    content: "Migration 071+ slot_invites, occupancy counters/state, property onboarding_settings; ADR"
    status: completed
  - id: p0-b-invites
    content: "Owner+manager slot CRUD (capabilities); resolve at firebase; immutable terms at issuance; expiry job"
    status: completed
  - id: p0-b-capacity
    content: "Occupancy state machine in TX; over_capacity separate capability; distinct APIs not shared 409 semantics"
    status: completed
  - id: p0-c-onboarding
    content: "Fixed occupation_type fields + profile_schema_version frozen at invite; auto-activate TX; GET /me/onboarding"
    status: completed
  - id: p0-c-kyc
    content: "Tier router DigiLocker→Cashfree doc upload/OTP path→manual pending_manual_review; non-blocking login"
    status: completed
  - id: p0-hygiene
    content: "Idempotency-Key POST /join; per-phone firebase rate limit; join_request TTL job; owner join notify stub"
    status: completed
  - id: p0-authz-tests
    content: "Gate 06 matrix + concurrency/IDOR tests per exit criteria"
    status: pending
  - id: p1-waitlist
    content: "join_waitlist, offer_waitlist_slot, accept_waitlist_offer, offer expiry; vacate prefers waitlist over over-cap slot"
    status: pending
  - id: p1-referral
    content: "Payment-triggered referral credit, holdback window, ledger idempotency, self-referral block — extends domain.Referral"
    status: pending
isProject: true
---

# Auth & admission module — ground-truth plan

**Last verified:** repo state with `migrations/070_manager_capabilities.sql` as HEAD; no `slot_invites` / waitlist / `occupancy_revision` in code.

**User decisions locked:** referral financial payout → **P1**; holdback window → **decide in P1**. Manager `slot_invites.create` in **P0-B**; `slot_invites.allow_over_capacity` **separate** capability (owner-grantable, not bundled).

---

## 1. Ground truth (grep/read verified)

| Claim                                      | Reality in repo                                                                                                                                                                          |
| ------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `slot_invites` (property+floor+room+terms) | **Does not exist.** Only `properties.invite_code` ([`property_repo.go`](internal/postgres/property_repo.go)).                                                                            |
| Live occupancy / `occupancy_revision`      | **Does not exist.** [`computeOccupancyMetrics`](internal/api/handlers_owner_dashboard.go) recomputes on read.                                                                            |
| Waitlist                                   | **Does not exist.**                                                                                                                                                                      |
| `finishLogin` blocks `pending_allocation`  | **Confirmed bug** [`service.go:318-326`](internal/auth/service.go) — non-`active` → `ErrTenantVacated`.                                                                                  |
| `POST /join` session                       | **No re-issue**; message tells client to re-exchange Firebase ([`handlers_join.go:161`](internal/api/handlers_join.go)).                                                                 |
| Happy path broken by design                | `CompleteOnboarding` sets `pending_allocation` + links `tenant_id` → re-exchange hits `finishLogin` → vacated error.                                                                     |
| `RotateRefreshToken`                       | **Correct today** — blocks only `vacated`, not `pending_allocation` ([`service.go:548-554`](internal/auth/service.go)).                                                                  |
| Rent at invite time                        | **Not today.** Rent/room on [`ActivateJoin`](internal/api/handlers_join.go) after tenant profile.                                                                                        |
| Floors/rooms                               | **Real:** `POST /owner/floors`, `POST /owner/rooms` ([`router_owner.go`](internal/api/router_owner.go)).                                                                                 |
| Manager create                             | **Real:** `OwnerCreateManager` ([`handlers_gamification.go`](internal/api/handlers_gamification.go)).                                                                                    |
| Manager capabilities                       | **Real:** migration `070_manager_capabilities.sql`.                                                                                                                                      |
| KYC                                        | **Partially built** — DigiLocker via Cashfree in [`internal/kyc`](internal/kyc); upload/QR per [CONTRACT.md](../CONTRACT.md); jobs `digilocker_reconcile`, `kyc_expiry`. Not greenfield. |
| Referral                                   | **Partial** — [`domain.Referral`](internal/domain/gamification.go), `POST/GET /tenant/referrals` (points-oriented); **not** payment-triggered ledger yet → **P1**.                       |
| Student/working fields                     | **Not in** domain/join handlers → **P0-C** new.                                                                                                                                          |
| `/areas/aadhaar-kyc.md`                    | **Not in this repo**; treat [CONTRACT.md](../CONTRACT.md) + `internal/kyc` as source of truth for KYC.                                                                                   |

This is **build slot-invites + frozen terms + occupancy states + session fix + typed onboarding** on top of flat property invite + manual `ActivateJoin`, not “enhance existing slot flow.”

---

## 2. Consolidated decisions (supersedes earlier plan prose where they conflict)

| #   | Area                | Decision                                                                                                                                                                                                                                                                                                                                                                              |
| --- | ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | KYC                 | One workflow, tiers: **DigiLocker** (existing Cashfree Secure ID link) → **document upload / OCR** (existing `/tenant/kyc/upload`) → **manual** ends `pending_manual_review`, never auto-`verified`. Login **non-blocking**; `require_kyc_before_pay` owner toggle.                                                                                                                   |
| 2   | Terms freeze        | Room/rent/deposit/due-day **immutable** at slot invite issuance. `profile_schema_version` frozen on invite row for in-flight onboarding.                                                                                                                                                                                                                                              |
| 3   | Onboarding fields   | Fixed enums `student` \| `working_professional` \| `other` — **no form-builder**. Field list below; owner configurability deferred until proven need.                                                                                                                                                                                                                                 |
| 4   | Capacity / waitlist | **Distinct commands** (not one `409` semantics): `allow_over_capacity`, `join_waitlist`, `offer_waitlist_slot`, `accept_waitlist_offer`, offer expiry. **Occupancy states** authoritative in DB TX: `physical_capacity`, `normal_allocated`, `pending_reserved`, `over_capacity_allocated`, `available_normal` (see §5). **P1** for waitlist automation. **P0-B** over-capacity only. |
| 5   | Invites             | Owner + manager (`slot_invites.create`); `slot_invites.allow_over_capacity` separate.                                                                                                                                                                                                                                                                                                 |
| 6   | Session             | P0-A first: `finishLogin` + inline `IssueSession` on join.                                                                                                                                                                                                                                                                                                                            |
| 7   | Scale               | Per-room `FOR UPDATE` on `slot_invites` row; single-property 100+ tenants; no multi-owner sharding.                                                                                                                                                                                                                                                                                   |
| 8   | Referral            | **P1** — optional code on join may be stubbed or omitted in P0-C; payout on first rent + holdback + ledger idempotency in P1.                                                                                                                                                                                                                                                         |
| 9   | Idempotency         | `Idempotency-Key` on `POST /join`; separate boundaries for join vs payment vs referral (P1).                                                                                                                                                                                                                                                                                          |

---

## 3. Release sequencing

```
P0-A  Session continuity     (no migration; 2 files + tests)
P0-B  Core admission         (071+ migrations, slot invites, occupancy TX, expiry, capabilities)
P0-C  Onboarding completion  (typed fields, KYC tier in timeline, auto-activate TX, onboarding API)
P1    Waitlist + referral    (vacate hooks, financial referral controls, holdback TBD)
```

Design **P0-C field schema** alongside **P0-B** invite row (`profile_schema_version` on `slot_invites`) so freeze rules are one pattern.

---

## 4. P0-A — Session continuity (implementable spec)

### Root cause (verified)

1. Join creates `pending_allocation` + sets `users.tenant_id`.
2. Join response instructs Firebase re-exchange.
3. `finishLogin` rejects non-`active` → `ErrTenantVacated`.

### Patch 1 — [`internal/auth/service.go`](internal/auth/service.go) `finishLogin`

```go
switch tenant.Status {
case domain.TenantStatusActive, domain.TenantStatusPendingAllocation:
default:
    return "", nil, ErrTenantVacated
}
```

Closed world: only three statuses in [`domain/tenant.go`](internal/domain/tenant.go).

### Patch 2 — [`internal/api/handlers_join.go`](internal/api/handlers_join.go) `JoinProfile`

After successful `CompleteOnboarding`:

- `freshUser, _ := h.AuthUserRepo.GetByID(ctx, claims.UserID)` (do not reuse stale JWT claims).
- `accToken, refToken, _ := h.Auth.IssueSession(ctx, freshUser)`
- `h.setRefreshCookie(...)`
- Response: `token`, `user`, `join`, `tenant`; message **without** re-exchange instruction.

### Tests (table-driven)

| Suite                | Cases                                                                           |
| -------------------- | ------------------------------------------------------------------------------- |
| `finishLogin`        | `active` OK; `pending_allocation` OK; `vacated` → `ErrTenantVacated`            |
| `JoinProfile`        | 200 + `token` + cookie; `user.tenant_id` set; new JWT works on `GET /tenant/me` |
| `RotateRefreshToken` | Pin: `pending_allocation` OK; `vacated` denied                                  |

### Exit criteria

- Manual: Firebase → `POST /join` → tenant route with join response token, no re-exchange.
- Full test suite green including existing vacated/refresh tests.

### Rollback

Revert two commits; no schema.

---

## 5. P0-B — Core admission lifecycle

### 5.1 Schema (migration `071_*.sql` onward)

**`slot_invites`**

- `id`, `property_id`, `floor_id`, `room_id`, `code` (unique), `rent_paise`, `deposit_paise`, `due_day`
- `status` (`open`, `consumed`, `revoked`, `expired`)
- `terms_frozen_at`, `profile_schema_version` (int, e.g. 1)
- `allow_over_capacity` (bool, if this invite was minted under override)
- `expires_at`, `consumed_at`, `consumed_by_user_id`, `consumed_tenant_id`
- `created_by_user_id`, `created_at`
- Optional: `bound_phone` for waitlist offer (P1)

**`properties`**

- `occupancy_revision` bigint default 0, increment on bed-affecting TX

**`property_onboarding_settings`** (or JSON on existing settings)

- `default_rent_paise`, `default_deposit_paise`, `default_due_day`
- `auto_activate` bool
- `reserve_beds_for_open_invites` bool default true
- `max_extra_beds_per_room` int
- `slot_invite_default_ttl_days` int default 14
- `waitlist_enabled` bool (P1 behavior)

**Room occupancy (authoritative counters or derived in TX — pick one in ADR; prefer transactional counts updated in same TX as allocate/deallocate)**

Per `room_id`:

- `physical_capacity` (from `rooms.capacity`)
- `normal_allocated` (active + pending_allocation tenants + open invites if reserving)
- `over_capacity_allocated` (tenants/invites flagged over-cap)
- `available_normal = physical_capacity - normal_allocated` (floor 0)

Dashboard [`GET /owner/occupancy`](internal/api/handlers_owner_dashboard.go) becomes **read view** of these values, not sole source of truth.

### 5.2 Terms immutability

- On `POST /owner/slot-invites`, copy rent/deposit/due_day + `profile_schema_version` into row; **no PATCH** of terms on open invite (revoke + re-mint only).
- Tenant join must not accept body overrides that disagree with slot row.

### 5.3 Firebase / join resolution

1. Resolve code → slot row OR `properties.invite_code` (master path uses property defaults + `profile_schema_version` from settings).
2. Stash `slot_invite_id` on join request / user session context until `POST /join` consumes in TX.

### 5.4 Atomic consume TX order

`FOR UPDATE` slot row → validate open + not expired → validate capacity rules → create/update tenant → `AssignTerms` if auto_activate → mark consumed → bump `occupancy_revision` → return snapshot in response.

### 5.5 Over-capacity (P0-B)

- **409** `slot.room_full` when `available_normal <= 0` and no override.
- Owner/manager with **`slot_invites.allow_over_capacity`** may pass `allow_over_capacity: true` + `reason` (audit log).
- Not the same as waitlist (P1).

### 5.6 Owner / manager APIs

| Method | Path                                | Capability                           |
| ------ | ----------------------------------- | ------------------------------------ |
| POST   | `/owner/slot-invites`               | owner or `slot_invites.create`       |
| GET    | `/owner/slot-invites`               | owner or `slot_invites.create`       |
| POST   | `/owner/slot-invites/:id/revoke`    | same                                 |
| POST   | `/owner/slot-invites` with over-cap | + `slot_invites.allow_over_capacity` |

Bootstrap gate: **422** `onboarding.room_master_missing` if property has zero rooms.

### 5.7 Jobs

- `cmd/invite-expiry` (or existing job binary): `open` + `expires_at < now` → `expired`, release reservation, bump revision.

### 5.8 P0-B exit criteria

- Concurrency test: two joins one slot → one succeeds.
- Cross-property slot code → 403/404.
- Terms on invite cannot change after create.
- Expired invite does not consume.

---

## 6. P0-C — Tenant onboarding completion

### 6.1 Occupation types (fixed fields — no form-builder)

**Common (required):** name, phone (login), photo, permanent_address, current_address, emergency_contact_name, emergency_contact_phone (≠ login), consent, move_in_date (server may set `joined_on`).

**`student`:** college_name, course, year, student_id_text (optional), parent_guardian_name, parent_guardian_phone.

**`working_professional`:** employer_name, work_area (optional), employee_id_text (optional), reference_contact_name/phone (optional).

**`other`:** free-text occupation_note (optional); common fields only.

Minors: if DOB &lt; 18 → require guardian fields (ADR-011); disable gamification flags (existing pattern).

Store on `join_requests` + `tenants`; validate against `profile_schema_version` from invite.

### 6.2 Auto-activate

- If slot invite has terms OR property `auto_activate` + master path defaults complete → `AssignTerms` in same TX as join (replaces default owner `ActivateJoin` path for invite-in).
- Keep `ActivateJoin` as **exception** manual override for legacy/master edge cases.

### 6.3 KYC in timeline (do not re-architect `internal/kyc`)

- `GET /me/onboarding` stages: `authenticated` → `profile` → `kyc_optional` → `active` / `payable`.
- Tier UI order: DigiLocker initiate → upload fallback → manual review state.
- `require_kyc_before_pay` gates `IsPayable()` only.

### 6.4 P0 hygiene (same release)

- `Idempotency-Key` on `POST /join`
- Per-phone rate limit on `/auth/firebase`
- `join_requests` pending TTL + cleanup job
- Owner join notification stub (outbox/SMS)

### 6.5 Referral (P1 only)

- P0-C: **no** referral code on join required; existing `/tenant/referrals` unchanged.
- P1: optional `referral_code` on join; credit on first rent with holdback + ledger — extend [`Referral`](internal/domain/gamification.go).

---

## 7. P1 — Waitlist + referral

### 7.1 Waitlist commands (distinct from over-capacity)

| Command                 | Actor        | Effect                         |
| ----------------------- | ------------ | ------------------------------ |
| `join_waitlist`         | Tenant       | FIFO row per `room_id`         |
| `offer_waitlist_slot`   | System/owner | Mint bound slot invite, notify |
| `accept_waitlist_offer` | Tenant       | Atomic consume + join          |
| Offer expiry job        | System       | Next waiter                    |

**Vacate precedence:** fill **normal** bed via waitlist first; only then re-offer over-capacity slots if any.

### 7.2 Referral (P1)

- Payment-triggered (not signup); holdback days **TBD in P1 spec**
- Self-referral block via KYC `identity_hash` / phone
- Immutable reward amounts at referral record creation
- Idempotent payment webhook handler + ledger + reversal on refund

---

## 8. Role journey recommendations (in scope)

| Role    | P0 behavior                                                                                      |
| ------- | ------------------------------------------------------------------------------------------------ |
| Tenant  | Session until logout; timeline via `/me/onboarding`; no Firebase re-exchange after join          |
| Owner   | Bootstrap floors/rooms → mint slot codes with frozen terms; occupancy updates in write responses |
| Manager | `slot_invites.create` when granted; not `allow_over_capacity` unless separately granted          |

Walk-in: keep [`POST /owner/tenants`](internal/api/router_owner.go) + [`attach-phone`](CONTRACT.md).

---

## 9. Verification

- **ASD-STE100** one assertion per threat row (authz matrix).
- **Karpathy:** one property / one floor / one room / one slot → e2e join → k6 auth+join burst ([Gate 09](wayfinder/tickets/track_r09_gate09_k6_load_and_soak.md)).
- **CONTRACT.md** updates: join response tokens, cookie flags, slot invite routes, deprecate re-exchange message.

---

## 10. Explicit non-goals (P0)

- Form-builder / per-owner dynamic required fields
- Multi-property concurrent tenancy on one phone
- Referral payout (P1)
- Waitlist automation (P1)
- Redis / WebSocket occupancy

---

## 11. Next action

**Implement P0-A** (patches in §4) before any migration. Then P0-B ADR + `071` migration review.
