# Ticket T4c: Tenant and Manager Self-Service Routes (G3)

- **Track:** T4c
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/api/handlers_tenant_manager_test.go`, `internal/api/handlers_notification_isolation_test.go`

## Acceptance Criteria
- [x] Comprehensive test coverage for all tenant and manager self-service routes:
  1. `TenantInspections`: verified list returns room/property inspections, role boundaries (tenant-only, 403 on manager/unauthorized).
  2. `TenantDisputeInspectionItem`: verified dispute note validation (400 on empty note), successful dispute submission (200 OK), and outbox event `inspection_disputed` emission.
  3. `TenantGetMealRSVP`: verified retrieval of tomorrow's breakfast, lunch, and dinner RSVP records.
  4. `TenantSubmitMealRSVP`: verified valid meal RSVP submission (200 OK) and rejection of malformed dates (400 Bad Request).
  5. `TenantReportHazard`: verified JSON hazard submission (200 OK), outbox event `hazard_reported` emission, and rejection of empty reports (400 Bad Request).
  6. `TenantViolations`: verified tenant-scoped violation notices retrieval.
  7. `TenantReferrals`: verified referral creation with phone validation (400 on missing phone) and referral listing.
  8. `ManagerResolveInspectionItem`: verified resolution of inspection dispute ("upheld", "overturned") (200 OK), rejection of invalid resolution status (400 Bad Request), and tenant role rejection (403 Forbidden).
  9. `ManagerFinanceToday`: verified manager operations digest, capital hiding when `ManagerCanViewCapital=false`, and tenant role rejection (403 Forbidden).
  10. `PostMealPrep`: verified manager meal prep actuals recording (200 OK), malformed JSON rejection (400 Bad Request), and tenant role rejection (403 Forbidden).
  11. `MarkNotificationRead` & `MarkAllNotificationsRead`: verified cross-user tampering protection (tenant cannot mark another tenant's notifications as read).
