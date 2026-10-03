package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

type SearchRepo struct {
	db         DBTX
	concurrent bool // true only for *pgxpool.Pool (safe for concurrent queries)
}

func NewSearchRepo(db DBTX) *SearchRepo {
	_, isPool := db.(*pgxpool.Pool)
	return &SearchRepo{db: db, concurrent: isPool}
}

const (
	searchQueryTimeout = 300 * time.Millisecond // per entity query
	searchFanout       = 4                      // max concurrent queries per request
)

type queryOutcome struct {
	res []search.Result
	err error
	dur time.Duration
}

// runConcurrent executes every entity query with bounded parallelism and a
// per-query timeout. Results keep the original query order so the service's
// merge step is deterministic. It returns an error only when every query failed.
func (r *SearchRepo) runConcurrent(ctx context.Context, queries []entityQuery) ([]search.Result, bool, error) {
	outs := make([]queryOutcome, len(queries))
	sem := make(chan struct{}, searchFanout)
	var wg sync.WaitGroup
	for i, eq := range queries {
		wg.Add(1)
		go func(i int, eq entityQuery) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				outs[i].err = ctx.Err()
				return
			}
			qctx, cancel := context.WithTimeout(ctx, searchQueryTimeout)
			defer cancel()
			start := time.Now()
			rows, err := r.db.Query(qctx, eq.sql, eq.args...)
			if err != nil {
				outs[i] = queryOutcome{err: err, dur: time.Since(start)}
				return
			}
			res, scanErr := eq.scan(rows)
			rows.Close()
			if scanErr == nil {
				scanErr = rows.Err()
			}
			outs[i] = queryOutcome{res: res, err: scanErr, dur: time.Since(start)}
		}(i, eq)
	}
	wg.Wait()

	var merged []search.Result
	failed := 0
	for i, o := range outs {
		if o.err != nil {
			failed++
			slog.Warn("search query failed", "type", queries[i].entityType, "ms", o.dur.Milliseconds(), "err", o.err)
			continue
		}
		if o.dur > 100*time.Millisecond {
			slog.Warn("search query slow", "type", queries[i].entityType, "ms", o.dur.Milliseconds())
		}
		merged = append(merged, o.res...)
	}
	if failed == len(outs) {
		return nil, true, fmt.Errorf("search: all %d entity queries failed", failed)
	}
	return merged, failed > 0, nil
}

type entityQuery struct {
	entityType search.EntityType
	sql        string
	args       []any
	scan       func(pgx.Rows) ([]search.Result, error)
}

