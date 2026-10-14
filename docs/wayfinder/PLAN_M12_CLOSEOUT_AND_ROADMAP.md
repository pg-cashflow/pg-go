# Plan: M12 Closeout and Module Roadmap

**Branch:** `div_dev`  **Date:** 2026-10-14
**Method:** `system-design` framework (requirements, design, deep dive, scale, trade-offs) + one ADR per decision (`architecture`).
**Evidence rule:** every status below comes from reading the uploaded zip (`pg-go-div_dev (11).zip`). Go was **not** run. The zip needs Go 1.26 (`go.mod`); the review sandbox has Go 1.22 and cannot download the toolchain. No claim here is "tests pass".

---

## 1. Verified status of the M12 remediation report

| ID | Report says | What the zip shows | Verdict |
|---|---|---|---|
| F1 | Payout key must not fall back to `JWT_SECRET` | `cmd/server/main.go:307-326` fatal when production or `CF_PAYOUT_CLIENT_ID` set; `validate.go:94-97` requires payout secrets whenever payout credentials exist | **Verified** |
| F2 | "Fixed" | Fallback `config.go:141-143` still there. Required-check `config.go:183` and `validate.go:110` only fire when the PG secret is missing, with a misleading message. The L1 warning (`validate.go:144`) now reads both env names but still fires on every normal setup | **Not a defect; needs cleanup.** See ADR-031 |
| F3 | 9 job binaries validate | 9 of 9 recurring jobs call `ValidateForRealDeployment`. `migrate` and `sandbox-smoke` are exempt integration CLIs. There are 12 non-server binaries | **Verified**, exceptions documented in Gate 12 |
| F4 | Constant-time metrics token | `router.go:143` uses `subtle.ConstantTimeCompare`; `Config.MetricsSecret` (`config.go:117`); `.env.example:120` | **Verified** |
| F5 | 32-char minimum and key separation | Present in `validate.go`. **Risk:** the 32-char rule also covers `CF_PAYOUT_WEBHOOK_SECRET`, which Cashfree issues. A valid vendor secret could block boot | **Partial. Amend (R2)** |
| F6 | Fatal on key-ring errors | `main.go:317-326` | **Verified** |
| F7 | Dead job removed | Legacy search reindex binary and systemd units removed; doc grep clean | **Resolved** |
| — | `git diff --stat` / `git status` output | The zip has **no `.git`**. Not verifiable here | **Unverified** |

### New findings from this pass

| ID | Finding | Evidence | Rating |
|---|---|---|---|
| F8 | **`pgapp_worker` is wired to nothing.** All 9 jobs open `cfg.DatabaseMaintURL`. No job runs a role guard. In production a job could run as any role, including a table owner. Today `pgapp_worker` and `pgapp_maint` have the same grants and the same policies (069 lines 32-39, 82, 109; 065 line 15), so wiring it gives credential and log separation, not fewer privileges | `cmd/*/main.go` (`NewPool(ctx, cfg.DatabaseMaintURL)`), `guard.go` only called from `cmd/server` | Medium |
| F9 | **ADR-029 D1 looks unimplemented.** ADR-029 says 56 of 69 property tables have no RLS and money-path tables get RLS first. The newest migration is 069 and none extends RLS to those tables. I did not read the `ScopedDB` wiring, so this is an inference | `migrations/`, ADR-029 section 2 | High if confirmed (R9) |
| F10 | Doc drift: Ticket 16 says 12 binaries and migrations 001-044. The zip has 13 binaries and 69 migrations | `track_q_*.md` | Low (R10) |

---

## 2. Requirements and constraints (system-design step 1)

**Functional.** One owner now, 100+ tenants paying monthly rent by per-tenant anniversary date. Manager role runs daily operations. Tenants are mostly students; some are minors, so a guardian pays. Deposit is refundable with documented damages deducted. Staff payroll for 3-4 people by bulk bank transfer. Owner is the only payout approver.

**Non-functional.** Correctness over latency. Integer paise only. Spiky and mostly idle traffic. Grows from 100+ to 1,000+ tenants. PWA only (Android and iOS through the browser). Cost close to zero.

