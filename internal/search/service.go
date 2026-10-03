package search

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// Service orchestrates lexical search with RBAC types.
// ADR-012: vector/hybrid search is permanently disabled.
type Service struct {
	Repo     Repository
	Embedder Embedder // retained for wiring compat; never called in the search path
}

// Search executes a federated lexical query for the given role scope.
// Enforces an 800 ms overall search budget, returning partial: true if any entity query timed out.
func (s *Service) Search(ctx context.Context, role domain.Role, propertyID uuid.UUID, tenantID *uuid.UUID, q string, limit int, _ Mode, typeFilter []EntityType) (string, Mode, []Result, bool, error) {
	q = NormalizeQuery(q)
	if q == "" {
		return q, ModeLexical, nil, false, ErrQueryRequired
	}
	if !ValidQueryLength(q) {
		if utf8.RuneCountInString(q) > MaxQueryLength {
			return q, ModeLexical, nil, false, ErrQueryTooLong
		}
		return q, ModeLexical, nil, false, ErrQueryTooShort
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}

	types := filterTypes(AllowedTypes(role), typeFilter)
	if len(types) == 0 {
		return q, ModeLexical, nil, false, nil
	}

	// 800 ms overall search budget
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 800*time.Millisecond)
		defer cancel()
	}

	// ADR-012: Cap at 5 per entity type to prevent type starvation
	perType := 5
	if limit < perType {
		perType = limit
	}

	tokens := TokenizeQuery(q)
	p := Params{
		Query:      q,
		Tokens:     tokens,
		Limit:      limit,
		PerType:    perType,
		Mode:       ModeLexical,
		PropertyID: propertyID,
		TenantID:   tenantID,
		Role:       string(role),
		Types:      types,
	}

	lexical, partial, err := s.Repo.SearchLexical(ctx, p, types, perType)
	if err != nil {
		return q, ModeLexical, nil, partial, err
	}

	return q, ModeLexical, trimLimit(lexical, limit), partial, nil
}

func trimLimit(rs []Result, limit int) []Result {
	if len(rs) <= limit {
		return rs
	}
	return rs[:limit]
}
