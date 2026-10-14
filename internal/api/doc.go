// Package api is the HTTP adapter layer for pg-go (Gin).
//
// System map: docs/ARCHITECTURE.md. Package layout: docs/CODEBASE.md.
//
// Route registration lives in router.go and router_*.go files.
// Business logic lives in internal/{finance,tenant,payment,...}; keep handlers thin.
//
// Error handling:
//   - Use respondErr for all handler errors (internal/api/apierr.go).
//   - Use apierr.Code for stable machine-readable codes (internal/apierr/codes.go).
//   - Never put raw err.Error() in JSON responses.
//
// Handler files by bounded context:
//   - Auth / join: handlers_public.go, handlers_join.go, handlers_step_up.go
//   - Owner: handlers_owner.go, handlers_owner_*.go, handlers_owner_dashboard.go
//   - Finance / ledger: handlers_finance.go, handlers_settlement.go
//   - Payments / gateway: handlers_pay_*.go
//   - Payouts: handlers_payouts.go, handlers_payouts_webhook.go
//   - Tenant: handlers_tenant.go
//   - Manager capabilities: handlers_manager_capabilities.go
//   - KYC: handlers_kyc.go
//   - Gamification / property ops: handlers_gamification.go, handlers_attendance.go
//   - Search / notifications / preferences: handlers_search.go, handlers_notifications.go, handlers_preferences.go
//   - Metrics: handlers_metrics.go
//
// Interfaces consumed by handlers are defined in deps.go (dependency inversion).
package api
