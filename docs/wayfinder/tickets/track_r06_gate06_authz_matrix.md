# Ticket 22: Track R.6 — Gate 06: Multi-Role Authorization & Cross-Property IDOR Matrix

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: [Gate 03](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r03_gate03_api_contract_tests.md)
- **Tier**: PR Gate (< 10 min)

## Objective

Generate a comprehensive authorization matrix covering every route registered in `NewRouter`.
Verify role-based access control and cross-property isolation across all roles:
$$\text{Routes} \times \{\text{Anonymous}, \text{Tenant}, \text{Manager}, \text{Owner}\} \times \{\text{Own Property}, \text{Other Property}\}$$
Assert that no endpoint leaks unauthorized data or allows horizontal privilege escalation (IDOR).

## Expected Status Codes

- Anonymous request to protected route: HTTP 401 Unauthorized.
- Unauthorized role access (e.g., Tenant calling Owner endpoint): HTTP 403 Forbidden.
- Cross-property tenant/manager access: HTTP 404 Not Found (or 403 Forbidden).
- Authorized role on owned property: HTTP 200/201/204.

## Karpathy Test Discipline

1. **Look at data first:**
   Extract every route definition from `internal/api/router.go`.
   Group routes by middleware prefix: `/api/auth/*`, `/api/tenant/*`, `/api/manager/*`, `/api/owner/*`.

2. **Check state at initialization:**
   Create seed data in PostgreSQL:
   - Property A with Owner A, Manager A, Tenant A.
   - Property B with Owner B, Manager B, Tenant B.

3. **Overfit one example:**
   Select `GET /api/owner/payouts/batches`.
   Call as Anonymous -> assert 401.
   Call as Tenant A -> assert 403.
   Call as Manager A -> assert 403.
   Call as Owner B for Property A -> assert 403 or 404.
   Call as Owner A -> assert 200.

4. **Compare with dumb baseline:**
   Build a static matrix table in code.
   Compare every test result against the expected cell in the table.

5. **Fix seeds:**
   Use fixed JWT signing keys and deterministic user claims.

6. **Change one thing at a time:**
   Test anonymous rejection for all routes first.
   Then test role mismatches.
   Finally test cross-property IDOR attempts.

## STE-100 Implementation Steps

1. Create test file `internal/api/authz_matrix_test.go`.
2. Construct a test helper generating valid JWTs for:
   - `TenantToken(propertyA, tenantA)`
   - `ManagerToken(propertyA, managerA)`
   - `OwnerToken(propertyA, ownerA)`
   - `OwnerToken(propertyB, ownerB)`
3. Iterate over every registered route in the engine.
4. For each route, send requests with:
   - No token (Anonymous)
   - Tenant A token
   - Manager A token
   - Owner B token (Cross-property attacker)
5. Assert that no authenticated route permits anonymous access.
6. Assert that manager and owner routes reject tenant tokens with 403.
7. Assert that property-scoped resource lookups reject cross-property owners with 403 or 404.

## Acceptance Criteria

- 100% of registered routes are tested against all four roles.
- Zero routes return 200 OK for unauthorized callers.
- Cross-property resource queries never leak data belonging to another property.
- Test runs in under 15 seconds.
