# Ticket 18: Track R.2 — Gate 02: Package Coverage Floors & Coverprofile Gating

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: [Gate 01](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r01_gate01_skip_audit.md)
- **Tier**: PR Gate (< 10 min)

## Objective

Enforce minimum test coverage floors for critical packages.
Prevent code merges that decrease test coverage on money-handling packages.
Fail the CI build if statement coverage drops below the required threshold.

## Package Thresholds

- `internal/finance`: Minimum 85% statement coverage.
- `internal/payment`: Minimum 85% statement coverage.
- `internal/auth`: Minimum 80% statement coverage.
- `internal/api`: Minimum 75% statement coverage.

## Karpathy Test Discipline

1. **Look at data first:**
   Run `go test -coverprofile=coverage.out ./...`.
   Inspect statement coverage for each package with `go tool cover -func=coverage.out`.
   Identify packages with coverage below the target thresholds.

2. **Check state at initialization:**
   Generate the baseline coverage profile on the base branch.
   Record the exact coverage percentage for each target package.

3. **Overfit one example:**
   Select package `internal/finance`.
   Write unit tests for untested edge cases in `mirror.go` and `reports.go`.
   Confirm coverage for `internal/finance` reaches 85% or higher.

4. **Compare with dumb baseline:**
   Use a simple awk or Go script to parse `go tool cover -func` output.
   Compare each package percentage against its fixed threshold.

5. **Fix seeds:**
   Ensure coverage numbers are deterministic across multiple runs.

6. **Change one thing at a time:**
   Add tests for one package at a time.
   Verify the coverage increase before moving to the next package.

## STE-100 Implementation Steps

1. Create a coverage check script at `scripts/check-coverage.go` or `scripts/check-coverage.sh`.
2. Configure the script to read `coverage.out`.
3. Configure the script with the threshold table:
   - `internal/finance`: 85
   - `internal/payment`: 85
   - `internal/auth`: 80
   - `internal/api`: 75
4. Parse the coverage output by package name.
5. Compare each package coverage value with its threshold.
6. If any package coverage is below its threshold, print an error and exit with code 1.
7. Update `.github/workflows/ci.yml` to generate `coverage.out` and run the coverage check script.

## Acceptance Criteria

- The CI workflow generates `coverage.out` on every pull request.
- The coverage check script validates all four target packages.
- A pull request with coverage below the threshold fails the CI build.
- `internal/finance` statement coverage is at or above 85%.
- `internal/payment` statement coverage is at or above 85%.
