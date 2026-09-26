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

## Frontier (Open Tickets)

- **[Ticket 3: Cross-Cutting IDOR Sweep Across All Remaining API Handlers](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_d_cross_cutting_idor_sweep.md)** `wayfinder:research` (Claimed next)
  - Audit all remaining handler endpoints in `internal/api/` (`handlers_tenants.go`, `handlers_dues.go`, `handlers_owner.go`, `handlers_manager.go`, `handlers_finance.go`, `handlers_collector.go`) against property ownership verification.
- **Ticket 4: Departure Settlement Mirror Post-Commit Resilience** `wayfinder:task`
  - Address post-commit best-effort mirror write in `SettleDepartureUnderLock` (evaluate transactional outbox vs reconciliation job).
- **Ticket 5: Audit Untouched Background & Secondary Packages** `wayfinder:research`
  - Sweep untouched packages: `billing`, `collector`, `jobs`, `events`, `join`, `magiclink`, `notification`, `push`, `sms`, `search`, `gamification`, `intelligence`, `roi`, `localization`.
- **Ticket 6: Automated Security & Dependency Vulnerability Audit** `wayfinder:task`
  - Execute Gitleaks secret scan and `govulncheck` in an environment with access to toolchain binaries.

## Not yet specified

- Gateway dispute / chargeback webhook handling (whether ADR-004 covers incoming disputes or requires dead-letter alerting).
- Post-commit journal event reconciliation / dead-letter queue design.

## Out of scope

- RBI Payment Aggregator licensing compliance (confirmed not applicable to accommodation provider).
- Multi-region active-active distributed database replication.
