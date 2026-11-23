# PG Cashflow — End-to-End Full-Stack Audit

**Audit date:** 22 November 2026
**Repositories:** `pg-go` and `pg-react` from the two ZIP files uploaded in this conversation
**Release decision:** **HOLD — do not deploy**
**Audit type:** Static source review + available script checks. This is not a production certification.

## 1. Executive decision

The project has several sound design choices: server-side payment confirmation, idempotent gateway commands, durable worker/outbox patterns, webhook signature checks, refund ambiguity handling, payout batch reconciliation, encrypted KYC documents, and role-scoped routes.

The uploaded ZIP baseline contains a release-blocking payment-intent recovery defect, a payout reconciliation RLS gap, and refund-approval policy/database-isolation defects. KYC method defaults conflict with the agreed product journey, and public-page prerendering has a deployment-path risk. A first remediation patch has now been applied to extracted working copies for several of these findings. The payment-intent recovery and refund-approval RLS gaps remain open. The backend build, frontend TypeScript build, and browser journeys could not be completed in this environment.

**Do not remove the deployment hold** until the P1 findings are corrected, the full test matrix passes, PostgreSQL RLS tests pass using the real application roles, and Cashfree sandbox plus reconciliation evidence is available.

## 2. Scope and verification limits

The review covered the Go API, authentication/session code, tenant/property scoping, Cashfree payment/refund/payout clients and workers, KYC service/webhooks, PostgreSQL repositories and migrations, React routing/auth/query state, KYC/payment screens, theme/i18n, service worker, and public-page prerendering. A first remediation patch was then applied to the extracted working copies; the original uploaded ZIPs were not modified.

The extracted repositories contain hundreds of files. This pass prioritised financial, identity, tenant-isolation, and release-critical paths. It is not a claim that every line of every ancillary module has received a formal line-by-line review.

### Verification performed

- Frontend static Node checks: company metadata **7/7**; palette check **pass**; typography check **pass**; policy parity **pass**; error-code parity **91/91 exact matches**.
- Locale key parity after Patch 1: `en-IN` has 1,035 keys; `te-IN`, `ta-IN`, and `kn-IN` have matching key sets. The string scan reports 0 literal strings across the files covered by that checker; this does not prove all visible text is translated.
- Go toolchain: repository declares `go 1.26.0`; environment has Go `1.23.2`. The attempt to obtain the declared toolchain could not reach the Go proxy. Go compilation and Go tests are therefore **not verified**.
- Frontend dependencies are not installed in the extracted workspace. `tsc -b`, Vite build, Vitest, and browser E2E are therefore **not verified**.
- No live PostgreSQL instance, production `pgapp_app` role, Cashfree sandbox credentials, merchant dashboard, or bank statement was available. Live RLS, provider behavior, payout settlement, and end-to-end browser tests remain open.

## 3. Confirmed findings in the uploaded ZIP baseline

### P1 — Payment-intent recovery can release an uncertain checkout

**Evidence**

- `migrations/119_payment_intent_recovery_and_command_fencing.sql:6-30` ranks `dispatch_unknown` intents below `created` and `initiating`, then changes duplicate rows to `superseded`.
- The partial unique index at lines 54-63 does not reserve `superseded` rows.
- `internal/postgres/intent_repo.go:281-315` blocks new checkout for `needs_review`, `initiating`, and `dispatch_unknown`, but not `superseded`.
- `internal/jobs/cashfree_poll.go:171-189` reconciles unpaid provider order status only for `initiating` and `dispatch_unknown`.
- `migrations/120_kyc_inbox_lease_token.sql:25-35` creates `payment_intent_quarantine`, but the application has no read/write workflow for that table.

**Risk**

`dispatch_unknown` means Cashfree may have accepted the order even when the application did not receive the response. The migration can remove that intent from the active reservation, and the application does not then prove the provider order is terminal before it permits a replacement checkout. A late success may still be detected in some poll paths, but the unpaid order is not reconciled once its intent is `superseded`.

