# Ticket 17: Track R — M12 Platform Closeout (Spec + Task Graph)

- **Type**: `wayfinder:spec`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](../map.md)
- **Plan**: [PLAN_M12_CLOSEOUT_AND_ROADMAP.md](../PLAN_M12_CLOSEOUT_AND_ROADMAP.md)
- **ADRs**: 030 (job role), 031 (webhook secret)
- **Integration branch**: `integ/m12-closeout` from `div_dev`. PR target: `div_dev`.
- **Standing rules**: integer paise; clear test cache; no ticket resolved without an independent read of the diff; one ticket resolved per session.

## Spec

Close every open M12 item with code, tests and docs so the platform module can be called done:

1. F2, F5, F7 finished as decided in ADR-031 and the plan.
2. Every runnable binary either validates real-deployment config or is explicitly exempt, enforced by a CI ratchet.
3. Background jobs connect as `pgapp_worker` and refuse to start in production with a wrong role (ADR-030).
4. The RLS state of the money-path tables is known and written down (ADR-029 D1).
5. A deploy runbook works for both a systemd VPS and Cloud Run.
6. A recorded clean-cache baseline, including the live-database suite.

Out of scope: new product features, UI, and the narrowing of worker grants (follow-up ticket from R6).

## Task graph

| Ticket | Size | Blocked by | Status |
|---|---|---|---|
| R1 Webhook secret cleanup | S | — | Resolved (`eb18507`) |
| R2 Payout secret rule amendment | S | — | Resolved (`eb18507`) |
| R3 Finish dead-job removal | S | — | Resolved (`4361021`) |
| R9 RLS D1 status audit | M | — | Resolved (`docs/wayfinder/RLS_D1_STATUS.md`) |
| R4 Command validation ratchet | S | R3 | Resolved (`4361021`) |
| R5 Jobs on `pgapp_worker` + guard | M | R4, ADR-030 | Resolved (`6328437`) |
| R6 Live role-grant test | M | R5 | Resolved (`role_grants_live_test.go`) |
| R7 Deploy-neutral runbook | M | R5 | Resolved (`docs/runbooks/deploy_and_jobs.md`) |
| R8 Clean-cache baseline gate | S | R1, R2, R6, R7 | Resolved (`BASELINE_REPORT.md`) |
| R10 Docs drift fix | S | R8 | Resolved (`map.md`, `track_q_...`) |

---

## R1 — Webhook secret cleanup (ADR-031)

**Files:** `internal/config/config.go:141-143, 183-185`, `internal/config/validate.go:110-112, 143-146`, `internal/config/validate_test.go`.
**Do:**
- Keep the fallback at `config.go:141`.
- Change both "required" messages to name `CASHFREE_PG_SECRET_KEY`.
- Delete the L1 warning; log `webhook_secret_source` (`pg_secret_key` or `override`) once at startup. Never log the value.

**Acceptance (tests, table-driven, written first):**
- Override set → used. Override empty, PG secret set → PG secret used.
- PG app id set, PG secret empty → error naming `CASHFREE_PG_SECRET_KEY`.
- No PG keys → no error, no warning.
- A correct config produces **zero** warnings from `ValidateWarnings()` (assert the slice is empty).

## R2 — Payout secret rule amendment (ADR-031 D4)

**Files:** `internal/config/validate.go:20-52`, `validate_test.go`.
**Do:** remove `CF_PAYOUT_WEBHOOK_SECRET` from the 32-character loop. Keep the `change-me` check and the required-when-payout-credentials check.
**Acceptance:** a 20-character payout webhook secret passes; `change-me-x` fails; empty with `CF_PAYOUT_CLIENT_ID` set fails; a 31-character `JWT_SECRET` still fails.

## R3 — Finish dead-job removal (F7)

**Files:** delete `deploy/systemd/pg-search-reindex.service` and `.timer`; decide `cmd/search-reindex`.
**Do:** `grep -rn search-reindex` across repo, CI, docs, Makefile. If no live reference except ADR-012 notes, delete `cmd/search-reindex` (default). If kept, add a build tag and say why in its `main.go`.
**Acceptance:** `go build ./...` clean; `grep -rn "pg-search-reindex" deploy` empty; README job table matches `install.sh` `JOBS` array (assert in a shell test or CI step).

## R4 — Command validation ratchet (F3)

