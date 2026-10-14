# pg-go codebase map

## Layers (inside-out)

| Layer | Path | Responsibility |
| --- | --- | --- |
| Entry | `cmd/*` | `main`, flags, process lifecycle |
| HTTP | `internal/api` | Gin routes, auth middleware, `respondErr` |
| Services | `internal/finance`, `tenant`, `payment`, … | Business rules |
| Domain | `internal/domain` | Types, invariants, shared errors |
| Data | `internal/postgres` | SQL, RLS-scoped access |
| Schema | `migrations/` | DDL, triggers, RLS policies |

**Wiring:** `internal/app/bootstrap.go` builds `api.Deps` for the server.

## Where to look

| Problem | Start here | Module doc |
| --- | --- | --- |
| Wrong HTTP status / leak | `internal/api/apierr.go` | M12 |
| Cross-property data | `scoped_db`, auth scope middleware | M2, ADR-029 |
| Due not marking paid | `internal/payment`, `handlers_pay_*` | M4 |
| Webhook duplicate / ordering | `handlers_pay_webhooks.go` | M5 |
| Ledger drift | `internal/finance`, `e2e_money_lifecycle_test.go` | M6 |
| Payout / payroll | `handlers_payouts.go` | M7 |
| Search empty / slow | `internal/search`, `search_repo.go` | M12 |
| Job did not run | `cmd/*`, `internal/jobs/lock.go` | M12, ADR-030 |
| KYC / Aadhaar | `internal/kyc`, `handlers_kyc.go` | M3 |

## Naming conventions

| Pattern | Meaning |
| --- | --- |
| `handlers_owner_*.go` | Owner-scoped HTTP adapters |
| `handlers_pay_*.go` | Checkout, webhooks, refunds |
| `*_live_test.go` | Needs real Postgres (`REQUIRE_DB=1`) |
| `*_test.go` (unit) | Hermetic, no DB |
| `internal/apierr.Code` | Stable machine-readable error codes |
| `respondErr` | Only path that may write errors to clients |

Do not merge `internal/apierr` into `internal/api` — handlers import both on purpose.

## Handler file map (HTTP)

| Files | Area |
| --- | --- |
| `handlers_public.go`, `handlers_join.go` | Auth, join, calendar |
| `handlers_owner*.go` | Owner property, tenants, dues |
| `handlers_finance.go` | Ledger, expenses, statements |
| `handlers_payouts*.go` | Payout batches, webhooks |
| `handlers_pay_*.go` | Gateway checkout and PG webhooks |
| `handlers_settlement.go` | Settlement ingress and EOD balancer |
| `handlers_tenant.go` | Tenant self-service |
| `handlers_gamification.go` | Points, rewards, ops |
| `handlers_kyc.go` | Tenant and owner KYC |
| `router_*.go` | Route registration only |

## Karpathy spine (one golden path)

Follow this order when learning money flow.

1. **Inspect the test:** Open [`internal/api/e2e_money_lifecycle_test.go`](../internal/api/e2e_money_lifecycle_test.go). Run `TestE2EMoneyLifecycle_DoubleEntryConservation` with `REQUIRE_DB=1`.
2. **Read contract inventory:** [`internal/api/contract_test.go`](../internal/api/contract_test.go) lists routes and response shapes.
3. **Trace HTTP → service:** From the e2e test, follow handler names into `internal/payment` and `internal/finance`.
4. **Trace SQL:** Follow repos in `internal/postgres/payment_repo.go` and `finance_repo.go`.
5. **Compare baseline:** Run `scripts/sql/ledger_preflight_audit.sql` after reproducing locally.

## Tests and gates

| Gate | Artifact |
| --- | --- |
| Route / schema | `contract_test.go` |
| Money conservation | `e2e_money_lifecycle_test.go` |
| Authz / IDOR | `route_role_matrix_test.go` |
| Coverage floors | `scripts/check-coverage.go` |
| Skip audit | `scripts/audit_skips.go` |

Full gate list: [TEST_READINESS_SPECIFICATION.md](wayfinder/TEST_READINESS_SPECIFICATION.md).

## Frontends

| Package | Serves |
| --- | --- |
| `internal/web` | Embedded pg-react `dist/` on `NoRoute` |
| `web/` (repo root) | Frozen `/app/` shell for local fallback |