func (r *SearchRepo) SearchLexical(ctx context.Context, p search.Params, types []search.EntityType, perType int) ([]search.Result, bool, error) {
	if perType <= 0 {
		perType = 5
	}
	tokens := p.Tokens
	if len(tokens) == 0 && p.Query != "" {
		tokens = search.TokenizeQuery(p.Query)
	}
	if len(tokens) == 0 {
		return nil, false, nil
	}

	// Defect 1: 800 ms overall search budget
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 800*time.Millisecond)
		defer cancel()
	}

	var queries []entityQuery

	for _, t := range types {
		switch t {
		case search.TypeTenant:
			if p.Role == string(domain.RoleTenant) || p.TenantID != nil {
				continue
			}
			queries = append(queries, r.buildTenantsQuery(p, tokens, perType))

		case search.TypeDue:
			if p.Role == string(domain.RoleManager) {
				continue
			}
			if p.Role == string(domain.RoleTenant) && p.TenantID == nil {
				continue
			}
			queries = append(queries, r.buildDuesQuery(p, tokens, perType))

		case search.TypePayment:
			if p.Role == string(domain.RoleManager) {
				continue
			}
			if p.Role == string(domain.RoleTenant) && p.TenantID == nil {
				continue
			}
			queries = append(queries, r.buildPaymentsQuery(p, tokens, perType))

		case search.TypePaymentReport:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			queries = append(queries, r.buildPaymentReportsQuery(p, tokens, perType))

		case search.TypeJoinRequest:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			queries = append(queries, r.buildJoinRequestsQuery(p, tokens, perType))

		case search.TypeEvent:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			queries = append(queries, r.buildEventsQuery(p, tokens, perType))

		case search.TypeInspection:
			if p.Role == string(domain.RoleTenant) {
				continue
			}
			queries = append(queries, r.buildInspectionsQuery(p, tokens, perType))

		case search.TypeHazard:
			if p.Role == string(domain.RoleTenant) {
				continue
			}
			queries = append(queries, r.buildHazardsQuery(p, tokens, perType))

		case search.TypeViolation:
			if p.Role == string(domain.RoleTenant) {
				continue
			}
			queries = append(queries, r.buildViolationsQuery(p, tokens, perType))

		case search.TypeBankTransaction:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			queries = append(queries, r.buildBankTransactionsQuery(p, tokens, perType))

		case search.TypeSettlement:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			queries = append(queries, r.buildSettlementsQuery(p, tokens, perType))

		case search.TypeRefund:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			queries = append(queries, r.buildRefundsQuery(p, tokens, perType))

		case search.TypePayout:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			queries = append(queries, r.buildPayoutsQuery(p, tokens, perType))

		case search.TypeDocument:
			queries = append(queries, r.buildDocumentsLexicalQuery(p, search.LikePattern(p.Query), perType))
		}
	}

	if len(queries) == 0 {
		return nil, false, nil
	}

	var out []search.Result
	var partial bool

	// Pool-backed repo: fan out per-type queries concurrently. Each query has its
	// own timeout and fails alone, so one slow type never discards the others.
	if r.concurrent {
		return r.runConcurrent(ctx, queries)
	}

	// Single-connection runner (tx / test double): pgx.Batch in one round-trip.
	// NOTE: statements still run serially and an error aborts the rest of the batch.
	if batchDB, ok := r.db.(interface {
		SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	}); ok {
		batch := &pgx.Batch{}
		batch.Queue("SET LOCAL statement_timeout = 300")
		for _, q := range queries {
			batch.Queue(q.sql, q.args...)
		}

		br := batchDB.SendBatch(ctx, batch)
		defer br.Close()

		if _, err := br.Exec(); err != nil {
			partial = true
		}
		for _, eq := range queries {
			rows, err := br.Query()
			if err != nil {
				partial = true
				slog.Warn("search query failed", "type", eq.entityType, "err", err)
				continue
			}
			res, scanErr := eq.scan(rows)
			rows.Close()
			if scanErr != nil {
				partial = true
				continue
			}
			out = append(out, res...)
		}
		return out, partial, nil
	}

	// Fallback serial execution if batching is not supported on runner
	for _, eq := range queries {
		rows, err := r.db.Query(ctx, eq.sql, eq.args...)
		if err != nil {
			partial = true
			continue
		}
		res, scanErr := eq.scan(rows)
		rows.Close()
		if scanErr != nil {
			partial = true
			continue
		}
		out = append(out, res...)
	}

	return out, partial, nil
}

