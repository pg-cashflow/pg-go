# Module: M1 Identity & Access

**Sprint:** 2 · **ADR:** ADR-002, ADR-011

## Gate 1 — Journeys (key routes)

| ID | Route | Status |
| --- | --- | --- |
| J1 | `POST /api/auth/otp/request` | Exists |
| J2 | `POST /api/auth/refresh` | Exists |
| J3 | `PATCH /api/tenant/me` | Added (name, guardian_phone, locale) |
| J4 | `POST /api/auth/revoke-sessions` | Exists |

## Gate 8 — Exit

- [x] Tenant self-service profile PATCH
- [x] Authz matrix row for PATCH /tenant/me (verified via route_role_matrix_test.go & handlers_tenant_test.go)
