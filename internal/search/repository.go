package search

import "context"

// Repository runs lexical queries scoped by property/tenant.
// ADR-012: vector/hybrid search is permanently disabled; the interface is lexical-only.
type Repository interface {
	SearchLexical(ctx context.Context, p Params, types []EntityType, perType int) ([]Result, bool, error)
}
