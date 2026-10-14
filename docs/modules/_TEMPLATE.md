# Module: `<Mn-name>`

**Owner:** Divakar · **Sprint:** `<n>` · **ADR:** `docs/adr/0xx-*.md`

## Gate 1 — Journeys (ASD-STE100)

| ID | Actor | Precondition | Action (route) | Expected HTTP | Ledger / side effect | Gap ticket |
| --- | --- | --- | --- | --- | --- | --- |
| J1 | | | | | | |

Rules: use short sentences; one verb per step; state amounts in integer paise; link CONTRACT routes.

## Gate 2 — Design

- Link ADR-001–035 where relevant.
- Add one new ADR only when a decision changes.

## Gate 3 — Data

- Migration: `___`
- RLS on `property_id`: yes/no — policy name
- Immutability: list append-only tables

## Gate 4 — API / authz

- Rows in `internal/api/route_role_matrix_test.go`
- Manager routes: `internal/api/capability_matrix_test.go`

## Gate 5 — Tests (Karpathy order)

1. Inspect DB rows or JSON once by hand.
2. One overfit journey test.
3. Property or fuzz tests for money paths (M4–M7).

## Gate 6 — Docs

- `CONTRACT.md` and Postman
- Runbook errors in STE100

## Gate 7 — Ops

- systemd unit/timer row in README (deploy-neutral until VPS is live)

## Gate 8 — Exit

```text
go clean -testcache && go test -count=1 ./...
REQUIRE_DB=1 DATABASE_URL='...' go test -count=1 ./...
```

- [ ] Checklist signed
