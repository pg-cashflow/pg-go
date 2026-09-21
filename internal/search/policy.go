package search

import "github.com/pg-cashflow/pg-go/internal/domain"

// AllowedTypes returns entity types visible for a role.
func AllowedTypes(role domain.Role) []EntityType {
	switch role {
	case domain.RoleOwner:
		return []EntityType{
			TypeTenant, TypeDue, TypePayment, TypePaymentReport, TypeJoinRequest, TypeEvent, TypeDocument,
		}
	case domain.RoleManager:
		return []EntityType{
			TypeTenant, TypeInspection, TypeHazard, TypeViolation, TypeDocument,
		}
	case domain.RoleTenant:
		return []EntityType{TypeDue, TypePayment, TypeDocument}
	default:
		return nil
}
}

func filterTypes(allowed []EntityType, requested []EntityType) []EntityType {
	if len(requested) == 0 {
		return allowed
	}
	set := make(map[EntityType]bool, len(allowed))
	for _, t := range allowed {
		set[t] = true
	}
	out := make([]EntityType, 0, len(requested))
	for _, t := range requested {
		if set[t] {
			out = append(out, t)
		}
	}
	return out
}