func (r *SearchRepo) buildTenantsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	isManager := p.Role == string(domain.RoleManager)
	args := []any{p.PropertyID}
	var tokenClauses []string

	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("name ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(room_number IS NOT NULL AND room_number ILIKE $%d ESCAPE '\\')", len(args)))

		// Defect 11: Unicode-aware rune count for typo tolerance (supports Telugu, Hindi, etc.)
		if utf8.RuneCountInString(t.Value) >= 4 {
			args = append(args, t.Value)
			sub = append(sub, fmt.Sprintf("($%d::text <%% name::text)", len(args)))
		}

		if isManager {
			// Manager: exact 10-digit phone only (scraping defense)
			if t.Kind == search.TokenPhone && len(t.PhoneDigits) == 10 {
				args = append(args, t.PhoneDigits)
				sub = append(sub, fmt.Sprintf("(phone = $%d OR phone = '+91' || $%d)", len(args), len(args)))
			}
		} else {
			// Owner: partial or full phone permitted
			args = append(args, likeVal)
			sub = append(sub, fmt.Sprintf("(phone IS NOT NULL AND phone ILIKE $%d ESCAPE '\\')", len(args)))
		}

		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, p.Query, limit)
	qIdx := len(args) - 1
	limitIdx := len(args)

	// Defect 5: 6-Tier ranking: exact (1000), starts-with (800), word-start (600), contains (400), fuzzy by word_similarity, recency
	sql := fmt.Sprintf(`
		SELECT id::text, name, COALESCE(room_number, ''), status
		FROM tenants
		WHERE property_id = $1
		  AND status != 'archived'
		  AND %s
		ORDER BY
		  CASE
		    WHEN UPPER(name) = UPPER($%d) THEN 1000
		    WHEN room_number = $%d THEN 950
		    WHEN name ILIKE ($%d || '%%') THEN 800
		    WHEN (name ILIKE ($%d || '%%') OR name ILIKE ('%% ' || $%d || '%%')) THEN 600
		    WHEN name ILIKE ('%%' || $%d || '%%') THEN 400
		    ELSE (word_similarity($%d, name) * 200)::int
		  END DESC,
		  created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		qIdx, qIdx, qIdx, qIdx, qIdx, qIdx, qIdx, limitIdx,
	)

	return entityQuery{
		entityType: search.TypeTenant,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, name, room, status string
				if err := rows.Scan(&id, &name, &room, &status); err != nil {
					return nil, err
				}
				sub := status
				if room != "" {
					sub = fmt.Sprintf("Room %s · %s", room, status)
				}
				out = append(out, search.Result{
					Type:     search.TypeTenant,
					ID:       id,
					Title:    name,
					Subtitle: sub,
				})
			}
			return out, nil
		},
	}
}

func (r *SearchRepo) buildDuesQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	tenantFilter := ""
	if p.TenantID != nil {
		args = append(args, *p.TenantID)
		tenantFilter = fmt.Sprintf(" AND d.tenant_id = $%d", len(args))
	}

	var codeClauses []string
	var nameClauses []string
	for _, t := range tokens {
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		likeIdx := len(args)

		codeClauses = append(codeClauses, fmt.Sprintf("d.due_code ILIKE $%d ESCAPE '\\'", likeIdx))
		nameClauses = append(nameClauses, fmt.Sprintf("(t.name IS NOT NULL AND t.name ILIKE $%d ESCAPE '\\')", likeIdx))
	}

	args = append(args, p.Query, limit)
	qIdx := len(args) - 1
	limitIdx := len(args)

	sql := fmt.Sprintf(`
		SELECT id, due_code, status, tenant_name
		FROM (
			SELECT d.id::text, d.due_code, d.status, COALESCE(t.name, '') AS tenant_name, d.created_at
			FROM dues d
			JOIN tenants t ON t.id = d.tenant_id
			WHERE d.property_id = $1 AND t.property_id = $1%s
			  AND %s
			UNION
			SELECT d.id::text, d.due_code, d.status, COALESCE(t.name, '') AS tenant_name, d.created_at
			FROM dues d
			JOIN tenants t ON t.id = d.tenant_id
			WHERE d.property_id = $1 AND t.property_id = $1%s
			  AND %s
		) u
		ORDER BY
		  CASE
		    WHEN UPPER(due_code) = UPPER($%d) THEN 1000
		    WHEN due_code ILIKE ($%d || '%%') THEN 800
		    WHEN UPPER(tenant_name) = UPPER($%d) THEN 700
		    WHEN tenant_name ILIKE ($%d || '%%') THEN 500
		    WHEN (tenant_name ILIKE ($%d || '%%') OR tenant_name ILIKE ('%% ' || $%d || '%%')) THEN 400
		    WHEN tenant_name ILIKE ('%%' || $%d || '%%') THEN 300
		    ELSE 100
		  END DESC,
		  created_at DESC
		LIMIT $%d`,
		tenantFilter,
		strings.Join(codeClauses, " AND "),
		tenantFilter,
		strings.Join(nameClauses, " AND "),
		qIdx, qIdx, qIdx, qIdx, qIdx, qIdx, qIdx, limitIdx,
	)

	return entityQuery{
		entityType: search.TypeDue,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, code, status, tenantName string
				if err := rows.Scan(&id, &code, &status, &tenantName); err != nil {
					return nil, err
				}
				sub := status
				if tenantName != "" {
					sub = tenantName + " · " + status
				}
				out = append(out, search.Result{
					Type:     search.TypeDue,
					ID:       id,
					Title:    "Due " + code,
					Subtitle: sub,
				})
			}
			return out, nil
		},
	}
}

// prefixRange returns [lo, hi) bounds for a prefix match against a
// text_pattern_ops btree on lower(col) (byte-wise / C ordering).
// ok=false for tokens under 3 bytes or containing anything but printable ASCII;
// callers then fall back to the trigram path.
func prefixRange(s string) (lo, hi string, ok bool) {
	s = strings.TrimSpace(s)
	if len(s) < 3 {
		return "", "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return "", "", false
		}
	}
	lo = strings.ToLower(s)
	b := []byte(lo)
	b[len(b)-1]++ // <= 0x7f, still valid single-byte UTF-8
	return lo, string(b), true
}

// identifierLike reports whether a token should be matched against reference
// columns (upi_txn_id / txn_id): code-like ids, or long digit-only references
// such as 12-digit UPI RRNs, which the tokenizer classifies as phone/short-number.
func identifierLike(t search.Token) bool {
	if t.Kind == search.TokenCode {
		return true
	}
	if len(t.Raw) < 6 {
		return false
	}
	for i := 0; i < len(t.Raw); i++ {
		if t.Raw[i] < '0' || t.Raw[i] > '9' {
			return false
		}
	}
	return true
}

func (r *SearchRepo) buildPaymentsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	tenantFilter := ""
	if p.TenantID != nil {
		args = append(args, *p.TenantID)
		tenantFilter = fmt.Sprintf(" AND p.tenant_id = $%d", len(args))
	}

	var isCodeSearch bool
	var fastClauses []string
	var fallbackClauses []string

	for _, t := range tokens {
		pLower, pUpper, ok := prefixRange(t.Raw)
		if ok && identifierLike(t) {
			isCodeSearch = true
			args = append(args, pLower, pUpper)
			fastClauses = append(fastClauses, fmt.Sprintf("(p.upi_txn_id IS NOT NULL AND lower(p.upi_txn_id) ~>=~ $%d AND lower(p.upi_txn_id) ~<~ $%d)", len(args)-1, len(args)))
		} else {
			likeVal := search.LikePattern(t.Raw)
			args = append(args, likeVal)
			fastClauses = append(fastClauses, fmt.Sprintf("(t.name IS NOT NULL AND t.name ILIKE $%d ESCAPE '\\')", len(args)))
		}

		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		fallbackClauses = append(fallbackClauses,
			fmt.Sprintf("((p.raw_note IS NOT NULL AND p.raw_note ILIKE $%d ESCAPE '\\') OR (p.upi_txn_id IS NOT NULL AND p.upi_txn_id ILIKE $%d ESCAPE '\\'))", len(args), len(args)))
	}

	fastOrder := ""
	if isCodeSearch {
		fastOrder = "\n\t\t\tORDER BY lower(p.upi_txn_id) USING ~<~"
	}

	args = append(args, limit)
	limitIdx := len(args)
	args = append(args, p.Query)
	qIdx := len(args)

	var fastScore, fallbackScore string
	if isCodeSearch {
		fastScore = fmt.Sprintf(`
			  CASE
			    WHEN UPPER(p.upi_txn_id) = UPPER($%d) THEN 1000
			    WHEN p.upi_txn_id ILIKE ($%d || '%%%%') THEN 800
			    ELSE 600
			  END`, qIdx, qIdx)
		fallbackScore = "400"
	} else {
		fastScore = fmt.Sprintf(`
			  CASE
			    WHEN UPPER(t.name) = UPPER($%d) THEN 1000
			    WHEN t.name ILIKE ($%d || '%%%%') THEN 800
			    WHEN (t.name ILIKE ($%d || '%%%%') OR t.name ILIKE ('%%%% ' || $%d || '%%%%')) THEN 600
			    ELSE 400
			  END`, qIdx, qIdx, qIdx, qIdx)
		fallbackScore = "200"
	}

	// Two-stage lookup matching ADR-012 latency targets:
	// Stage 1 (fast): Prefix range scan on upi_txn_id (via idx_payments_upi_prefix) or name scan on tenants (via idx_tenants_name_trgm)
	// Stage 2 (fallback): raw_note and upi_txn_id trigram scan, gated by (SELECT count(*) FROM fast) < limit
	sql := fmt.Sprintf(`
		WITH fast AS MATERIALIZED (
			SELECT p.id::text, COALESCE(p.upi_txn_id, 'Payment') AS upi_txn_id, COALESCE(t.name, '') AS tenant_name, p.amount, p.created_at,
			  %s AS rank_score
			FROM payments p
			INNER JOIN tenants t ON t.id = p.tenant_id
			WHERE t.property_id = $1%s
			  AND %s%s
			LIMIT $%d
		),
		fallback AS (
			SELECT p.id::text, COALESCE(p.upi_txn_id, 'Payment') AS upi_txn_id, COALESCE(t.name, '') AS tenant_name, p.amount, p.created_at,
			  %s AS rank_score
			FROM payments p
			INNER JOIN tenants t ON t.id = p.tenant_id
			WHERE t.property_id = $1%s
			  AND (SELECT count(*) FROM fast) < $%d
			  AND %s
			LIMIT $%d
		)
		SELECT id, upi_txn_id, tenant_name, amount
		FROM (
			SELECT * FROM fast
			UNION ALL
			SELECT * FROM fallback WHERE id NOT IN (SELECT id FROM fast)
		) u
		ORDER BY rank_score DESC, created_at DESC
		LIMIT $%d`,
		fastScore,
		tenantFilter,
		strings.Join(fastClauses, " AND "),
		fastOrder,
		limitIdx,
		fallbackScore,
		tenantFilter,
		limitIdx,
		strings.Join(fallbackClauses, " AND "),
		limitIdx,
		limitIdx,
	)

	return entityQuery{
		entityType: search.TypePayment,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, utr, tenantName string
				var amount int
				if err := rows.Scan(&id, &utr, &tenantName, &amount); err != nil {
					return nil, err
				}
				sub := fmt.Sprintf("₹%d", amount/100)
				if tenantName != "" {
					sub = fmt.Sprintf("%s · ₹%d", tenantName, amount/100)
				}
				out = append(out, search.Result{
					Type:     search.TypePayment,
					ID:       id,
					Title:    utr,
					Subtitle: sub,
				})
			}
			return out, nil
		},
	}
}

func (r *SearchRepo) buildPaymentReportsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		tokenClauses = append(tokenClauses, fmt.Sprintf("(upi_txn_id ILIKE $%d ESCAPE '\\' OR (note IS NOT NULL AND note ILIKE $%d ESCAPE '\\'))", len(args), len(args)))
	}

	args = append(args, limit)
	sql := fmt.Sprintf(`
		SELECT id::text, upi_txn_id, status, amount
		FROM payment_reports
		WHERE property_id = $1
		  AND %s
		ORDER BY created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		len(args),
	)

	return entityQuery{
		entityType: search.TypePaymentReport,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, ref, status string
				var amount int
				if err := rows.Scan(&id, &ref, &status, &amount); err != nil {
					return nil, err
				}
				out = append(out, search.Result{
					Type:     search.TypePaymentReport,
					ID:       id,
					Title:    "Report " + ref,
					Subtitle: fmt.Sprintf("%s · ₹%d", status, amount/100),
				})
			}
			return out, nil
		},
	}
}

