# pg-go debugging guide

Follow sections in order (Karpathy recipe). See [TEST_READINESS_SPECIFICATION §4](wayfinder/TEST_READINESS_SPECIFICATION.md#4-karpathy-test-recipe).

## 1. Inspect data first

**Precondition:** You have a failing route, job, or test name.

1. Run `go test -count=1 -v -run '<TestName>' ./internal/...` with `REQUIRE_DB=1` when the test name ends in `Live` or uses postgres.
2. Inspect JSON from `httptest` or `curl` before reading handler code.
3. Inspect SQL rows for money bugs:

```bash
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/sql/ledger_preflight_audit.sql
```

4. After load tests, run `scripts/loadtest/post_run_invariants.sql`.

**Expected outcome:** You see either zero paise drift or a specific row id to trace.

## 2. Check initialization state

**Precondition:** `.env` exists from `.env.example`.

1. Run `go run ./cmd/migrate/`.
2. Run `go run ./cmd/server/`.
3. Verify `GET http://localhost:8080/healthz` returns `{"status":"ok"}`.
4. Confirm pool URLs: `DATABASE_URL` (app), `DATABASE_MAINT_URL` (maint), `DATABASE_WORKER_URL` (jobs).

Production refuses startup when `ValidateForRealDeployment` or `ValidateStartupRoles` fails.

**Expected outcome:** Server listens; role guard passes.

## 3. Overfit one example (golden path)

**Precondition:** Postgres available with migrations applied.

```bash
go clean -testcache
set REQUIRE_DB=1
go test -count=1 -run TestE2EMoneyLifecycle_DoubleEntryConservation ./internal/api/
```

Read the test file top to bottom. Mirror the same HTTP sequence with Postman ([`postman/`](../postman/)) or `curl`.

**Expected outcome:** Test passes; ledger sum matches payment amount in paise.

## 4. Compare with a dumb baseline

1. Sum journal debits and credits in SQL (`SUM(amount_paise)` grouped by entry).
2. Compare finance service output to a hand spreadsheet for one due.
3. Run `go run ./scripts/check-coverage.go -profile=coverage.out -require-all` after `go test -coverprofile=coverage.out ./...`.

**Expected outcome:** Baseline and engine match exactly.

## 5. Fix seeds (determinism)

1. Always use `go test -count=1` when chasing flakes.
2. Run `go clean -testcache` after changing migration or init logic.
3. On fuzz failure, record the seed from test output.

**Expected outcome:** Same command reproduces the same result.

## 6. Change one variable at a time

| Variable | Header / env |
| --- | --- |
| Request trace | `X-Request-ID` (see `middleware_request_id.go`) |
| Property scope | `X-Property-ID`, JWT property claim |
| Idempotency | `Idempotency-Key` |
| DB role | `DATABASE_URL` vs `DATABASE_MAINT_URL` |
| Webhook secret | `CASHFREE_*` env — wrong secret → 401/503 |

**Expected outcome:** You isolate one misconfiguration.

## Operational notes

- Gin redacts `/p/<token>` paths in access logs.
- Use `respondErr` in handlers — never return raw `err.Error()` to clients.
- Firebase auth returns 503 in dev when `./secrets/firebase-sa.json` is missing.
- Set `CORS_ALLOWED_ORIGINS=http://localhost:5173` for pg-react local dev.

## Useful breakpoints

1. `internal/app/bootstrap.go` — `Build` (nil dependency)
2. `internal/api.NewRouter` — route registration
3. `handlers_pay_webhooks.go` — Cashfree PG webhook
4. `handlers_payouts_webhook.go` — payout webhook
5. `internal/finance/ledger_worker.go` — outbox retry

## Dev shortcuts

See [Makefile](../Makefile) or `scripts/dev.ps1`:

| Target | Action |
| --- | --- |
| `make migrate` | Apply migrations |
| `make server` | Run API |
| `make test` | Unit tests |
| `make test-db` | Tests with `REQUIRE_DB=1` |
| `make golden` | E2E money lifecycle test |

## Verification gates (CI)

| Gate | Local command |
| --- | --- |
| Skip audit | `go test ... \| tee test_output.log` then `go run ./scripts/audit_skips.go test_output.log` |
| Coverage | `go run ./scripts/check-coverage.go -profile=coverage.out -require-all` |

See [`.github/workflows/test.yml`](../.github/workflows/test.yml) for full CI env.