This is a **financial-integrity P1**. It creates a possible window for two payable provider orders for one due. It is not proof that every user can double-pay, but the invariant is not safely enforced.

**Required correction**

1. Do not demote an uncertain provider order until the provider confirms a terminal state under a documented consistency policy.
2. Backfill unresolved records into quarantine before changing their statuses.
3. Keep the due reserved while any associated provider order can still accept payment.
4. Make the reconciliation worker read and update quarantine records, with alerting and an operator resolution path.
5. Add migration tests using duplicate sets containing `created`, `initiating`, and `dispatch_unknown`; prove that no unknown order is released.
6. Add concurrent checkout tests and late-payment tests for every status transition.

### P1 — Platform payout exception views can be blocked by `payout_payees` RLS

**Evidence**

- `migrations/118_platform_ops_payout_reconciliation_rls.sql:29-47` gives platform operations cross-property access to `payout_items` and uses `payout_payees` in its policy subquery.
- `migrations/053_fail_closed_rls_safe_uuid_cast.sql:107-130` defines the `payout_payees` policy using the current property or `app.ledger_maintenance`; it has no `app.is_platform_ops` exception.
- `internal/postgres/payout_repo.go:1935-1951` sets `app.is_platform_ops=true` and joins `payout_items` to `payout_payees` for the exception list. The manual reconciliation path also joins these records.

**Risk**

With forced RLS, platform operations can see the batch/item side while the payee side is hidden. The exception list can be empty or manual reconciliation can report an item as missing, even though the payout item exists.

**Patch 1 status**

A new migration, `migrations/121_platform_ops_payout_payees_read.sql`, adds a narrowly scoped `SELECT` policy for `pgapp_app` when `app.is_platform_ops=true`. It does not grant platform-wide writes. This is applied only in the extracted working copy and has not been run against a live database.

**Remaining verification**

Verify that only trusted platform-operations code can set `app.is_platform_ops`, and test the migration using the actual `pgapp_app` role. Confirm that ordinary property users still cannot read another property's payee rows.

### P1 — Manager refund approval requests bypass the owner's delegation policy

**Evidence**

- `internal/api/handlers_pay_refund_approvals.go:49-60` sends manager requests directly to `executeCreateRefundRequest`.
- `internal/postgres/refund_approval_repo.go:42-46` checks the `refunds.request` capability, but does not enforce the property setting `allow_manager_refunds` or the configured maximum.
- The older direct manager refund route in `internal/api/handlers_pay_refunds.go:1102-1118` calls `requireManagerRefunds` and checks the maximum amount.
- `requireManagerRefunds` at lines 1124-1136 rejects requests when the owner has disabled delegation.
- The tests include the older manager cancellation setting, but no test was found for the new `ManagerCreateRefundRequest` path enforcing these rules.

**Risk**

A manager can create owner-approval requests while the owner has disabled manager refunds or set a lower limit. Owner approval is still needed before money moves, so this is not a direct manager refund dispatch. It is a policy bypass and can create unwanted approval-queue work.

**Patch 1 status**

`RefundApprovalRepo.CreateRequest` now checks `allow_manager_refunds` and `manager_refund_limit_paise` in the same transaction as request creation. It locks the settings row `FOR SHARE` after the existing user/capability authorization locks. Missing settings fail closed. Error mapping is added to the handler. Pure policy tests cover disabled delegation, unlimited limit, exact limit, and over-limit cases. The Go tests could not be executed in this environment.

**Remaining verification**

Add/execute repository integration tests for missing settings, concurrent owner setting changes, duplicate idempotent requests, and policy changes while an approval is pending. Decide whether disabling delegation must also cancel or block previously created manager requests at approval time.

### P2 — `refund_approval_requests` has no database row-level security

**Evidence**

- `migrations/112_refund_approval_requests.sql:8-32` creates the table and indexes but does not enable or force RLS or define a policy.
- `internal/postgres/refund_approval_repo.go` reads and updates several records by request ID or payment ID without also requiring `property_id` in the SQL predicate.
- Application handlers check property ownership before performing these actions. I did not establish a direct cross-property API leak from this source review.