func (r *SearchRepo) buildJoinRequestsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("name ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("phone ILIKE $%d ESCAPE '\\'", len(args)))
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, limit)
	sql := fmt.Sprintf(`
		SELECT id::text, name, status, phone
		FROM join_requests
		WHERE property_id = $1
		  AND %s
		ORDER BY created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		len(args),
	)

	return entityQuery{
		entityType: search.TypeJoinRequest,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, name, status, phone string
				if err := rows.Scan(&id, &name, &status, &phone); err != nil {
					return nil, err
				}
				out = append(out, search.Result{
					Type:     search.TypeJoinRequest,
					ID:       id,
					Title:    "Join: " + name,
					Subtitle: fmt.Sprintf("%s · %s", status, phone),
				})
			}
			return out, nil
		},
	}
}

func (r *SearchRepo) buildEventsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		tokenClauses = append(tokenClauses, fmt.Sprintf("event_type ILIKE $%d ESCAPE '\\'", len(args)))
	}

	args = append(args, limit)
	sql := fmt.Sprintf(`
		SELECT id::text, event_type
		FROM (
			SELECT id, event_type, occurred_at
			FROM events
			WHERE property_id = $1
			  AND %s
		) e
		ORDER BY occurred_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		len(args),
	)

	return entityQuery{
		entityType: search.TypeEvent,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, eventType string
				if err := rows.Scan(&id, &eventType); err != nil {
					return nil, err
				}
				out = append(out, search.Result{
					Type:     search.TypeEvent,
					ID:       id,
					Title:    eventType,
					Subtitle: "Event",
				})
			}
			return out, nil
		},
	}
}

