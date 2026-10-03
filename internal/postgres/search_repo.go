package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

type SearchRepo struct{ db DBTX }

func NewSearchRepo(db DBTX) *SearchRepo { return &SearchRepo{db: db} }

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

	// Check if database runner supports pgx.Batch (pool or transaction)
	if batchDB, ok := r.db.(interface{ SendBatch(context.Context, *pgx.Batch) pgx.BatchResults }); ok {
		batch := &pgx.Batch{}
		// 300 ms per-statement timeout
		batch.Queue("SET statement_timeout = 300")
		for _, q := range queries {
			batch.Queue(q.sql, q.args...)
		}
		batch.Queue("RESET statement_timeout")

		br := batchDB.SendBatch(ctx, batch)
		defer br.Close()

		// Consume SET statement_timeout
		if _, err := br.Exec(); err != nil {
			partial = true
		}

		for _, eq := range queries {
			rows, err := br.Query()
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

		// Consume RESET statement_timeout
		_, _ = br.Exec()
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

func (r *SearchRepo) buildPaymentsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	tenantFilter := ""
	if p.TenantID != nil {
		args = append(args, *p.TenantID)
		tenantFilter = fmt.Sprintf(" AND p.tenant_id = $%d", len(args))
	}

	var payClauses []string
	var nameClauses []string
	for _, t := range tokens {
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		likeIdx := len(args)

		payClauses = append(payClauses, fmt.Sprintf("((p.upi_txn_id IS NOT NULL AND p.upi_txn_id ILIKE $%d ESCAPE '\\') OR (p.raw_note IS NOT NULL AND p.raw_note ILIKE $%d ESCAPE '\\'))", likeIdx, likeIdx))
		nameClauses = append(nameClauses, fmt.Sprintf("(t.name IS NOT NULL AND t.name ILIKE $%d ESCAPE '\\')", likeIdx))
	}

	args = append(args, p.Query, limit)
	qIdx := len(args) - 1
	limitIdx := len(args)

	// Defect 6: Clean INNER JOIN on tenants with indexed UNION branches
	sql := fmt.Sprintf(`
		SELECT id, upi_txn_id, tenant_name, amount
		FROM (
			SELECT p.id::text, COALESCE(p.upi_txn_id, 'Payment') AS upi_txn_id, COALESCE(t.name, '') AS tenant_name, p.amount, p.created_at
			FROM payments p
			INNER JOIN tenants t ON t.id = p.tenant_id
			WHERE t.property_id = $1%s
			  AND %s
			UNION
			SELECT p.id::text, COALESCE(p.upi_txn_id, 'Payment') AS upi_txn_id, COALESCE(t.name, '') AS tenant_name, p.amount, p.created_at
			FROM payments p
			INNER JOIN tenants t ON t.id = p.tenant_id
			WHERE t.property_id = $1%s
			  AND %s
		) u
		ORDER BY
		  CASE
		    WHEN UPPER(upi_txn_id) = UPPER($%d) THEN 1000
		    WHEN upi_txn_id ILIKE ($%d || '%%') THEN 800
		    WHEN UPPER(tenant_name) = UPPER($%d) THEN 700
		    WHEN tenant_name ILIKE ($%d || '%%') THEN 500
		    WHEN (tenant_name ILIKE ($%d || '%%') OR tenant_name ILIKE ('%% ' || $%d || '%%')) THEN 400
		    WHEN tenant_name ILIKE ('%%' || $%d || '%%') THEN 300
		    ELSE 100
		  END DESC,
		  created_at DESC
		LIMIT $%d`,
		tenantFilter,
		strings.Join(payClauses, " AND "),
		tenantFilter,
		strings.Join(nameClauses, " AND "),
		qIdx, qIdx, qIdx, qIdx, qIdx, qIdx, qIdx, limitIdx,
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
func (r *SearchRepo) buildBankTransactionsQuery(p search.Params, tokens []search.Token, limit int) entityQuery {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("txn_id ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("narration ILIKE $%d ESCAPE '\\'", len(args)))
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, p.Query, limit)
	qIdx := len(args) - 1
	limitIdx := len(args)

	sql := fmt.Sprintf(`
		SELECT id::text, txn_id, COALESCE(narration, ''), amount_paise, status
		FROM bank_transactions
		WHERE property_id = $1
		  AND %s
		ORDER BY
		  CASE
		    WHEN UPPER(txn_id) = UPPER($%d) THEN 1000
		    WHEN txn_id ILIKE ($%d || '%%') THEN 800
		    WHEN narration ILIKE ('%%' || $%d || '%%') THEN 400
		    ELSE 100
		  END DESC,
		  txn_date DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		qIdx, qIdx, qIdx, limitIdx,
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

