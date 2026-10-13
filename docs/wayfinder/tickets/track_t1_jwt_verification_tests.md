# Ticket T1: Access-Token Verification Tests (G1)

- **Track:** T1
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/auth/jwt_test.go`

## Acceptance Criteria
- [x] Add table-driven tests in `internal/auth/jwt_test.go`.
- [x] Test cases covered:
  - valid token
  - expired token
  - wrong secret
  - tampered payload
  - tampered role claim / signature
  - `alg=none` token
  - `HS384` token
  - `HS512` token
  - empty string token
  - garbage string token
  - missing `exp` claim
- [x] Assert `VerifyToken` returns an error for every bad case and correct claims for valid case.
- [x] Mutating the HS256 check in `jwt.go` fails algorithm downgrade test cases.
