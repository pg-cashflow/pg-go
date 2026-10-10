# Wayfinder Map: Production Money-Handling Readiness

## Destination

`div_dev` verified production-ready for money-handling — no open Critical/High findings across Cashfree, payouts/payroll, DigiLocker/Aadhaar/DPDP, and ledger integrity; every claim backed by file:line + independent check; an honest, current gap list for what remains genuinely unverified.

## Notes

- **Domain**: Go fintech backend (PG Cashflow), 1–10 properties / 50–500 tenants / 5–20 staff.
- **Standing Rules**:
  - Correctness and zero data-leakage strictly outrank raw latency at this scale.
  - Integer paise precision only; zero floats in financial calculations.
  - Balanced double-entry journals ($\sum\text{Debits} == \sum\text{Credits}$).
  - Always clear test caches (`go clean -testcache` / `go test -count=1`) so nothing is cached.
  - No finding gets marked resolved without an independent read of the actual diff / code.
  - Plan before making any single edit.
  - Follow Wayfinder discipline: never resolve more than one ticket per session.

## Decisions so far

- [Track 0: IDOR Guards, Error Leak Sanitization & Departure Journal Balance](file:///c:/Users/divak/Downloads/pg-go/internal/api/handlers_payouts.go): Resolved in commit `34ec648`. 4x IDOR guards added to `handlers_payouts.go`, 13 raw `err.Error()` leaks sanitized to `respondErr()`, and `MirrorDepartureSettlement` balances by crediting `outstandingDuesNettedPaise` to `AcctRentRevenue`.
- [Track A: KYC & DigiLocker Subsystem Audit](file:///c:/Users/divak/Downloads/pg-go/internal/kyc/service.go): Verified clean. RSA-2048 UIDAI QR signature verification is fail-closed, DPDP consent gating with atomic PII wipe in `RevokeConsent`, distributed 30s in-flight mutex lease with 60s pending verification cooldown, zero raw 12-digit Aadhaar storage, property isolation verified on all 3 KYC routes.
- [Track B: Cashfree Gateway Ingress, Isolation & Refund Fail-Safe](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_b2_cashfree_isolation_tdr_refunds.md): Verified clean in commit `af4ed81`. Timing-safe `hmac.Equal`, fail-closed 503 on unconfigured secret, replay tolerance, audit ledger in `webhook_events`, server-side amount cross-check against intent, `CASHFREE_ENV` sandbox vs. prod URL isolation, tenant TDR surcharge protection, and transaction-safe refund failure allocation releases with webhook recovery. Two error leaks sanitized via Track 0.5 in `handlers_pay.go:1336,1364`.
- [Track C: Payout & Payroll Tamper Window](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_c_payout_payroll_tamper_window.md): Resolved. Confirmed strict item immutability (zero update/delete query paths for batched items) and property isolation in `CreateBatchFromUnbatchedItems`. Resolved active checksum gate (`hmac.Equal` check against computed checksum + `slog.Error` on tamper), OWASP CSV injection prefix escaping on `ReferenceNumber`, `Purpose`, `PeriodLabel`, `UTR`, and pure integer-paise INR formatting.
- [Track D (Ticket 3): Cross-Cutting IDOR Sweep Across All Remaining API Handlers](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_d_cross_cutting_idor_sweep.md): Resolved. Comprehensive sweep across all handlers in `internal/api/`. Remediated IDOR gaps in `GetLeakage`, `RecommendationAction`, `TenantRedeem`, `TenantVoteMenuPoll`, and manager/owner operations. Added unit tests in `handlers_finance_test.go` and `handlers_gamification_test.go`.
- [Track E (Ticket 4): Departure Settlement Mirror Post-Commit Resilience](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_e_departure_settlement_resilience.md): Resolved. Implemented dedicated `ledger_outbox_events` table (migration 023) enqueued inside `SettleDepartureUnderLock` before `tx.Commit(ctx)`. Added inline fast-path mirror post-commit, asynchronous `LedgerOutboxWorker` with row-level `FOR UPDATE SKIP LOCKED` and loud `slog.Error` dead-letter escalation on max attempts, and secondary reconciliation check `ReconcileDepartureSettlements`.
- [Track C.2 (Ticket 4a): Payout Batch Dual-Control & Maker-Checker Gating](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_c2_payout_maker_checker.md): Resolved. Batches initialize in `domain.BatchDraft` with `ApprovedBy: nil`. Dedicated `POST /owner/payouts/batches/:id/approve` endpoint enforces affirmative re-acknowledgment (`expected_item_count` and `expected_total_paise`), strict maker-checker rejection (`claims.UserID != batch.CreatedBy`) on multi-owner properties, and graceful fallback to step-up reauthentication on solo-owner properties. `GET /owner/payouts/batches/:id/export` strictly gated on `batch.Status == domain.BatchApproved` (`409 Conflict` on draft).
- [Track C.3 (Ticket 4b): Payout Batch Cryptographic Step-Up Reauth & OTP Verification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_c3_payout_step_up_otp.md): Resolved. Replaced non-empty placeholder string in solo-owner batch approval with real cryptographic step-up reauthentication. Implemented dedicated trigger endpoint `POST /owner/payouts/batches/:id/approve/request-otp` dispatching purpose-parameterized SMS copy. Enforced fail-closed verification via unexpired, single-use, attempt-locked OTP records (preventing replay attacks) and Firebase ID token freshness verification requiring `auth_time` $\le 5$ minutes.
- [Track F (Ticket 5): Audit Untouched Background & Secondary Packages](file:///c:/Users/divak/Downloads/pg-go/internal/jobs/reminder.go): Resolved across all 4 blast-radius buckets: Bucket 1 (`join`, `magiclink`), Bucket 2 (`billing`, `collector`, `jobs`, `events` — remediated atomic `TryLog` dedup in `reminder.go` with `DeleteLog` rollback on send failure in commit `35ee7bf`), Bucket 3 (`notification`, `push`, `sms` — added table-driven unit tests for SMS failover, WebPush lifecycle, and outbox backoff/cursors in commit `74e95af`), and Bucket 4 (`search`, `gamification`, `intelligence`, `roi`, `localization` — verified 100% green test coverage).
- [Track G (Ticket 6): Automated Security & Dependency Vulnerability Audit](file:///c:/Users/divak/Downloads/pg-go/go.mod): Resolved in commit `8e55f48` and persisted in `6587ac1`. 28/28 commits clean under Gitleaks (0 leaks found, verified via `gitleaks detect -v` and `gitleaks-report.json`). `govulncheck` call-graph scan verified 0 reachable symbol vulnerabilities across 42 packages (`govulncheck-report.json`). Documented permanent module-level deprecation advisory `GO-2026-5932` (`x/crypto/openpgp`) with 0 reachable call sites. Transitive advisories cleared by bumping `grpc` to `v1.83.2` and `golang.org/x/crypto` to `v0.56.0`.
- [Track H (Ticket 7): Capacity & Connection-Pool Hardening (1000+ Concurrent Scale)](file:///c:/Users/divak/Downloads/pg-go/internal/postgres/db.go): Resolved. Configurable `DATABASE_MAX_CONNS` (default 25) with proportional `MinConns` in `db.go`; `pg_advisory_xact_lock` dual-locking inside migration tx and `DATABASE_URL_UNPOOLED`/`DIRECT_URL` fallback for PgBouncer/Neon compatibility in `cmd/migrate`; production timeouts (`ReadTimeout: 30s`, `WriteTimeout: 60s`, `IdleTimeout: 120s`) on `http.Server` in `cmd/server/main.go`; and memory-bounded rate limiting (10,000 max entries, 10m TTL sweep, oldest eviction on saturation) in `internal/api/ratelimit.go`.
- [Track I (Ticket 8): Gateway Dispute & Chargeback Webhook Fail-Safe Handling](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_i_gateway_dispute_handling.md): Resolved. Added Cashfree dispute webhook parser (`DISPUTE_CREATED_WEBHOOK`, `PAYMENT_DISPUTE_CREATED_WEBHOOK`, `DISPUTE_STATUS_UPDATE_WEBHOOK`) with flexible alphanumeric ID deserialization. Integrated fail-safe handler recording `dispute_action_required` in `webhook_events`, loud operator alert via `slog.Error`, and property-scoped in-app notification `EvtPaymentDisputed`. Enforces strict fail-safe isolation and zero automated ledger reversal invariant (a dispute is a contested claim, not an approved refund) while responding HTTP 200 OK to prevent gateway retry storms. Operational response procedure documented in [Dispute & Chargeback Operational Runbook](file:///c:/Users/divak/Downloads/pg-go/docs/runbooks/dispute_chargeback_runbook.md).
- [Track J (Ticket 9): Ledger Outbox Dead-Letter Active Operator Alerting](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_j_ledger_outbox_dead_letter_alerting.md): Resolved. Added `DeadLetterNotifier` interface and `EmailDeadLetterNotifier` in `internal/finance/alert.go` using `mailer.Mailer` with structured HTML forensic metadata and instant SMS backstop (`WithSMSBackstop`). Integrated active escalation into `LedgerOutboxWorker` in `internal/finance/ledger_worker.go`, triggering loud `slog.Error` and dual-channel notifications when events exceed `MaxAttempts`. Enforced boot-time presence via `ValidateForRealDeployment()` in `internal/config/validate.go`. Verified via unit tests in `alert_test.go`, `validate_test.go`, and `ledger_worker_test.go`.
- [Track K (Ticket 10): Continuous Money-Math Invariants & Fuzzing Evals](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_k_money_math_invariants_evals.md): Resolved. Added comprehensive property-based and fuzzing evaluations in `internal/finance/invariants_test.go`. Verified double-entry conservation ($\sum \text{Debits} == \sum \text{Credits}$) across 10,000 randomized departure settlements, 100% fail-closed rejection across 5,000 imbalanced perturbations ($\pm 1$ paise), line degeneracy/negative-paise guards, and exact integer-paise conservation across partial payment decomposition.
- [Track L (Ticket 11): Payout Phase 2 — Cashfree Transfers V2 Integration](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_l_payout_phase_2_cashfree_transfers_v2.md): Resolved. Automated Cashfree Transfers V2 integration (Batch Transfer V2, Beneficiary V2 with account-hash derivation, fundsource_id wiring, dispatch_unknown ambiguous timeout handling, Cashfree approval pending bucket, and ledger mirror reversal). Verified via live database tests and 100% green test suite across all 30 internal packages.
- [Track M (Ticket 12): Staff Attendance & Wage-Calculation Engine](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_m_attendance_and_wage_calc.md): Resolved. Isolated upstream domain for per-property leave policies, daily attendance tracking, mid-cycle proration, and integer-paise wage calculation, feeding into `payout_items` (`PayeeTypeStaff`) without altering the audited payout pipeline. Verified via unit, live postgres, and HTTP integration tests across all 31 internal packages.
- [Track N (Ticket 13): Stream 3 Layer 1 — Cashfree Settlement Ingress & Order-to-Intent Reconciliation](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_n_settlement_reconciliation_layer1.md): Resolved. Ingress for Cashfree settlement webhooks and order-level API polling (2025-01-01 / 2026-01-01 polymorphic parser), order-to-intent matching, integer paise MDR/GST allocation, balanced double-entry journals, human-gated discrepancy review with maker-checker step-up, cross-property IDOR standardization (404), and continuous property-based money-math invariant evals.
- [Track O (Ticket 14): Stream 3 Layer 2 — Bank Statement Ingress & Unidentified Deposit Reconciliation](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_o_bank_statement_ingress_layer2.md): Resolved. Direct CSV bank statement ingress (SBI/HDFC parsers with composite dedup key), deterministic Tier 1 vs heuristic Tier 2 segregation, double-entry quarantine invariant (Dr bank / Cr unapplied_receipts), human-gated discrepancy resolution with dual control/step-up, and continuous property evaluation asserting bank debit conservation.
- [Track P (Ticket 15): Stream 3 Layer 3 — End-of-Day Multi-Way Settlement Balancer & Audit Trail](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_p_multi_way_settlement_balancer.md): Resolved. Reconciles Cashfree gateway clearing decomposition (Gross = Net + Fees + Tax + Adj), gateway in-transit balance, cleared bank statement inflows/outflows against general ledger journal lines, and Tier 2 unapplied receipt quarantine. Implemented durable daily snapshot store (`daily_settlement_balances`, migration 031), REST endpoints (`/owner/settlements/eod-balance`, `/run`, `/history`), UI integration in `pg-react` with dual tabs and variance breakdown, and 10,000-iteration randomized property invariant evaluations.
- [Track Q (Ticket 16): Final Staging Cutover & End-to-End Deployment Verification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_q_staging_cutover_verification.md): Resolved. Verified static binary build across all 12 `cmd/...` executables with 0 errors, resolved schema checksum drift, successfully applied and verified all 44 migrations (001–044) with zero drift, verified 100% test suite passing across all packages and invariant property evaluations with zero integer-paise drift.

## Frontier (Open Tickets)

> **Active Specification**: [Test Readiness Specification & Verification Gates](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)  
> **Methodology**: ASD-STE100 (Simplified Technical English) & Karpathy Test Discipline

### Tier 1: Pull Request Frontier (Blocking Gates)
1. [Track R.1 (Ticket 17): Gate 01 — Test Skip Audit & Zero Silent Skip Enforcement](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r01_gate01_skip_audit.md) `[Closed]`
   - *Dependencies*: None.
   - *Resolved By*: `scripts/audit_skips.go` enforcing zero unflagged skips when `REQUIRE_DB=1`.
2. [Track R.2 (Ticket 18): Gate 02 — Package Coverage Floors & Coverprofile Gating](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r02_gate02_coverage_gate.md) `[Closed]`
   - *Dependencies*: Gate 01.
   - *Resolved By*: `scripts/check-coverage.go` gating package statement coverage floors (`finance` 85%, `payment` 85%, `auth` 80%, `api` 75%).
3. [Track R.3 (Ticket 19): Gate 03 — API Contract & Router Response Schema Validation](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r03_gate03_api_contract_tests.md) `[Closed]`
   - *Dependencies*: Gate 01.
   - *Resolved By*: `internal/api/contract_test.go` verifying 50+ route inventory, envelope wrappers, error schema, and integer paise precision.
4. [Track R.4 (Ticket 20): Gate 04 — End-to-End Money Lifecycle & Double-Entry Conservation](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r04_gate04_e2e_money_flow.md) `[Closed]`
   - *Dependencies*: Gate 01, Gate 03.
   - *Resolved By*: `internal/api/e2e_money_lifecycle_test.go` verifying full join-due-checkout-webhook-ledger-refund flow with exactly 0 paise drift.
5. [Track R.5 (Ticket 21): Gate 05 — Gateway Webhook Replay, Out-of-Order & Concurrency](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r05_gate05_webhook_replay_ordering.md) `[Closed]`
   - *Dependencies*: Gate 04.
   - *Resolved By*: `internal/api/webhook_concurrency_test.go` verifying sequential duplicate replay, 10-goroutine stampede (0 double credits), out-of-order refunds, and 0 deadlock delta.
6. [Track R.6 (Ticket 22): Gate 06 — Multi-Role Authorization & Cross-Property IDOR Matrix](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r06_gate06_authz_matrix.md) `[Closed]`
   - *Dependencies*: Gate 03.
   - *Resolved By*: `internal/api/route_role_matrix_test.go` asserting 401 anonymous, 403 role isolation, and property boundary enforcement.
7. [Track R.7 (Ticket 23): Gate 07 — Schema Migration Freshness, Upgrades & Lock Safety](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r07_gate07_migration_tests.md) `[Closed]`
   - *Dependencies*: None.
   - *Resolved By*: `internal/postgres/migration_gate07_test.go` validating 001–044 continuity, idempotency, table/index invariants, and lock safety DDL linting.

### Tier 2: Nightly & Extended Frontier
8. [Track R.8 (Ticket 24): Gate 08 — Process Crash, Network Fault & Poison Recovery](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r08_gate08_crash_recovery.md) `[Closed]`
   - *Dependencies*: Gate 04.
   - *Resolved By*: `internal/finance/physical_crash_test.go` and `crash_proof_test.go` verifying uncommitted transaction rollback, outbox recovery with `SKIP LOCKED`, and deduplication.
9. [Track R.9 (Ticket 25): Gate 09 — k6 Open-Model Load, Soak & Post-Run SQL Invariant Gate](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r09_gate09_k6_load_and_soak.md) `[Resolved]`
   - *Dependencies*: Gate 04, Gate 05.
   - *Resolved By*: Fake gateway server, k6 read/checkout/soak scenarios, post-run SQL invariants.
   - *Target*: k6 50–100 RPS open model, fake gateway stub, 60m soak, post-run SQL 0-drift audit.
10. [Track R.10 (Ticket 26): Gate 10 — Native Go Fuzzing for Parser Boundaries](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r10_gate10_native_fuzzing.md) `[Closed]`
    - *Dependencies*: None.
    - *Resolved By*: `testing.F` across 5 parsers: `utr_fuzz_test.go`, `webhook_fuzz_test.go`, `phone_fuzz_test.go`, `csv_fuzz_test.go`, and `qr_fuzz_test.go`.
11. [Track R.12 (Ticket 28): Gate 12 — Background Job CLI Flags, Signal Handling & Exit Codes](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r12_gate12_job_binaries_tests.md) `[Closed]`
    - *Dependencies*: None.
    - *Resolved By*: `internal/jobs/jobs_cli_test.go` asserting fail-fast on missing env, dormant cashfree poll, migration execution, and graceful SIGTERM shutdown across all 12 binaries.
12. [Track R.13 (Ticket 29): Gate 13 — Calendar Month-End Clamping, Leap Year & Timezone Invariants](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r13_gate13_time_calendar_tests.md) `[Closed]`
    - *Dependencies*: None.
    - *Resolved By*: `internal/billing/calendar_invariants_test.go` verifying 12-month due-day clamping, Feb 28/29 leap year rollover, and IST midnight transitions.
13. [Track R.14 (Ticket 30): Gate 14 — Privacy, DPDP PII Masking & Log Leak Guards](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r14_gate14_privacy_pii_tests.md) `[Closed]`
    - *Dependencies*: None.
    - *Resolved By*: `internal/kyc/privacy_pii_test.go` verifying zero 12-digit Aadhaar regex matches in application logs, tenant response masking, and atomic DPDP consent wipe.

### Tier 3: Weekly & Pre-Release Frontier
14. [Track R.11 (Ticket 27): Gate 11 — Scheduled Cashfree Gateway Sandbox Smoke Harness](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r11_gate11_sandbox_gateway_smoke.md) `[Open]`
    - *Dependencies*: None.
    - *Target*: Automated weekly scheduled run of `cmd/sandbox-smoke` isolated from PR gates.


## Out of scope

- RBI Payment Aggregator licensing compliance (confirmed not applicable to accommodation provider).
- Multi-region active-active distributed database replication.