**Constraints.** Solo builder. Hosting is **undecided** (no server yet; the repo ships systemd units, while earlier plans named Cloud Run and Neon). So every job and runbook must work on both targets.

**Unanswered input:** weekly hours. Sprints below are sized S/M/L, not dated.

---

## 3. Architecture (system-design step 2)

```
 PWA (React/TS, one app, role-based nav)
        |  HTTPS
        v
 cmd/server  (Gin) ── handlers ── services (finance, billing, kyc, payout, ...)
        |                              |
        |  webhooks (Cashfree x3)      |  ScopedDB: SET app.current_property_id
        v                              v
   pgapp_app pool (RLS)         PostgreSQL ── ledger_outbox, events
                                       ^
 scheduled job binaries ───────────────┘   (pgapp_worker after ADR-030)
   billing-cycle, reminder, cashfree-poll, ...
```

**Monolith or microservices?** Stay a **modular monolith** plus scheduled job binaries. At 1,000 tenants the load is a few requests per second. Splitting services adds network failure modes inside money paths (a partial ledger write across services) with no benefit. Revisit only if one module needs independent scaling or a different team owns it. **Go stays the right choice**: low idle memory, one static binary, strong typing for money math.

**Module boundaries** are the 12 modules already agreed. The one structural fix: M10 (property operations) has no package; its code is inside `internal/gamification`. Split it before M10 starts.

---

## 4. M12 closeout work

Spec and tickets: `docs/wayfinder/tickets/track_r_m12_closeout.md`. Decisions: ADR-030, ADR-031.

```
R1 F2 cleanup ─┐
R2 F5 amend ───┤
R3 F7 finish ──┴─> R4 cmd validation ratchet ─> R5 jobs on worker role ─> R6 live role-grant test ─┐
R9 RLS D1 audit (parallel, produces follow-up tickets)                                              ├─> R8 clean-cache gate ─> R10 docs
R7 deploy-neutral runbook (after R5) ───────────────────────────────────────────────────────────────┘
```

**Exit gate for M12 (unchanged from your rule):** `go clean -testcache && go test -count=1 ./...` green, plus the live suite with `REQUIRE_DB=1` against a disposable Postgres where the roles exist. Record both outputs in `BASELINE_REPORT.md`. Nothing is "resolved" until someone reads the diff.

**How to run it.** `/implement-spec`, `/to-spec`, `/to-tickets`, `tdd` and `code-review` are skills in your local agent. This chat has no subagents, worktrees or issue tracker, so I cannot run them. The ticket file is written so they can: a spec section, tickets with blocked-by edges, and file-level acceptance criteria. Setup for your agent:
1. Point the tracker at `docs/wayfinder/tickets/` (no external tracker exists).
2. Integration branch: `integ/m12-closeout` cut from `div_dev`. PR into `div_dev` only (no `develop`, no `main`).
3. Frontier on day one: R1, R2, R3, R9. Merge each ticket, then take the next ready ticket.

---

## 5. Module roadmap (SDLC gate per module)

**Gate, in order:** 1 journeys → 2 ADR (link existing ADR-001..029, do not rewrite) → 3 data (migration, RLS, immutability) → 4 API (authz matrix row per route per role) → 5 tests (unit, authz, isolation, money invariants, journey) → 6 docs (contract, Postman) → 7 ops (job, runbook) → 8 exit (clean-cache green). A module closes only when all eight pass. UI for a module is a **thin PWA slice built right after its backend closes**, so missing fields show up early.

**Cross-cutting UX rules (apply to every module)**
- Role homes: Tenant "what do I owe, pay now"; Manager "today"; Owner "what needs my attention". Backend already has `/tenant/dues`, `/manager/finance/today`, `/owner/dashboard/summary`.
- A solo owner who also acts as manager sees **one** merged "Today" view, with no manager screens.
- Money is always shown in rupees with the exact paise source; every amount has a plain-language "why".
- Every destructive or money action: confirmation with the numbers, an undo window where the ledger allows it, a reason field where it does not.
- A route that is switched off returns 404/403. The UI hides it too, but hiding alone is never the control.
- Localization verified for every new string; students and guardians may not read English.
- Design tokens (color, spacing, type, one component set) before the first screen. Official-feeling palette, not template defaults.