**Risk**

The database does not enforce the same property boundary as the handler. A future repository caller or handler mistake could bypass the application check. This is a defence-in-depth gap in a financial approval table.

**Status: OPEN — not changed in Patch 1**

A safe fix requires changing repository method contracts and every query/transaction that reads or mutates this table. Do not add RLS alone: pool-level queries currently do not consistently establish the property GUC, and a policy-only migration could make valid owner requests fail. Pass the property ID through the store interface, run all repository operations in a property-scoped transaction, include `property_id` in predicates, then add `ENABLE`/`FORCE ROW LEVEL SECURITY` and test all paths with `pgapp_app`.

### P2 — Single-owner refund threshold is not wired through settings persistence

**Evidence**

- `migrations/112_refund_approval_requests.sql:34-35` adds `single_owner_refund_threshold_paise`.
- `internal/domain/slot_invite.go` includes the domain field; `handlers_pay_refund_approvals.go:38-46` reads it and falls back to an environment setting.
- `internal/postgres/onboarding_settings_repo.go:22-37` does not select the new column.
- `internal/api/handlers_owner_onboarding_settings.go:40-56` does not accept it in the PATCH body; lines 72-88 do not pass it into the patch.
- `internal/postgres/onboarding_settings_repo.go:115-129` does not persist it.

**Risk**

The property-level setting cannot be configured through the application and is not loaded by the repository. The effective approval threshold can therefore fall back to the account-level environment value instead of the configured property value.

**Patch 1 status**

The extracted working copy now selects and scans `single_owner_refund_threshold_paise`, accepts the tri-state nullable PATCH value, validates non-negative paise, and persists the value. The owner settings screen exposes the delegation toggle, per-request manager limit, and single-owner threshold. Blank threshold means use the account-level default; blank manager limit is explicitly shown as no per-request ceiling.

**Remaining verification**

Run repository/API integration tests and browser tests. Confirm the product owner accepts an unlimited manager request ceiling when delegation is enabled; owner approval is still required before the refund can be dispatched.

### P2 — KYC defaults to DigiLocker in the uploaded ZIP baseline

**Evidence**

- `internal/config/config.go:172` sets `FeatureKYCDigiLocker` to `envBoolDefaultTrue`.
- `src/components/shared/TenantKYCCard.tsx:74-81` treats a missing `methods.digilocker` value as enabled and selects DigiLocker by default.
- OCR results are sent to manual review; the secure QR path verifies the signed QR data before manual review. OCR itself does not auto-approve.

**Risk**

The production default can offer a method the product decision excludes. A missing capability response from an older server also causes the browser to assume DigiLocker is enabled. This makes the client fail open for a method flag.

**Patch 1 status**

The extracted working copy now defaults `FEATURE_KYC_DIGILOCKER` to false in Go and `.env.example`. The frontend shows DigiLocker only when the server explicitly returns `methods.digilocker === true`, and otherwise defaults to upload. Tests were updated for missing and explicit capability values. ADR-005 now records OCR + UIDAI Secure QR + manual review, with DigiLocker opt-in and Aadhaar OTP disabled by PG Cashflow product policy. The old DigiLocker-first ADR is marked superseded.

**Provider-doc correction**

Cashfree's current public material lists Aadhaar OTP, OCR, and DigiLocker verification products. Therefore, comments claiming Cashfree no longer supports Aadhaar OTP are too broad. The decision not to use OTP can remain; describe it as a PG Cashflow product, privacy, and low-friction decision—not as a global Cashfree deprecation.

### P2 — KYC webhook can acknowledge and discard an event when the KYC service is absent

**Evidence**

- `internal/api/handlers_kyc.go:1067-1070` returns HTTP 200 with `status=ignored` when `KYCSvc` is nil.
- Bootstrap can leave the service nil when the KYC identity secret is not configured.

**Risk**

If Cashfree is configured to send callbacks while this service is dormant, the callback is accepted but not stored for later processing. This is only a defect when webhook delivery is expected in the active deployment; a deliberately disabled KYC module can be valid.

