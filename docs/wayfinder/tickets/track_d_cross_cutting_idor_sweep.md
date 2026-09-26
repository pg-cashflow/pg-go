# Ticket 3: Cross-Cutting IDOR Sweep Across All Remaining API Handlers

- **Type**: `wayfinder:research` / `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Summary of Findings & Audit Results

Every handler in `internal/api/` was methodically audited against property and tenant authorization boundaries:

### 1. Verified Clean Handlers
- **`handlers_owner.go`**: All tenant management (`CreateTenant`, `ListTenants`, `UpdateTenant`, `TenantNotice`, `TenantVacate`, `TenantAttachPhone`, `TenantProrate`, `TenantDepositSettle`, `TenantIDPhoto`), dues (`ListDues`, `WaiveDue`, `ManualMatch`, `MarkCashPaid`, `DueQR`, `DueToken`), statement imports (`ImportStatements`), payments list (`ListPayments`), audit events (`ListEvents`), and reconciliation (`Reconciliation`) strictly enforce `pid, ok := propertyIDFromClaims(c)` and `resource.PropertyID != pid` before performing reads or mutations.
- **`handlers_tenant.go`**: All routes derive identity strictly from `auth.ContextTenantKey` (`tenantFromContext(c)`). Dues queries, payment histories, QR generation, push subscriptions, and Aadhaar consent operations use `tenant.ID` and cannot access foreign tenant records.
- **`handlers_pay.go`**: Tenant endpoints (`TenantDuePay`, `TenantSubmitReport`) enforce `due.TenantID == t.ID`; batch payments (`TenantDuePayBatch`) validate option combinations against open dues of the calling tenant; owner pay and refunds (`OwnerDuePay`, `OwnerRefundPayment`) verify `PropertyID == pid`; payment report confirmations (`ConfirmPaymentReport`, `RejectPaymentReport`) enforce `rep.PropertyID == pid`.
- **`handlers_join.go`**: `ActivateJoin` and `RejectJoin` pass caller `pid` to `join.Service`, which enforces `j.PropertyID != propertyID` with `ErrNotFound` / `ErrNotPendingOwner`.
- **`handlers_notifications.go`**: Notification reads and acknowledgment updates are scoped to `claims.UserID` at the repository query level.
- **`handlers_preferences.go`**: Preference retrieval and mutations are scoped to `claims.UserID`.
- **`handlers_search.go`**: Scoped strictly by `*claims.PropertyID` and caller's `claims.TenantID`.

### 2. Remediated IDOR Vulnerabilities

#### A. Financial Subsystem (`internal/api/handlers_finance.go`)
- **`GetLeakage` (GET `/api/owner/leakage/:id`)**: Previously fetched leakage events without verifying property ownership. Remediated by requiring `propertyIDFromClaims(c)` and asserting `e.PropertyID == pid` (404 on mismatch).
- **`RecommendationAction` (POST `/api/owner/recommendations/:id/:action`)**: Previously updated recommendations without property scoping. Remediated by requiring `propertyIDFromClaims(c)` and asserting `rec.PropertyID == pid` (404 on mismatch).

#### B. Gamification & Operations Subsystem (`internal/api/handlers_gamification.go`)
- **`TenantRedeem` (POST `/api/tenant/rewards/:id/redeem`)**: Added validation that the catalog item's `PropertyID` matches `tenant.PropertyID` (404 on foreign reward).
- **`TenantVoteMenuPoll` (POST `/api/tenant/menu-poll/vote`)**: Validates that the submitted `poll_id` matches the calling tenant's active property poll (404 on foreign poll).
- **`ManagerSubmitInspection` (POST `/api/manager/inspections`)**: Enforces `*claims.PropertyID == propID` across both manager and owner callers (403 on mismatch).
- **`ManagerListInspections` (GET `/api/manager/inspections`)**: Enforces that query parameter `property_id` cannot query outside `claims.PropertyID` (403 on mismatch).
- **`ManagerResolveInspectionItem` (POST `/api/manager/inspections/items/:id/resolve`)**: Updated repository, service, and handler layers so `ResolveInspectionItem` takes `propertyID`, constraining the database update to `WHERE id = $1 AND inspection_id IN (SELECT id FROM inspections WHERE property_id = $5)`.
- **`ManagerLogViolation` (POST `/api/manager/violations`)**: Added verification that the targeted tenant belongs to caller's property (`tenant.PropertyID == *claims.PropertyID`).
- **`ManagerRecordMeterReading` (POST `/api/manager/meter-readings`)**: Enforces `*claims.PropertyID == propID` (403 on foreign property).
- **`ManagerKitchenHeadcount` (GET `/api/manager/kitchen/headcount`)**: Enforces `*claims.PropertyID == propID` (403 on foreign property).
- **`ManagerListHazards` (GET `/api/manager/hazards`)**: Enforces `*claims.PropertyID == propID` (403 on foreign property).
- **`ManagerResolveHazard` (POST `/api/manager/hazards/:id/resolve`)**: Verifies `hz.PropertyID == *claims.PropertyID` before resolving (404 on foreign hazard).
- **`ManagerSubmitVendorInspection` (POST `/api/manager/vendor-inspections`)**: Enforces `*claims.PropertyID == propID` (403 on foreign property).
- **`OwnerGetGamificationSettings` (GET `/api/owner/gamification/settings`)**: Disallows foreign `?property_id=` querying (403 on mismatch).
- **`OwnerUpdateGamificationSettings` (PATCH `/api/owner/gamification/settings`)**: Overwrites `body.PropertyID = pid` using caller claims.
- **`OwnerListFloors` (GET `/api/owner/floors`)**: Disallows foreign `?property_id=` querying (403 on mismatch).
- **`OwnerCreateFloor` (POST `/api/owner/floors`)**: Overwrites `PropertyID: pid` using caller claims.
- **`OwnerListRooms` (GET `/api/owner/rooms`)**: Disallows foreign `?property_id=` querying (403 on mismatch).
- **`OwnerCreateRoom` (POST `/api/owner/rooms`)**: Overwrites `PropertyID: pid` and verifies referenced `floor_id` belongs to caller's property.
- **`OwnerCreateManager` (POST `/api/owner/managers`)**: Forbids passing foreign `property_id` overrides (403 on mismatch) and strictly binds manager to `&pid`.

#### C. Identity Subsystem (`internal/api/handlers_kyc.go`)
- **`TenantKYCReturn` (GET `/api/tenant/kyc/return`)**: Remediated IDOR where an arbitrary query parameter `vendor_ref_id` could fetch or complete another tenant's verification session. Added unconditional affirmative ownership verification (`v.TenantID != t.ID`) returning `404 Not Found` on any mismatch, reinforced by schema `tenant_id UUID NOT NULL REFERENCES tenants(id)`.

## Automated Tests Added

1. **`internal/api/handlers_finance_test.go`**:
   - `TestFinanceCrossPropertyIDORGuards`: Asserts Owner A cannot read Property B leakage events (404) or mutate Property B recommendations (404), while Owner B succeeds (200).
2. **`internal/api/handlers_gamification_test.go`**:
   - `TestGamificationCrossPropertyIDORGuards`: Table and unit scenarios covering all 14 cross-property isolation boundaries across tenants, managers, and owners.
3. **`internal/api/handlers_kyc_test.go`**:
   - `TestTenantKYCReturn_ForeignTenant_Returns404`: Asserts Tenant A cannot inspect or poll foreign Tenant B's DigiLocker return session (`404 Not Found`).

## Uncached Test Verification

Executed `go test -count=1 ./...` with zero failures across all packages.
