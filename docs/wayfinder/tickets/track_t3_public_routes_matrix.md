# Ticket T3: Matrix: Public Routes (G5)

- **Track:** T3
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/api/route_role_matrix_test.go`

## Acceptance Criteria
- [x] In `TestRouteRoleAuthorizationMatrix`, assert every public route does not return an auth middleware refusal code (`apierr.CodeAuth*`).
- [x] Uses the 6 codes matching `TestMiddlewareGuards_AllSixCodes`:
  - `auth.missing_token`
  - `auth.invalid_token`
  - `auth.access_revoked`
  - `auth.forbidden`
  - `auth.profile_incomplete`
  - `auth.already_activated`
- [x] Includes allow-list for non-middleware handler-level refusals.
- [x] Verified that moving a public route (`/webhooks/cashfree`) behind auth causes the test to fail via `TestRouteRoleAuthorizationMatrix_MovingPublicRouteBehindAuthFails`.