| # | Module | Done means (backend) | UX must-haves | Known gaps from `router.go` (confirm which are wanted) |
|---|---|---|---|---|
| **M12** | Platform | Section 4 | Operator "what is misconfigured" startup report in plain text | none |
| **M1** | Identity & Access | Phone OTP, sessions, step-up, join. Decide on dropping tenant magic-link (keep manual-upload fallback for gateway downtime) | One-tap OTP, remember device, session list with revoke | Manager lifecycle: only `POST /managers` exists |
| **M1b** | Delegated Permissions | ADR-032: capability table, `RequireCapability`, audit | Owner presets ("Front desk", "Kitchen and maintenance"); manager "request access" button that notifies the owner | No list, deactivate or reset for managers |
| **M4** | Rent Collection | Anniversary billing, per-due QR, server-computed pay options (oldest 1, oldest 2, clear all), auto-mark paid, bulk cash-paid, no late fees | Tenant pay screen in 2 taps; share link a guardian can pay without logging in (`/p/:token`); receipt download | Tenant receipt route; manager tenant list and dues view |
| **M5** | Gateway & Reconciliation | Settlements, refunds, disputes, EOD balancer | Owner "needs attention" queue with plain-language variance reasons; gateway-down banner with manual-upload path | none found |
| **M6** | Ledger & Finance | Journals, expenses, period lock, statements, quarterly and half-year summaries | Manager posts an expense with a receipt photo in under a minute; setup-cost and breakeven card; fixed vs variable split | Floor/room removal |
| **M7** | Money Out | Payee registry, payroll for 3-4 staff, SBI-format bulk-transfer export, owner-only approval with step-up | Payslip share for each staff member; "who is unpaid this month" list | Manager attendance marking is owner-only today (make it a capability) |
| **M3** | KYC / Aadhaar / DPDP | Upload with auto-fill, DigiLocker, guardian verification for minors, consent and revoke, expiry reminders | Step-by-step progress; fields auto-filled, only the rest asked | Tenant profile edit |
| **M2** | Property & Tenancy | Floors, rooms, join, notice, vacate, damage-based deposit deduction, pro-rata refund on cash actually paid | Departure checklist with live refund preview; tenant sees deposit and departure status | Tenant departure/deposit status view |
| **M10** | Property Operations (new package) | Inspections, violations, meters, kitchen headcount, hazards, complaint/suggestion box | Manager one-screen daily checklist; tenant complaint form with status | Complaint box: confirm it exists |
| **M9** | Engagement | Points, 2/day late penalty (configurable), grace period before deduction, streak decrement not reset, redemption | Tenant sees points and why they changed; owner can switch the whole module off per property | none found |
| **M11** | Comms | Push, SMS, mail, outbox, dead-letter alerts | Preference screen; owner can forward a payment link on WhatsApp in one tap | none found |
| **M8** | Insights | Dashboards, KPI snapshot, rollups | Owner weekly digest; every number drills to its source rows | none found |

**Order:** M12 → M1 → M1b → M4 → M5 → M6 → M7 → M3 → M2 → M10 → M9 → M11 → M8. Run one module per sprint. Run the RLS D1 work (R9 output) **before M4**, because M4 and M5 touch the tables ADR-029 names.

---

## 6. Trade-offs I would revisit as the system grows

- Single `pgapp_worker` role for all jobs. Split per job family once grants are narrowed.
- Modular monolith. Revisit at 10x load or a second team.
- Neon free tier with no backups is an accepted choice, but this database holds the only copy of the ledger. If you ever reverse it, a scheduled `pg_dump` to any free storage is the cheapest step.
- Per-property capability defaults (ADR-032) assume one owner. A multi-owner future needs a maker-checker review of every capability.

## 7. Decisions needed from you (defaults applied if you say nothing)

1. **ADR-030:** wire jobs to `pgapp_worker` (default) or delete the role.
2. **ADR-032 granularity:** per manager, with a per-property default template (default).
3. **Tenant toggles from the owner per property** via `active_modules` (default yes).
4. **Legacy search reindex CLI:** removed; search maintenance is in-database (ADR-012).
5. **Tenant magic-link:** keep only as a gateway-downtime fallback (default).
6. **Weekly hours**, so sprints can get dates.