**Patch 1 status**

The extracted working copy now returns HTTP 503 instead of HTTP 200 when the webhook signature is valid but `KYCSvc` is absent. A regression test verifies this behavior. The Go test could not be executed in this environment.

**Remaining verification**

Confirm bootstrap fails or clearly alerts when KYC callbacks are configured without a functioning service/inbox. Verify Cashfree webhook delivery configuration and replay behavior in the merchant dashboard; do not rely on this handler test as provider-retry proof.

### P2 — Public landing-page prerender output depends on an unverified host rewrite

**Evidence**

- `scripts/prerender-landing.mjs:249-251` writes `dist/landing.html` only.
- Policy pages write both `dist/<slug>.html` and `dist/<slug>/index.html` at lines 316-322.
- The application route is `/landing` (`src/router.tsx:292-304`).
- `src/sw.ts:26-40` excludes `/landing` and legal paths from the SPA navigation fallback.
- No hosting rewrite/deployment configuration was found in the uploaded repository.

**Risk**

The URL `/landing` requires the hosting layer to map to `landing.html` or to serve the intended prerendered content. The archive does not prove that this rule exists. Without the rewrite, the prerender output may not be served at the expected route.

**Required correction**

Either emit `dist/landing/index.html` like the legal pages, or add a documented host/edge rewrite from `/landing` to `/landing.html`. Test a fresh browser with service worker disabled and confirm HTTP status, canonical URL, metadata, and rendered body for `/landing`, `/terms`, `/privacy`, `/refund-policy`, and `/contact`.

## 4. Patch 1 — changes applied to extracted working copies

These changes are present in the downloadable Patch 1 ZIPs, not in the original uploaded archives. They are source changes only; database migrations have not been applied to any environment.

| Area | Change | Current status |
|---|---|---|
| Payout reconciliation RLS | Added `121_platform_ops_payout_payees_read.sql`, read-only platform-operations SELECT policy | Applied in working copy; live-role test pending |
| Manager refund delegation | Enforce owner toggle and per-request ceiling in the approval-request transaction, after capability locks | Applied in working copy; Go tests pending |
| Refund settings persistence | Wire single-owner threshold through GET/PATCH/repository and add validation | Applied in working copy; DB/API tests pending |
| Owner UX | Add an owner refund-controls card for manager delegation, manager limit, and single-owner threshold | Applied in working copy; browser test pending |
| KYC methods | DigiLocker default is false; frontend fails closed when capability is absent; `.env.example` is explicit | Applied in working copy; full frontend tests pending |
| KYC policy docs | Add ADR-005; mark DigiLocker-first ADR-004 as superseded; remove unsupported provider-deprecation claims | Applied in working copy |
| KYC webhook | Return 503 if the signed callback cannot be processed because KYC service is absent | Applied in working copy; Go tests pending |

**Still open and release-blocking:** payment-intent uncertainty/quarantine and refund-approval database isolation. The landing-page routing issue also needs a deployment-origin test.

## 5. Positive architecture findings

### Authentication and sessions

- Access tokens are held in browser memory rather than persistent browser storage.
- Refresh uses an HttpOnly cookie with strict same-site behavior and secure production handling.
- Refresh rotation is transactional and serializes concurrent refreshes.
- The API client avoids automatically replaying a non-idempotent write after 401 unless the request has an idempotency key.
- The backend uses token-version checks and session-family state for revocation.

**Remaining hardening:** token-version cache TTL is 30 seconds and refresh-family cache TTL is 10 seconds. Local invalidation does not guarantee immediate invalidation on every API instance. For high-risk actions—payout dispatch, refund approval, and role/capability changes—use a shared invalidation channel or a database-backed fresh check. Treat the database as the final authority.

### Manager capability locking

The capability code follows the intended lock order: lock the manager `users` row, then the `manager_capabilities` row, then the domain object. Grant/revoke operations use the same order. Preserve this order across every new manager mutation to avoid time-of-check/time-of-use races.

### Cashfree Payments