func (r *SearchRepo) buildInspectionsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("inspection_type ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(notes IS NOT NULL AND notes ILIKE $%d ESCAPE '\\')", len(args)))
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, limit)
	sql := fmt.Sprintf(`
		SELECT id::text, inspection_type, COALESCE(notes, ''), passed
		FROM inspections
		WHERE property_id = $1
		  AND %s
		ORDER BY created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		len(args),
	)

	return entityQuery{
		entityType: search.TypeInspection,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, inspType, notes string
				var passed bool
				if err := rows.Scan(&id, &inspType, &notes, &passed); err != nil {
					return nil, err
				}
				sub := "passed"
				if !passed {
					sub = "failed"
				}
				if notes != "" {
					sub = sub + " · " + truncate(notes, 60)
				}
				out = append(out, search.Result{
					Type:     search.TypeInspection,
					ID:       id,
					Title:    "Inspection " + inspType,
					Subtitle: sub,
				})
			}
			return out, nil
		},
	}
}

func (r *SearchRepo) buildHazardsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("category ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(description IS NOT NULL AND description ILIKE $%d ESCAPE '\\')", len(args)))
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, limit)
	sql := fmt.Sprintf(`
		SELECT id::text, category, COALESCE(description, ''), status
		FROM hazards
		WHERE property_id = $1
		  AND %s
		ORDER BY created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		len(args),
	)

	return entityQuery{
		entityType: search.TypeHazard,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, category, desc, status string
				if err := rows.Scan(&id, &category, &desc, &status); err != nil {
					return nil, err
				}
				sub := status
				if desc != "" {
					sub = truncate(desc, 80)
				}
				out = append(out, search.Result{
					Type:     search.TypeHazard,
					ID:       id,
					Title:    "Hazard " + category,
					Subtitle: sub,
				})
			}
			return out, nil
		},
	}
}

