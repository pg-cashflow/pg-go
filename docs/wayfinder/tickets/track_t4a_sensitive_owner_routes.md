# Ticket T4a: Sensitive and Money-Moving Owner Routes (G3)

- **Track:** T4a
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/api/handlers_owner_sensitive_test.go`

## Acceptance Criteria
- [x] Comprehensive test coverage for all 11 sensitive and money-moving owner routes:
  1. `OwnerTenantKYCPhoto`: happy path (200), foreign-property (404), non-owner role (403).
  2. `TenantIDPhoto`: happy path (200), foreign-property (404), non-owner role (403).
  3. `WaiveDue`: happy path (200 with step-up OTP), foreign-property (404), non-owner role (403), missing step-up (401), non-waivable status (400).
  4. `PostReimbursement`: happy path (201), missing idempotency key (400).
  5. `DueToken`: happy path (200), foreign-property (404).
  6. `OwnerDuePay`: happy path (200), foreign-property (404), non-owner role (403).
  7. `TenantProrate`: happy path (200), foreign-property (404), non-owner role (403).
  8. `TenantAttachPhone`: happy path (200), foreign-property (404), non-owner role (403).
  9. `RejectJoin`: happy path (200), foreign join request (404), non-owner role (403).
  10. `OwnerRotateInvite`: happy path (200), foreign-property (403), non-owner role (403).
  11. `OwnerInvite`: happy path (200), foreign-property (403), non-owner role (403).