- Server-side order creation controls amount and currency.
- The browser receives a payment session, not authority to mark a due paid.
- Return/redirect state is not trusted as final payment proof.
- The backend verifies provider status and handles signed webhook events.
- Settlement is idempotent and checks order, amount, and transaction identity.
- The checkout UI retains an unresolved state instead of allowing a blind immediate retry.

This is the correct shape. Continue using Cashfree's server-side Get Payments/Get Order status and webhook paths as the source of truth; do not trust client callbacks alone.

### Cashfree Refunds — use durable jobs/outbox, not an inline provider call

The repository's refund dispatcher is built around durable local state, stable refund identifiers/idempotency keys, leases, and status recovery after ambiguous provider responses. It moves ambiguous results to review or status lookup rather than creating a new refund with a rotated key.

**Recommendation:** keep refunds asynchronous. The API should validate and record the command/approval in a database transaction and enqueue an outbox event in that same transaction. A worker should claim the event with a lease/fencing token, dispatch using the same deterministic provider identifiers, query status after an ambiguous result, update the ledger exactly once, and alert/reconcile unresolved cases. Do not execute the Cashfree refund call inside the HTTP request. A scheduled job may wake workers or run reconciliation, but the durable database outbox—not a timer in process memory—must own the work.

### Cashfree Payouts V2

- The repository uses the Payouts V2 host and API version `2024-01-01`.
- Batch and item status are treated separately.
- After uncertain batch submission/5XX outcomes, the code queries batch status before resubmission.
- Individual transfers are not treated as settled merely because they reached `SENT_TO_BENEFICIARY`; the code waits for the acknowledged `COMPLETED` state.
- Transfer IDs are stable, payout items have snapshots, payee details are protected, and dispatch concurrency is bounded.

Keep the two-person reconciliation model. Platform Operations must compare Cashfree dashboard/batch evidence, transfer status, bank statement, and UTR before manual correction. Do not let a property owner attest to evidence that only exists in the platform's Cashfree/bank accounts.

## 6. KYC design recommendation

Keep the agreed low-friction journey:

1. Ask for consent with a clear purpose, version, and language.
2. Offer document upload with OCR prefill and UIDAI Secure QR as supported paths.
3. Use OCR only to extract and prefill data; never treat OCR output as proof of identity.
4. Verify Secure QR cryptographically before using its data.
5. Send the result to an authorised human reviewer; record reviewer, evidence, reason, and timestamp.
6. Retain only the minimum permitted data. Purge raw document bytes after the approved retention/decision step defined by the policy; keep masked identifiers and non-reversible deduplication evidence only where legally and operationally justified.
7. Keep viewing the room, dues, receipts, and payment history available before verification. Enforce KYC at the action that needs it, such as dispatching a payment, rather than blocking the entire tenant dashboard.

DigiLocker and Aadhaar OTP should remain off unless the product owner explicitly changes the decision after assessing access, consent, provider entitlements, and privacy impact.

## 7. SSR, CSR, and SSG recommendation

| Route / area | Recommended rendering | Reason |
|---|---|---|
| Landing, terms, privacy, refund policy, contact | **SSG** | Public, cacheable, indexable, no user-specific data |
| Owner portal | **CSR** | Private, role-scoped financial/tenant data; authenticated API is the source of truth |
| Manager portal | **CSR** | Capability checks and property scope must be current |
| Tenant portal | **CSR** | Private dues, payment history, KYC status, and user session |
| Payment return / checkout status | **CSR** | Must fetch fresh server-side provider status; never embed payment truth in HTML |
| KYC return / review | **CSR** | Session-bound and sensitive; fetch the authoritative status after return |
| Invite and join | **CSR** with public metadata only | Invite tokens and session data must not be exposed in shared cached HTML |
| Social preview for public invites | Edge-generated OG metadata | Public metadata only; never include identity/KYC/financial data |

Do not add runtime SSR to the authenticated portals just to claim SSR support. It would increase server/session/cache complexity and could expose private data through incorrect caching. Prefer one React/content source for public SSG pages instead of maintaining large, hand-built HTML strings beside React page components.

## 8. UX and role-flow direction