func (r *SearchRepo) buildViolationsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("rule_code ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(description IS NOT NULL AND description ILIKE $%d ESCAPE '\\')", len(args)))
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, limit)
	sql := fmt.Sprintf(`
		SELECT id::text, rule_code, COALESCE(description, ''), severity
		FROM violations
		WHERE property_id = $1
		  AND %s
		ORDER BY created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		len(args),
	)

	return entityQuery{
		entityType: search.TypeViolation,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, code, desc, severity string
				if err := rows.Scan(&id, &code, &desc, &severity); err != nil {
					return nil, err
				}
				sub := severity
				if desc != "" {
					sub = severity + " · " + truncate(desc, 60)
				}
				out = append(out, search.Result{
					Type:     search.TypeViolation,
					ID:       id,
					Title:    "Violation " + code,
					Subtitle: sub,
				})
			}
			return out, nil
		},
	}
}

// buildBankTransactionsQuery covers unmatched bank statement credits and debits (Ticket 14 / Migration 027).
// Uses a two-stage lookup:
// - Stage 1 (fast): Exact/prefix index scan on txn_id via idx_bank_txn_prop_prefix
// - Stage 2 (fallback): Trigram search on narration via idx_bank_transactions_prop_narration_trgm,
//   gated on (SELECT count(*) FROM fast) < limit so Postgres skips narration scan with a one-time filter.
func (r *SearchRepo) buildBankTransactionsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var fastClauses []string
	var fallbackClauses []string

	for _, t := range tokens {
		pLower, pUpper, ok := prefixRange(t.Raw)
		if ok {
			args = append(args, pLower, pUpper)
			fastClauses = append(fastClauses, fmt.Sprintf("(lower(txn_id) ~>=~ $%d AND lower(txn_id) ~<~ $%d)", len(args)-1, len(args)))
		} else {
			likeVal := search.LikePattern(t.Raw)
			args = append(args, likeVal)
			fastClauses = append(fastClauses, fmt.Sprintf("txn_id ILIKE $%d ESCAPE '\\'", len(args)))
		}

		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		fallbackClauses = append(fallbackClauses,
			fmt.Sprintf("(narration ILIKE $%d ESCAPE '\\' OR txn_id ILIKE $%d ESCAPE '\\')", len(args), len(args)))
	}

	args = append(args, limit)
	limitIdx := len(args)
	args = append(args, p.Query)
	qIdx := len(args)

	sql := fmt.Sprintf(`
		WITH fast AS MATERIALIZED (
			SELECT id::text, txn_id, COALESCE(narration, '') AS narration, amount_paise, status, txn_date,
			  CASE
			    WHEN UPPER(txn_id) = UPPER($%d) THEN 1000
			    WHEN txn_id ILIKE ($%d || '%%%%') THEN 800
			    ELSE 600
			  END AS rank_score
			FROM bank_transactions
			WHERE property_id = $1
			  AND %s
			ORDER BY lower(txn_id) USING ~<~
			LIMIT $%d
		),
		fallback AS (
			SELECT id::text, txn_id, COALESCE(narration, '') AS narration, amount_paise, status, txn_date,
			  CASE
			    WHEN narration ILIKE ('%%%%' || $%d || '%%%%') OR txn_id ILIKE ('%%%%' || $%d || '%%%%') THEN 400
			    ELSE 100
			  END AS rank_score
			FROM bank_transactions
			WHERE property_id = $1
			  AND (SELECT count(*) FROM fast) < $%d
			  AND %s
			LIMIT $%d
		)
		SELECT id, txn_id, narration, amount_paise, status
		FROM (
			SELECT * FROM fast
			UNION ALL
			SELECT * FROM fallback WHERE id NOT IN (SELECT id FROM fast)
		) combined
		ORDER BY rank_score DESC, txn_date DESC
		LIMIT $%d`,
		qIdx, qIdx,
		strings.Join(fastClauses, " AND "),
		limitIdx,
		qIdx, qIdx,
		limitIdx,
		strings.Join(fallbackClauses, " AND "),
		limitIdx,
		limitIdx,
	)

	return entityQuery{
		entityType: search.TypeBankTransaction,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, txnID, narration, status string
				var amt int64
				if err := rows.Scan(&id, &txnID, &narration, &amt, &status); err != nil {
					return nil, err
				}
				title := txnID
				if title == "" {
					title = "Bank Transaction"
				}
				sub := fmt.Sprintf("%s · ₹%d", status, amt/100)
				if narration != "" {
					sub += " · " + truncate(narration, 50)
				}
				out = append(out, search.Result{
					Type:     search.TypeBankTransaction,
					ID:       id,
					Title:    title,
					Subtitle: sub,
				})
			}
			return out, nil
		},
	}
}

// buildSettlementsQuery covers gateway settlements, UTR tie-outs, and recon status (Migration 026).
func (r *SearchRepo) buildSettlementsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("cf_settlement_id ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("utr ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(order_id IS NOT NULL AND order_id ILIKE $%d ESCAPE '\\')", len(args)))
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, p.Query, limit)
	qIdx := len(args) - 1
	limitIdx := len(args)

	sql := fmt.Sprintf(`
		SELECT id::text, cf_settlement_id, COALESCE(utr, ''), net_amount_paise, settlement_status, reconciliation_status
		FROM gateway_settlements
		WHERE property_id = $1
		  AND %s
		ORDER BY
		  CASE
		    WHEN UPPER(utr) = UPPER($%d) THEN 1000
		    WHEN utr ILIKE ($%d || '%%') THEN 800
		    WHEN UPPER(cf_settlement_id) = UPPER($%d) THEN 700
		    WHEN cf_settlement_id ILIKE ($%d || '%%') THEN 500
		    ELSE 100
		  END DESC,
		  created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		qIdx, qIdx, qIdx, qIdx, limitIdx,
	)

	return entityQuery{
		entityType: search.TypeSettlement,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, cfID, utr, setStatus, reconStatus string
				var netAmt int64
				if err := rows.Scan(&id, &cfID, &utr, &netAmt, &setStatus, &reconStatus); err != nil {
					return nil, err
				}
				title := "Settlement " + cfID
				if utr != "" {
					title = "Settlement " + utr
				}
				sub := fmt.Sprintf("%s (%s) · ₹%d", setStatus, reconStatus, netAmt/100)
				out = append(out, search.Result{
					Type:     search.TypeSettlement,
					ID:       id,
					Title:    title,
					Subtitle: sub,
				})
			}
			return out, nil
		},
	}
}

