# Ticket T4d: Auth HTTP Handlers (G3)

- **Track:** T4d
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/api/handlers_auth_http_test.go`

## Acceptance Criteria
- [x] Comprehensive test coverage for auth HTTP endpoints:
  1. `AuthRefresh`:
     - Valid refresh token cookie returns 200 OK and rotates the refresh token cookie.
     - Missing cookie returns 401 Unauthorized (`CodeAuthUnauthorized`).
     - Replayed token returns 401 Unauthorized (`CodeAuthAccessRevoked`) and clears cookie.
     - Revoked or expired session returns 401 Unauthorized and clears cookie.
     - Vacated tenant session returns 403 Forbidden with `CodeAuthAccessRevoked`.
  2. `OTPRequest`:
     - Malformed JSON body returns 400 Bad Request (`CodeRequestInvalidBody`).
     - Known and unknown phone numbers return identical 200 response bodies for anti-enumeration.
     - Rate limited phone returns 429 Too Many Requests (`CodeAuthRateLimited`).
  3. `LookupInvite`:
     - Valid invite code returns 200 OK with property details.
     - Invalid / unknown invite code returns 404 Not Found.
  4. `JoinMe`:
     - Pending unallocated tenant retrieves join status (200 OK).
     - Missing join record returns 404 Not Found.
     - Already activated tenant receives 403 Forbidden.