The present architecture supports a low-friction model. Preserve it and improve the moments where the user needs to act.

### Tenant

- Join through invite; collect only data needed to create the account.
- Show room, rent/deposit, dues, receipts, and payment history immediately after joining.
- Keep KYC status visible as a clear inline banner, not a full-screen gate.
- When the tenant presses Pay, explain the required identity step, preserve the selected due and amount, and return the user to the exact checkout step after approval.
- Keep an explicit “payment status is being confirmed” state after a timeout. Do not offer another payment attempt until the previous provider order is reconciled or authoritatively terminal.

### Owner

- First-run checklist: property details → rooms/beds → staff/manager access → tenant invitations → first dues.
- Use progressive disclosure. Keep advanced finance, approvals, payout controls, and reconciliation out of the primary daily path until needed.
- Put refund delegation limits and dual-approval thresholds in one understandable financial-controls screen. Explain the effective rule before saving.

### Manager

- Show only granted capabilities and the active property context.
- When a task is denied, explain which owner-controlled permission is missing and who can grant it.
- Never show a control that appears actionable but will fail at the API. Keep capability checks authoritative on the server.

### Platform Operations

- Use one queue for unresolved payout/refund/payment exceptions with clear owner, age, evidence, next action, and immutable audit history.
- Require maker/checker separation for manual financial reconciliation.
- Do not present a manual “mark paid” or “mark refunded” control without verified provider/bank evidence and a reason.

## 9. Performance and reliability direction

“Zero lag” cannot be promised without device/network measurements. Set measurable budgets instead:

- Track Core Web Vitals at the 75th percentile: LCP ≤ 2.5s, INP ≤ 200ms, CLS ≤ 0.1.
- Measure API p50/p95 separately for normal reads, auth, payment-intent creation, and worker/provider calls.
- Keep Cashfree network calls out of ordinary CRUD requests. Use bounded worker concurrency and timeouts.
- Preserve lazy route chunks. Prefetch only the likely next route after authentication or on idle; do not preload every role's portal.
- Use pagination/virtualization for large tables and audit logs; do not fetch a full ledger for a dashboard summary.
- Keep query cache and offline passbook data user-scoped. Clear them on logout and account switch. Continue to avoid caching private `/api` responses in the service worker.
- Test on low/mid-range Android devices, constrained CPU, 4G/3G, cold cache, and service-worker update paths.

These are target budgets. No field performance result was measured in this audit.

## 10. Sequential remediation plan

### Gate 0 — Freeze and preserve evidence

- Keep deployment disabled.
- Record current commit IDs for both repositories and the migration version applied in each environment.
- Take a verified database backup before any migration repair.
- Capture existing `dispatch_unknown`, `superseded`, and pending refund/payout records before remediation.

### Gate 1 — Financial integrity P1

1. Repair the payment-intent migration/backfill and quarantine worker. This remains the highest-priority open defect.
2. Run migration 121 in an isolated database and verify platform-operations `payout_payees` SELECT RLS with `pgapp_app`; then verify property isolation.
3. Execute the new manager-refund policy tests and add repository-level tests for missing settings, concurrency, and idempotency.
4. Add regression tests for late payments, uncertain provider orders, cross-property payout exceptions, and duplicate commands.

### Gate 2 — Database and policy hardening

1. Add forced RLS and property predicates to refund approval records. This remains open.
2. Execute the new single-owner threshold GET/PATCH/repository tests and browser flow.
3. Test RLS with `pgapp_app`, `pgapp_worker`, and maintenance roles as appropriate.
4. Confirm audit retention and delete behavior for refund approvals and payout evidence.

### Gate 3 — KYC configuration and operational safety

1. Verify Patch 1 KYC defaults and the ADR-005 product policy in each environment.
2. Keep legacy Aadhaar OTP code only as long as required by historical data and compatibility; do not expose the route.
3. Confirm webhook bootstrap/inbox readiness and provider delivery behavior; the handler now returns 503 when KYC service is absent.
4. Test consent revoke, raw document purge, signed QR tampering, duplicate webhook, worker lease expiry, and manual reviewer audit history.

