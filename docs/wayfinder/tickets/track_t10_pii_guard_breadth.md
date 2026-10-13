# Ticket T10: PII Guard Breadth (G9)

- **Track:** T10
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/api/privacy_pii_test.go`

## Acceptance Criteria
- [x] Extend the Gate 14 log-capture test to API handlers touching phone and Aadhaar data:
  - `TenantAadhaar` (`POST /tenant/aadhaar`)
  - `OTPRequest` (`POST /auth/otp/request`)
  - `OTPVerify` (`POST /auth/otp/verify`)
  - `TenantAttachPhone` (`POST /owner/tenants/:id/attach-phone`)
  - `CreateTenant` (`POST /owner/tenants`)
  - `OwnerTenantKYCPhoto` (`GET /owner/tenants/:id/kyc-photo`)
  - `TenantIDPhoto` (`GET /owner/tenants/:id/id-photo`)
- [x] Verify zero raw 12-digit Aadhaar numbers in structured application logs.
- [x] Verify zero raw unmasked 10-digit phone numbers in application logs.
