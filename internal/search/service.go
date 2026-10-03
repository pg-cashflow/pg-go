package search

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// Service orchestrates lexical and hybrid search with RBAC types.
type Service struct {
	Repo     Repository
	Embedder Embedder
}

// Search executes a federated query for the given role scope.
func (s *Service) Search(ctx context.Context, role domain.Role, propertyID uuid.UUID, tenantID *uuid.UUID, q string, limit int, mode Mode, typeFilter []EntityType) (string, Mode, []Result, error) {
	q = NormalizeQuery(q)
	if q == "" {
		return q, ModeLexical, nil, fmt.Errorf("query required")
	}
	if !ValidQueryLength(q) {
		if utf8.RuneCountInString(q) > MaxQueryLength {
			return q, ModeLexical, nil, fmt.Errorf("query too long")
		}
		return q, ModeLexical, nil, fmt.Errorf("query too short")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	if mode != ModeHybrid {
		mode = ModeLexical
	}

	types := filterTypes(AllowedTypes(role), typeFilter)
	if len(types) == 0 {
		return q, ModeLexical, nil, nil
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

	lexical, err := s.Repo.SearchLexical(ctx, p, types, perType)
	if err != nil {
		return q, ModeLexical, nil, err
	}

	return q, ModeLexical, trimLimit(lexical, limit), nil
}

func boostExactToken(q string, rs []Result) []Result {
	if !IsExactTokenQuery(q) {
		return rs
	}
	upper := strings.ToUpper(q)
	out := make([]Result, 0, len(rs))
	var boosted []Result
	var rest []Result
	for _, r := range rs {
		if strings.Contains(strings.ToUpper(r.Title), upper) || strings.Contains(strings.ToUpper(r.Subtitle), upper) {
			r.Score += 1000
			boosted = append(boosted, r)
		} else {
			rest = append(rest, r)
		}
	}
	out = append(out, boosted...)
	out = append(out, rest...)
	return out
}

func trimLimit(rs []Result, limit int) []Result {
	if len(rs) <= limit {
		return rs
	}
	return rs[:limit]
}