### Gate 4 — Frontend, SSG, and UX

1. Fix or document `/landing` output routing.
2. Use one source for React public pages and generated static pages.
3. Run TypeScript, Vite, Vitest, and browser E2E.
4. Run full tenant/owner/manager/platform-operations journeys in each supported locale and theme.
5. Run performance profiling on target devices and enforce the budgets above.

### Gate 5 — Cashfree and production readiness

1. Confirm the merchant account has the exact PG, Payouts V2, and Secure ID products enabled; API documentation availability does not prove account entitlement.
2. Verify webhook URLs, secrets, event subscriptions, environment IDs, API versions, IP/network rules, and secret rotation.
3. Execute sandbox scenarios: success, failure, pending, timeout-after-provider-accept, webhook-before-return, return-before-webhook, duplicate webhook, refund timeout, payout batch 5XX, transfer pending, beneficiary rejection, insufficient balance, and late acknowledgement.
4. Compare Cashfree records to local ledger and bank/settlement evidence.
5. Complete production migration dry run, rollback plan, backup restore test, alerting, on-call ownership, and reconciliation sign-off.

**Release rule:** a green unit test suite is necessary but not sufficient. Release only when all financial invariants, live-role RLS checks, sandbox recovery cases, deployment path tests, and external payout/refund evidence pass.

## 11. Source files to start the remediation patch series

| Finding | Primary source files |
|---|---|
| Payment-intent recovery | `migrations/119_payment_intent_recovery_and_command_fencing.sql`, `migrations/120_kyc_inbox_lease_token.sql`, `internal/postgres/intent_repo.go`, `internal/jobs/cashfree_poll.go` |
| Payout RLS | `migrations/053_fail_closed_rls_safe_uuid_cast.sql`, `migrations/118_platform_ops_payout_reconciliation_rls.sql`, `migrations/121_platform_ops_payout_payees_read.sql`, `internal/postgres/payout_repo.go` |
| Manager refund policy | `internal/api/handlers_pay_refund_approvals.go`, `internal/postgres/refund_approval_repo.go`, `internal/api/handlers_pay_refunds.go` |
| Refund approval RLS | `migrations/112_refund_approval_requests.sql`, `internal/postgres/refund_approval_repo.go` |
| Single-owner threshold | `internal/api/handlers_owner_onboarding_settings.go`, `internal/postgres/onboarding_settings_repo.go`, `internal/postgres/onboarding_settings_patch.go`, `internal/domain/slot_invite.go` |
| KYC method defaults | `internal/config/config.go`, `.env.example`, `src/components/shared/TenantKYCCard.tsx`, `internal/api/handlers_kyc.go`, `docs/adr/005-low-friction-tenant-kyc-policy.md` |
| Landing SSG routing | `scripts/prerender-landing.mjs`, `src/sw.ts`, hosting/edge rewrite configuration |

## 12. External Cashfree documentation checked

- Cashfree Checkout Dev Studio demonstrates server-side order creation and a server-side Get Payments step after checkout; browser redirects/callbacks are not final payment proof: https://www.cashfree.com/devstudio/preview/pg/web/checkout
- Cashfree webhook verification guidance: https://www.cashfree.com/devstudio/preview/pg/tools/webhookVerification
- Cashfree's public identity documentation currently lists Aadhaar OTP, OCR, and DigiLocker as separate verification options; product/account enablement must still be confirmed with the merchant account: https://www.cashfree.com/digital-kyc/ and https://www.cashfree.com/aadhaar-verification/
- Cashfree OCR documentation describes OCR as extraction and prefill; it is not a substitute for PG Cashflow's manual identity decision: https://www.cashfree.com/aadhaar-ocr-verification/

---

**Final status: HOLD.** Patch 1 reduces several baseline gaps, but the payment-intent recovery invariant and refund-approval RLS gap remain open. The payout RLS patch and manager-refund/threshold changes still require Go, database-role, and end-to-end integration tests. Do not deploy or apply migrations until those gates pass.
