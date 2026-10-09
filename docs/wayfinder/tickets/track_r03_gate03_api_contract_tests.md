# Ticket 19: Track R.3 — Gate 03: API Contract & Router Response Schema Validation

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: [Gate 01](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r01_gate01_skip_audit.md)
- **Tier**: PR Gate (< 10 min) + Nightly

## Objective

Validate all API routes against `CONTRACT.md` Rev 13 and `error-codes.json`.
Ensure JSON response shapes, envelope keys, and machine-readable error codes remain compatible with `pg-react`.
Prevent breaking API changes from reaching production.

## Technical Context

`CONTRACT.md` is the authoritative contract for the frontend.
`internal/api/router.go` registers all public and authenticated HTTP endpoints.
The frontend expects specific response envelopes: `{ "properties": [...] }`, `{ "tenants": [...] }`, `{ "dues": [...] }`.
Errors must contain `{ "error": string, "code": string? }`.

## Karpathy Test Discipline

1. **Look at data first:**
   Inspect `CONTRACT.md` Rev 13 routes and sample response payloads.
   Inspect `internal/api/error_codes.go` and `error-codes.json`.
   Review raw JSON responses from running endpoints.

2. **Check state at initialization:**
   Verify that all routes in `NewRouter` match documented paths in `CONTRACT.md`.
   Assert that every registered error code exists in the error code registry.

3. **Overfit one example:**
   Select endpoint `GET /api/properties`.
   Write a contract test verifying that the response envelope is `{ "properties": [...] }`.
   Verify each property object contains required integer-paise fields and string IDs.

4. **Compare with dumb baseline:**
   Generate or validate against a static OpenAPI 3 schema derived from `CONTRACT.md`.
   Use `kin-openapi` or JSON schema validation to assert complete structural conformity.

5. **Fix seeds:**
   Use fixed database seed data for contract assertions.

6. **Change one thing at a time:**
   Test successful responses for all routes first.
   Then test error responses and status codes for all routes.

## STE-100 Implementation Steps

1. Create a contract test suite in `internal/api/contract_test.go`.
2. Extract all route paths and HTTP methods registered in `NewRouter`.
3. Verify each route returns the expected JSON envelope structure on success.
4. Verify all integer money values serialize as integer paise without decimal points.
5. Verify that error responses conform to `{ "error": string, "code": string }`.
6. Assert that every emitted error code starts with a valid domain prefix (`auth.`, `billing.`, `payment.`, `kyc.`, `finance.`).
7. Run the contract test suite in CI on every pull request.
8. (Nightly) Run Schemathesis against the OpenAPI schema to fuzz request and response types.

## Acceptance Criteria

- Every endpoint in `internal/api/router.go` has an automated contract test.
- All list responses wrap arrays in the exact envelope documented in `CONTRACT.md`.
- All monetary amounts serialize as integer paise (`int64`).
- All error responses contain a human-readable `error` string and a canonical dot-delimited `code`.
