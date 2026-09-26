# Ticket 3: Cross-Cutting IDOR Sweep Across All Remaining API Handlers

- **Type**: `wayfinder:research`
- **Status**: Open (Claimed next)
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Question

Do any remaining handler endpoints in `internal/api/` suffer from IDOR vulnerabilities by omitting property ownership checks against caller claims (e.g. discarding `propertyIDFromClaims(c)`)? Specifically auditing:
- `handlers_tenants.go`
- `handlers_dues.go`
- `handlers_owner.go`
- `handlers_manager.go`
- `handlers_finance.go`
- `handlers_collector.go`
