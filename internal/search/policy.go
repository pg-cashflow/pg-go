package search

import "github.com/pg-cashflow/pg-go/internal/domain"

// AllowedTypes returns entity types visible for a role.
func AllowedTypes(role domain.Role) []EntityType {
	switch role {
	case domain.RoleOwner:
		return []EntityType{
			TypeTenant, TypeDue, TypePayment, TypePaymentReport, TypeJoinRequest, TypeEvent, TypeInspection, TypeHazard, TypeViolation,
		}
	case domain.RoleManager:
		return []EntityType{
			TypeTenant, TypeInspection, TypeHazard, TypeViolation,
		}
	case domain.RoleTenant:
		return []EntityType{TypeDue, TypePayment}
	default:
		return nil
	}
}

// AllowedDocumentEntityTypes returns document entity types visible for a role.
// Strictly scopes search_documents queries to prevent privilege escalation.
func AllowedDocumentEntityTypes(role domain.Role) []string {
	switch role {
	case domain.RoleOwner:
		return []string{"inspection", "hazard", "violation", "payment_note"}
	case domain.RoleManager:
		return []string{"inspection", "hazard", "violation"}
	case domain.RoleTenant:
		return []string{"hazard", "violation"}
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
