# Ticket 17: Track R.1 — Gate 01: Test Skip Audit & Zero Silent Skip Enforcement

- **Type**: `wayfinder:task`
- **Status**: Closed
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: None
- **Tier**: PR Gate (< 10 min)

## Objective

Eliminate silent test skips across the entire test suite.
A skipped test is not a passing test.
When `REQUIRE_DB=1` is set in CI, any skipped database test must cause an immediate build failure.

## Technical Context

There are 127 `t.Skip` invocations across 31 packages.
Tests skip when `DATABASE_URL` is empty or when `-short` flag is set.
If a configuration error occurs in CI, tests skip silently and report green status.
This behavior creates a false sense of security.

## Karpathy Test Discipline

1. **Look at data first:**
   Run `go test -v ./...` in a local shell without `DATABASE_URL`.
   Count every line containing `--- SKIP:`.
   Identify all packages that skip database tests.

2. **Check state at initialization:**
   Verify `REQUIRE_DB=1` is exported in the environment.
   Assert the test harness reads this variable at package startup.

3. **Overfit one example:**
   Select `internal/postgres/payout_repo_test.go`.
   Clear `DATABASE_URL`.
   Run the test with `REQUIRE_DB=1`.
   Assert the test fails with `t.Fatalf` instead of `t.Skipf`.

4. **Compare with dumb baseline:**
   Use `grep -c "--- SKIP:"` on test output.
   If the count is greater than zero when `REQUIRE_DB=1`, fail the CI step.

5. **Fix seeds:**
   Deterministic test execution. No random flags.

6. **Change one thing at a time:**
   Update the database helper function first.
   Then verify all repository tests honor the rule.

## STE-100 Implementation Steps

1. Open `internal/testutil/db.go` or equivalent test database helper.
2. Inspect the database connection setup function.
3. Check if the environment variable `REQUIRE_DB` equals `"1"`.
4. If `REQUIRE_DB` equals `"1"` and `DATABASE_URL` is empty, call `t.Fatalf("DATABASE_URL required but missing")`.
5. Do not call `t.Skip` when `REQUIRE_DB` equals `"1"`.
6. Add a CI step in `.github/workflows/ci.yml` to parse test output for unexpected skip messages.
7. Run the full test suite in CI.
8. Verify zero tests are skipped.

## Acceptance Criteria
 
- When `REQUIRE_DB=1` and `DATABASE_URL` is unset, `go test` fails with exit code 1.
- In CI, `go test -race -v ./...` completes with zero skipped tests.
- CI pipeline logs report `0 tests skipped`.

## Resolution & Verification Evidence

1. **Central Test Helper (`internal/testutil`)**:
   - `testutil.RequireDB(t)` and `testutil.FailOnSkipIfDBRequired(t, reason)` enforce immediate `t.Fatalf` exit whenever `REQUIRE_DB=1`.
   - Verified negative acceptance criterion: setting `REQUIRE_DB=1` with unset `DATABASE_URL` halts execution immediately with `FATAL: REQUIRE_DB=1 but DATABASE_URL is missing or empty` and exit code 1.

2. **Complete Codebase Audit**:
   - Audited and refactored all raw `t.Skip` invocations across `internal/api`, `internal/finance`, and `internal/postgres`.
   - 0 raw `t.Skip` invocations remain across the entire project outside of `internal/testutil/db.go`.

3. **Audit Script & CI Integration**:
   - Implemented `scripts/audit_skips.go` with zero-tolerance scanner for `--- SKIP:`.
   - Integrated into `.github/workflows/ci.yml` and `.github/workflows/test.yml`.
   - Full suite execution (`powershell -Command "$env:REQUIRE_DB='1'; go test -v ./... | Tee-Object -FilePath test_output.log; go run ./scripts/audit_skips.go test_output.log"`) completed with exit code 0:
     `Gate 01 Success: 0 tests skipped across entire test suite.`
