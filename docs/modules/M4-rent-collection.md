# Module: M4 Rent Collection

**Sprint:** 4 · **ADR:** ADR-006, ADR-002

## Gate 1 — Journeys

| ID | Route | Status | Notes |
| --- | --- | --- | --- |
| J1 | `GET /api/tenant/dues` | Verified | Gated by KYC onboarding, tenant dues list with calculation options |
| J2 | `GET /api/tenant/payments/:id/receipt` | Verified | Gated by KYC onboarding, cross-tenant isolation tested in `handlers_tenant_receipt_test.go` |

## Gate 8 — Exit Checklist

- [x] Tenant dues options server-computed (`CalculatePaymentOptions`)
- [x] Per-due QR endpoint (`GET /api/tenant/dues/:id/qr`)
- [x] Standalone unauthenticated payment page (`GET /p/:token`)
- [x] Payment receipt endpoint with cross-tenant isolation (`GET /api/tenant/payments/:id/receipt`)
- [x] Table-driven unit tests passing in `internal/api/handlers_tenant_receipt_test.go`

