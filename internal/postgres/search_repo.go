package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

type SearchRepo struct{ db DBTX }

func NewSearchRepo(db DBTX) *SearchRepo { return &SearchRepo{db: db} }

func (r *SearchRepo) SearchLexical(ctx context.Context, p search.Params, types []search.EntityType, perType int) ([]search.Result, error) {
	if perType <= 0 {
		perType = 5
	}
	tokens := p.Tokens
	if len(tokens) == 0 && p.Query != "" {
		tokens = search.TokenizeQuery(p.Query)
	}
	if len(tokens) == 0 {
		return nil, nil
	}

	var out []search.Result

	for _, t := range types {
		switch t {
		case search.TypeTenant:
			if p.Role == string(domain.RoleTenant) || p.TenantID != nil {
				continue
			}
			rs, err := r.searchTenants(ctx, p, tokens, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)

		case search.TypeDue:
			if p.Role == string(domain.RoleManager) {
				continue
			}
			if p.Role == string(domain.RoleTenant) && p.TenantID == nil {
				continue
			}
			rs, err := r.searchDues(ctx, p, tokens, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)

		case search.TypePayment:
			if p.Role == string(domain.RoleManager) {
				continue
			}
			if p.Role == string(domain.RoleTenant) && p.TenantID == nil {
				continue
			}
			rs, err := r.searchPayments(ctx, p, tokens, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)

		case search.TypePaymentReport:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			rs, err := r.searchPaymentReports(ctx, p, tokens, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)

		case search.TypeJoinRequest:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			rs, err := r.searchJoinRequests(ctx, p, tokens, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)

		case search.TypeEvent:
			if p.Role != string(domain.RoleOwner) {
				continue
			}
			rs, err := r.searchEvents(ctx, p, tokens, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)

		case search.TypeInspection:
			if p.Role == string(domain.RoleTenant) {
				continue
			}
			rs, err := r.searchInspections(ctx, p, tokens, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)

		case search.TypeHazard:
			if p.Role == string(domain.RoleTenant) {
				continue
			}
			rs, err := r.searchHazards(ctx, p, tokens, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)

		case search.TypeViolation:
			if p.Role == string(domain.RoleTenant) {
				continue
			}
			rs, err := r.searchViolations(ctx, p, tokens, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)

		case search.TypeDocument:
			likeVal := search.LikePattern(p.Query)
			rs, err := r.searchDocumentsLexical(ctx, p, likeVal, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		}
	}

	return out, nil
}

func (r *SearchRepo) SearchVector(ctx context.Context, p search.Params, limit int, embedding []float32) ([]search.Result, error) {
	allowedDocTypes := search.AllowedDocumentEntityTypes(domain.Role(p.Role))
	if len(allowedDocTypes) == 0 {
		return nil, nil
	}

	q := `
		SELECT entity_type, entity_id::text, title, body
		FROM search_documents
		WHERE property_id = $1
		  AND entity_type = ANY($4)`
	args := []any{p.PropertyID, pgVectorLiteral(embedding), limit, allowedDocTypes}
	n := 5
	if domain.Role(p.Role) == domain.RoleTenant {
		if p.TenantID == nil {
			return nil, nil
		}
		q += fmt.Sprintf(` AND tenant_id = $%d`, n)
		args = append(args, *p.TenantID)
	}
	q += ` ORDER BY embedding <=> $2 LIMIT $3`
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
}

func pgVectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// searchTenants executes live search across tenants.
// Manager: name, room, and exact 10-digit phone only (no partial phone).
// Owner: name, room, and partial/full phone.
// Multi-token: all tokens must match (AND conjunction).
func (r *SearchRepo) searchTenants(ctx context.Context, p search.Params, tokens []search.Token, limit int) ([]search.Result, error) {
	isManager := p.Role == string(domain.RoleManager)

	args := []any{p.PropertyID}
	var tokenClauses []string

	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("name ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(room_number IS NOT NULL AND room_number ILIKE $%d ESCAPE '\\')", len(args)))

		// Fuzzy typo tolerance on names only (4+ characters)
		if len(t.Value) >= 4 {
			args = append(args, t.Value)
			sub = append(sub, fmt.Sprintf("($%d::text <%% name::text)", len(args)))
		}

		// Phone handling:
		if isManager {
			// Manager: exact 10-digit phone only (no partial scraping)
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

	args = append(args, p.Query, search.LikePattern(p.Query), limit)
	qIdx := len(args) - 2
	prefixIdx := len(args) - 1
	limitIdx := len(args)

	sql := fmt.Sprintf(`
		SELECT id::text, name, COALESCE(room_number, ''), status
		FROM tenants
		WHERE property_id = $1
		  AND status != 'archived'
		  AND %s
		ORDER BY
		  CASE
		    WHEN UPPER(name) = UPPER($%d) THEN 1000
		    WHEN name ILIKE $%d THEN 500
		    WHEN ($%d::text <%% name::text) THEN 200
		    ELSE 100
		  END DESC,
		  created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		qIdx, prefixIdx, qIdx, limitIdx,
	)

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
}

// searchDues searches dues for owner or scoped tenant.
// Fuzzy matching is never allowed on due codes.
func (r *SearchRepo) searchDues(ctx context.Context, p search.Params, tokens []search.Token, limit int) ([]search.Result, error) {
	args := []any{p.PropertyID}
	tenantFilter := ""
	if p.TenantID != nil {
		args = append(args, *p.TenantID)
		tenantFilter = fmt.Sprintf(" AND d.tenant_id = $%d", len(args))
	}

	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("d.due_code ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(t.name IS NOT NULL AND t.name ILIKE $%d ESCAPE '\\')", len(args)))

		if len(t.Value) >= 4 {
			args = append(args, t.Value)
			sub = append(sub, fmt.Sprintf("(t.name IS NOT NULL AND $%d::text <%% t.name::text)", len(args)))
		}
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, p.Query, limit)
	qIdx := len(args) - 1
	limitIdx := len(args)

	sql := fmt.Sprintf(`
		SELECT d.id::text, d.due_code, d.status, COALESCE(t.name, '')
		FROM dues d
		LEFT JOIN tenants t ON t.id = d.tenant_id
		WHERE d.property_id = $1%s
		  AND %s
		ORDER BY
		  CASE
		    WHEN UPPER(d.due_code) = UPPER($%d) THEN 1000
		    WHEN d.due_code ILIKE $%d || '%%' THEN 500
		    ELSE 100
		  END DESC,
		  d.due_date DESC
		LIMIT $%d`,
		tenantFilter,
		strings.Join(tokenClauses, " AND "),
		qIdx, qIdx, limitIdx,
	)

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
}

// searchPayments searches payments.
// UTR matching is exact or prefix substring only (NO fuzzy matching on money identifiers).
func (r *SearchRepo) searchPayments(ctx context.Context, p search.Params, tokens []search.Token, limit int) ([]search.Result, error) {
	args := []any{p.PropertyID}
	tenantFilter := ""
	if p.TenantID != nil {
		args = append(args, *p.TenantID)
		tenantFilter = fmt.Sprintf(" AND p.tenant_id = $%d", len(args))
	}

	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		likeIdx := len(args)

		sub = append(sub, fmt.Sprintf("(p.upi_txn_id IS NOT NULL AND p.upi_txn_id ILIKE $%d ESCAPE '\\')", likeIdx))
		sub = append(sub, fmt.Sprintf("(p.raw_note IS NOT NULL AND p.raw_note ILIKE $%d ESCAPE '\\')", likeIdx))
		sub = append(sub, fmt.Sprintf("(t.name IS NOT NULL AND t.name ILIKE $%d ESCAPE '\\')", likeIdx))

		if len(t.Value) >= 4 {
			args = append(args, t.Value)
			sub = append(sub, fmt.Sprintf("(t.name IS NOT NULL AND $%d::text <%% t.name::text)", len(args)))
		}
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, p.Query, limit)
	qIdx := len(args) - 1
	limitIdx := len(args)

	sql := fmt.Sprintf(`
		SELECT p.id::text, COALESCE(p.upi_txn_id, 'Payment'), COALESCE(t.name, ''), p.amount
		FROM payments p
		LEFT JOIN tenants t ON t.id = p.tenant_id
		WHERE (t.property_id = $1 OR p.tenant_id IN (SELECT id FROM tenants WHERE property_id = $1))%s
		  AND %s
		ORDER BY
		  CASE
		    WHEN UPPER(COALESCE(p.upi_txn_id, '')) = UPPER($%d) THEN 1000
		    WHEN p.upi_txn_id ILIKE $%d || '%%' THEN 500
		    ELSE 100
		  END DESC,
		  p.created_at DESC
		LIMIT $%d`,
		tenantFilter,
		strings.Join(tokenClauses, " AND "),
		qIdx, qIdx, limitIdx,
	)

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
}

func (r *SearchRepo) searchPaymentReports(ctx context.Context, p search.Params, tokens []search.Token, limit int) ([]search.Result, error) {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("(pr.upi_txn_id ILIKE $%d ESCAPE '\\')", len(args)))
		sub = append(sub, fmt.Sprintf("(pr.note IS NOT NULL AND pr.note ILIKE $%d ESCAPE '\\')", len(args)))
		sub = append(sub, fmt.Sprintf("(t.name IS NOT NULL AND t.name ILIKE $%d ESCAPE '\\')", len(args)))
		tokenClauses = append(tokenClauses, "("+strings.Join(sub, " OR ")+")")
	}

	args = append(args, limit)
	sql := fmt.Sprintf(`
		SELECT pr.id::text, pr.upi_txn_id, pr.status, COALESCE(t.name, '')
		FROM payment_reports pr
		LEFT JOIN tenants t ON t.id = pr.tenant_id
		WHERE pr.property_id = $1
		  AND %s
		ORDER BY pr.created_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		len(args),
	)

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []search.Result
	for rows.Next() {
		var id, utr, status, tenantName string
		if err := rows.Scan(&id, &utr, &status, &tenantName); err != nil {
			return nil, err
		}
		sub := status
		if tenantName != "" {
			sub = tenantName + " · " + status
		}
		out = append(out, search.Result{
			Type:     search.TypePaymentReport,
			ID:       id,
			Title:    "Report: " + utr,
			Subtitle: sub,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchJoinRequests(ctx context.Context, p search.Params, tokens []search.Token, limit int) ([]search.Result, error) {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("name ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(phone IS NOT NULL AND phone ILIKE $%d ESCAPE '\\')", len(args)))
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

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []search.Result
	for rows.Next() {
		var id, name, status, phone string
		if err := rows.Scan(&id, &name, &status, &phone); err != nil {
			return nil, err
		}
		sub := "Join Request · " + status
		if phone != "" {
			sub = fmt.Sprintf("%s · %s", phone, status)
		}
		out = append(out, search.Result{
			Type:     search.TypeJoinRequest,
			ID:       id,
			Title:    name,
			Subtitle: sub,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchEvents(ctx context.Context, p search.Params, tokens []search.Token, limit int) ([]search.Result, error) {
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
		FROM events
		WHERE property_id = $1
		  AND %s
		ORDER BY occurred_at DESC
		LIMIT $%d`,
		strings.Join(tokenClauses, " AND "),
		len(args),
	)

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
}

func (r *SearchRepo) searchInspections(ctx context.Context, p search.Params, tokens []search.Token, limit int) ([]search.Result, error) {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("inspection_type ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(notes IS NOT NULL AND notes ILIKE $%d ESCAPE '\\')", len(args)))

		// Plain text FTS query using 'simple' configuration
		args = append(args, t.Raw)
		sub = append(sub, fmt.Sprintf("(notes IS NOT NULL AND to_tsvector('simple', notes) @@ plainto_tsquery('simple', $%d))", len(args)))

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

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
}

// searchHazards searches hazards.
// CRITICAL: Hazard reporter name is NEVER searched to preserve reporter anonymity.
func (r *SearchRepo) searchHazards(ctx context.Context, p search.Params, tokens []search.Token, limit int) ([]search.Result, error) {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("category ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(description IS NOT NULL AND description ILIKE $%d ESCAPE '\\')", len(args)))

		args = append(args, t.Raw)
		sub = append(sub, fmt.Sprintf("(description IS NOT NULL AND to_tsvector('simple', description) @@ plainto_tsquery('simple', $%d))", len(args)))

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

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
}

func (r *SearchRepo) searchViolations(ctx context.Context, p search.Params, tokens []search.Token, limit int) ([]search.Result, error) {
	args := []any{p.PropertyID}
	var tokenClauses []string
	for _, t := range tokens {
		var sub []string
		likeVal := search.LikePattern(t.Raw)
		args = append(args, likeVal)
		sub = append(sub, fmt.Sprintf("rule_code ILIKE $%d ESCAPE '\\'", len(args)))
		sub = append(sub, fmt.Sprintf("(description IS NOT NULL AND description ILIKE $%d ESCAPE '\\')", len(args)))

		args = append(args, t.Raw)
		sub = append(sub, fmt.Sprintf("(description IS NOT NULL AND to_tsvector('simple', description) @@ plainto_tsquery('simple', $%d))", len(args)))

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

	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (r *SearchRepo) UpsertDocument(ctx context.Context, propertyID uuid.UUID, tenantID *uuid.UUID, entityType string, entityID uuid.UUID, title, body string, embedding []float32) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO search_documents (property_id, tenant_id, entity_type, entity_id, title, body, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (property_id, entity_type, entity_id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			title = EXCLUDED.title,
			body = EXCLUDED.body,
			updated_at = NOW()`,
		propertyID, tenantID, entityType, entityID, title, body,
	)
	return err
}

func (r *SearchRepo) DeleteDocument(ctx context.Context, propertyID uuid.UUID, entityType string, entityID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `DELETE FROM search_documents WHERE property_id = $1 AND entity_type = $2 AND entity_id = $3`, propertyID, entityType, entityID)
	return err
}

func (r *SearchRepo) ListIndexSources(_ context.Context, _ uuid.UUID) ([]search.IndexSource, error) {
	return nil, nil
}

func (r *SearchRepo) searchDocumentsLexical(ctx context.Context, p search.Params, like string, limit int) ([]search.Result, error) {
	allowedDocTypes := search.AllowedDocumentEntityTypes(domain.Role(p.Role))
	if len(allowedDocTypes) == 0 {
		return nil, nil
	}

	q := `
		SELECT entity_type, entity_id::text, title, body
		FROM search_documents
		WHERE property_id = $1
		  AND (title ILIKE $2 ESCAPE '\' OR body ILIKE $2 ESCAPE '\')
		  AND entity_type = ANY($4)`
	args := []any{p.PropertyID, like, limit, allowedDocTypes}
	n := 5
	if domain.Role(p.Role) == domain.RoleTenant {
		if p.TenantID == nil {
			return nil, nil
		}
		q += fmt.Sprintf(` AND tenant_id = $%d`, n)
		args = append(args, *p.TenantID)
	}
	q += ` ORDER BY updated_at DESC LIMIT $3`
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
}