**Files:** new `cmd/validate_coverage_test.go` (package `cmd_test`).
**Do:** for every `cmd/*/main.go` except `server`, require either `ValidateForRealDeployment` or the marker comment `//m12:validate-exempt <reason>`. Mark `migrate` ("validates DB URL only; needs no app secrets") and `sandbox-smoke` ("sandbox-only; guarded by CASHFREE_ENV").
**Acceptance:** test passes today; adding a new `cmd/foo/main.go` without either fails the test (add a negative test using a temp dir fixture).

## R5 — Jobs on `pgapp_worker` + guard (ADR-030)

**Blocked on:** your yes/no on ADR-030.
**Files:** `internal/config/config.go` (+`DatabaseWorkerURL`), `internal/postgres/guard.go` (+`ValidateWorkerRole`), 9 job `main.go` files, `.env.example`, `deploy/systemd/*.service` env notes.
**Acceptance:**
- Production: missing `DATABASE_WORKER_URL`, or equal to `DATABASE_URL`/`DATABASE_MAINT_URL`, fails config validation.
- `ValidateWorkerRole` table-driven unit tests reject: wrong role name, superuser, `BYPASSRLS`, table owner. Accepts `pgapp_worker`.
- Non-production falls back to the maint URL with one warning.
- Each of the 9 jobs calls the guard after validation (R4 ratchet extended to check this).

## R6 — Live role-grant test (`REQUIRE_DB=1`)

**Files:** new `internal/postgres/role_grants_live_test.go` (pattern: `step3_maint_rls_live_test.go`).
**Do:** with the three roles present, assert for `pgapp_app`, `pgapp_maint`, `pgapp_worker`: no `UPDATE`/`DELETE` on `financial_journal_entries`, `capital_transactions`, `expense_payments`, `payout_records`, `payout_record_items`; no access to `schema_migrations`; `NOBYPASSRLS`. Run one write per job family as the worker role.
**Acceptance:** green under `REQUIRE_DB=1`. Output feeds a follow-up ticket "narrow worker grants per job family".

## R7 — Deploy-neutral runbook

**Files:** new `docs/runbooks/deploy_and_jobs.md`.
**Do:** two sections, same checklist shape. (a) VPS: systemd units, `EnvironmentFile` permissions (0600, root-owned), `install.sh`, sudoers file review, cloudflared. (b) Cloud Run + scheduler: build, env, migration via `cmd/migrate` with the **unpooled** URL, one scheduled invocation per job. Include: role creation SQL and password handling for both hosts, first-boot checklist, rollback, "what is verified vs not".
**Acceptance:** every command is copy-paste; every env var in `.env.example` appears in an env-matrix table with required-in-production yes/no, derived by script from `validate.go`, not typed by hand. Nothing is marked verified: no server exists. (Reads of `pg-app-deploy-sudoers` and `cloudflared-config.yml` happen in this ticket.)

## R8 — Clean-cache baseline gate

**Do (on a machine with Go 1.26 and a disposable Postgres):**
```
go clean -testcache && go test -count=1 ./...
REQUIRE_DB=1 go test -count=1 ./internal/postgres/... ./internal/finance/...
```
Paste both outputs, the Go version and the commit hash into `BASELINE_REPORT.md`.
**Acceptance:** green, or every failure listed with a cause. A report without raw output does not count.

## R9 — RLS D1 status audit (ADR-029)

**Do:** for `bank_accounts`, `bank_transactions`, `payout_batches`, `payout_items`, `financial_journal_entries`, `property_calendar_tokens`, and `tenants` identity fields: record RLS enabled (yes/no), policy name and predicate, which pool the repo uses, whether `ScopedDB` is on the request path, and whether a cross-property live test exists. Source: migrations, `internal/postgres`, `scripts/sql/rls_property_scope_ratchet.sql`.
**Acceptance:** one table in `docs/wayfinder/RLS_D1_STATUS.md`; each "no" becomes a numbered follow-up ticket with a size. **Run this before M4 starts.**

## R10 — Docs drift fix

**Do:** correct Ticket 16 counts (13 binaries, migrations 001-069); add Track R to `map.md` with the result of R8; add ADR-030/031 to the ADR index if one exists.
**Acceptance:** `ls cmd | wc -l` and `ls migrations | wc -l` match the docs.

---

## Definition of done for Track R

All R tickets resolved with the diff read by a second pass (`code-review` on the integration branch), R8 baseline recorded, and R9 follow-ups ticketed. Then M12 closes and M1 starts.
