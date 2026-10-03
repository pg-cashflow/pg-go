package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// Service orchestrates lexical search with RBAC types.
// ADR-012: vector/hybrid search is permanently disabled.
type Service struct {
	Repo  Repository
	Cache *Cache // optional; nil disables caching
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

	run := func(ctx context.Context) ([]Result, bool, error) {
		lexical, partial, err := s.Repo.SearchLexical(ctx, p, types, perType)
		if err != nil {
			return nil, partial, err
		}
		return MergeResults(lexical, tokens, limit), partial, nil
	}

	if s.Cache != nil {
		key := cacheKey(role, propertyID, tenantID, limit, types, q)
		// Detach from the first caller's cancellation: coalesced callers share this run.
		base := context.WithoutCancel(ctx)
		res, partial, err := s.Cache.Do(key, func() ([]Result, bool, error) {
			cctx, cancel := context.WithTimeout(base, 800*time.Millisecond)
			defer cancel()
			return run(cctx)
		})
		return q, ModeLexical, res, partial, err
	}

	res, partial, err := run(ctx)
	return q, ModeLexical, res, partial, err
}

// cacheKey includes every input that affects visibility or ranking.
func cacheKey(role domain.Role, property uuid.UUID, tenant *uuid.UUID, limit int, types []EntityType, q string) string {
	ts := make([]string, len(types))
	for i, t := range types {
		ts[i] = string(t)
	}
	sort.Strings(ts)
	tid := ""
	if tenant != nil {
		tid = tenant.String()
	}
	return fmt.Sprintf("%s|%s|%s|%d|%s|%s", role, property, tid, limit, strings.Join(ts, ","), strings.ToLower(q))
}