// buildRefundsQuery covers gateway refunds (Migration 020).
func (r *SearchRepo) buildRefundsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("cf_refund_id ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(provider_refund_id IS NOT NULL AND provider_refund_id ILIKE $%d ESCAPE '\\')", len(args)))
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, p.Query, limit)
	qIdx := len(args) - 1
	limitIdx := len(args)

	sql := fmt.Sprintf(`
		SELECT id::text, COALESCE(cf_refund_id, provider_refund_id, id::text), amount_paise, status
		FROM gateway_refunds
		WHERE property_id = $1
		  AND %s
		ORDER BY
		  CASE
		    WHEN UPPER(cf_refund_id) = UPPER($%d) THEN 1000
		    WHEN cf_refund_id ILIKE ($%d || '%%') THEN 800
		    ELSE 100
		  END DESC,
		  created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		qIdx, qIdx, limitIdx,
	)

	return entityQuery{
		entityType: search.TypeRefund,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, cfRefundID, status string
				var amt int64
				if err := rows.Scan(&id, &cfRefundID, &amt, &status); err != nil {
					return nil, err
				}
				out = append(out, search.Result{
					Type:     search.TypeRefund,
					ID:       id,
					Title:    "Refund " + cfRefundID,
					Subtitle: fmt.Sprintf("%s · ₹%d", status, amt/100),
				})
			}
			return out, nil
		},
	}
}

// buildPayoutsQuery covers payout payees (Migration 022).
func (r *SearchRepo) buildPayoutsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("name ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(phone IS NOT NULL AND phone ILIKE $%d ESCAPE '\\')", len(args)))
		sub = append(sub, fmt.Sprintf("(account_number_last4 IS NOT NULL AND account_number_last4 ILIKE $%d ESCAPE '\\')", len(args)))
		sub = append(sub, fmt.Sprintf("(upi_vpa IS NOT NULL AND upi_vpa ILIKE $%d ESCAPE '\\')", len(args)))

		if utf8.RuneCountInString(t.Value) >= 4 {
			args = append(args, t.Value)
			sub = append(sub, fmt.Sprintf("($%d::text <%% name::text)", len(args)))
		}
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, p.Query, limit)
	qIdx := len(args) - 1
	limitIdx := len(args)

	sql := fmt.Sprintf(`
		SELECT id::text, name, COALESCE(phone, ''), COALESCE(upi_vpa, ''), payee_type
		FROM payout_payees
		WHERE property_id = $1
		  AND %s
		ORDER BY
		  CASE
		    WHEN UPPER(name) = UPPER($%d) THEN 1000
		    WHEN name ILIKE ($%d || '%%') THEN 800
		    WHEN (name ILIKE ($%d || '%%') OR name ILIKE ('%% ' || $%d || '%%')) THEN 600
		    WHEN name ILIKE ('%%' || $%d || '%%') THEN 400
		    ELSE (word_similarity($%d, name) * 200)::int
		  END DESC,
		  created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		qIdx, qIdx, qIdx, qIdx, qIdx, qIdx, limitIdx,
	)

	return entityQuery{
		entityType: search.TypePayout,
		sql:        sql,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var id, name, phone, upiVPA, payeeType string
				if err := rows.Scan(&id, &name, &phone, &upiVPA, &payeeType); err != nil {
					return nil, err
				}
				sub := payeeType
				if phone != "" {
					sub += " · " + phone
				} else if upiVPA != "" {
					sub += " · " + upiVPA
				}
				out = append(out, search.Result{
					Type:     search.TypePayout,
					ID:       id,
					Title:    name,
					Subtitle: sub,
				})
			}
			return out, nil
		},
	}
}

// buildDocumentsLexicalQuery is retained as an unexported helper used by search_repo_live_test.go
// RBAC isolation tests. Production search does NOT use search_documents.
func (r *SearchRepo) buildDocumentsLexicalQuery(p search.Params, like string, limit int) entityQuery {
	allowedDocTypes := search.AllowedDocumentEntityTypes(domain.Role(p.Role))
	q := `
		SELECT entity_type, entity_id::text, title, body
		FROM search_documents
		WHERE property_id = $1
		  AND (title ILIKE $2 ESCAPE '\' OR body ILIKE $2 ESCAPE '\')
		  AND entity_type = ANY($4)`
	args := []any{p.PropertyID, like, limit, allowedDocTypes}
	n := 5
	if domain.Role(p.Role) == domain.RoleTenant {
		if p.TenantID != nil {
			q += fmt.Sprintf(` AND tenant_id = $%d`, n)
			args = append(args, *p.TenantID)
		}
	}
	q += ` ORDER BY updated_at DESC LIMIT $3`

	return entityQuery{
		entityType: search.TypeDocument,
		sql:        q,
		args:       args,
		scan: func(rows pgx.Rows) ([]search.Result, error) {
			var out []search.Result
			for rows.Next() {
				var entityType, id, title, body string
				if err := rows.Scan(&entityType, &id, &title, &body); err != nil {
					return nil, err
				}
				out = append(out, search.Result{
					Type:     search.TypeDocument,
					ID:       id,
					Title:    title,
					Subtitle: truncate(body, 80),
				})
			}
			return out, nil
		},
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// searchDocumentsLexical is an unexported helper retained for search_repo_live_test.go
// RBAC isolation tests. It validates that the search_documents query correctly enforces
// entity_type and tenant_id scoping so that no future reintroduction can regress RBAC.
func (r *SearchRepo) searchDocumentsLexical(ctx context.Context, p search.Params, like string, limit int) ([]search.Result, error) {
	// Fail-closed: RoleTenant with no TenantID must never query search_documents.
	if domain.Role(p.Role) == domain.RoleTenant && p.TenantID == nil {
		return nil, nil
	}
	eq := r.buildDocumentsLexicalQuery(p, like, limit)
	rows, err := r.db.Query(ctx, eq.sql, eq.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return eq.scan(rows)
}

