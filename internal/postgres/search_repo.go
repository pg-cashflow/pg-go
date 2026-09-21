package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/search"
)

type SearchRepo struct{ db DBTX }

func NewSearchRepo(db DBTX) *SearchRepo { return &SearchRepo{db: db} }

func (r *SearchRepo) SearchLexical(ctx context.Context, p search.Params, types []search.EntityType, perType int) ([]search.Result, error) {
	var out []search.Result
	like := search.LikePattern(p.Query)
	upper := strings.ToUpper(p.Query)

	for _, t := range types {
		switch t {
		case search.TypeTenant:
			if p.TenantID != nil {
				continue
			}
			rs, err := r.searchTenants(ctx, p.PropertyID, like, upper, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		case search.TypeDue:
			rs, err := r.searchDues(ctx, p.PropertyID, p.TenantID, like, upper, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		case search.TypePayment:
			rs, err := r.searchPayments(ctx, p.PropertyID, p.TenantID, like, upper, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		case search.TypePaymentReport:
			if p.TenantID != nil {
				continue
			}
			rs, err := r.searchPaymentReports(ctx, p.PropertyID, like, upper, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		case search.TypeJoinRequest:
			if p.TenantID != nil {
				continue
			}
			rs, err := r.searchJoinRequests(ctx, p.PropertyID, like, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		case search.TypeEvent:
			if p.TenantID != nil {
				continue
			}
			rs, err := r.searchEvents(ctx, p.PropertyID, like, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		case search.TypeInspection:
			if p.TenantID != nil {
				continue
			}
			rs, err := r.searchInspections(ctx, p.PropertyID, like, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		case search.TypeHazard:
			if p.TenantID != nil {
				continue
			}
			rs, err := r.searchHazards(ctx, p.PropertyID, like, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		case search.TypeViolation:
			if p.TenantID != nil {
				continue
			}
			rs, err := r.searchViolations(ctx, p.PropertyID, like, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		case search.TypeDocument:
			// Vector index only; lexical document search uses title/body ILIKE fallback.
			rs, err := r.searchDocumentsLexical(ctx, p.PropertyID, p.TenantID, like, perType)
			if err != nil {
				return nil, err
			}
			out = append(out, rs...)
		}
	}
	return out, nil
}

func (r *SearchRepo) SearchVector(ctx context.Context, p search.Params, perType int, embedding []float32) ([]search.Result, error) {
	vec := pgVectorLiteral(embedding)
	q := `
		SELECT entity_type, entity_id::text, title, body,
		       1 - (embedding <=> $3::vector) AS score
		FROM search_documents
		WHERE property_id = $1`
	args := []any{p.PropertyID, perType, vec}
	n := 4
	if p.TenantID != nil {
		q += ` AND (tenant_id IS NULL OR tenant_id = $` + itoa(n) + `)`
		args = append(args, *p.TenantID)
		n++
	}
	q += ` AND embedding IS NOT NULL
		ORDER BY embedding <=> $3::vector
		LIMIT $2`
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.Result
	for rows.Next() {
		var entityType, id, title, body string
		var score float64
		if err := rows.Scan(&entityType, &id, &title, &body, &score); err != nil {
			return nil, err
		}
		out = append(out, search.Result{
			Type:     search.TypeDocument,
			ID:       id,
			Title:    title,
			Subtitle: truncate(body, 80),
			Path:     documentPath(entityType, id),
			Score:    score,
		})
	}
	return out, nil
}

func (r *SearchRepo) UpsertDocument(ctx context.Context, propertyID uuid.UUID, tenantID *uuid.UUID, entityType string, entityID uuid.UUID, title, body string, embedding []float32) error {
	vec := pgVectorLiteral(embedding)
	_, err := r.db.Exec(ctx, `
		INSERT INTO search_documents (property_id, tenant_id, entity_type, entity_id, title, body, embedding, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::vector,NOW())
		ON CONFLICT (property_id, entity_type, entity_id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			title = EXCLUDED.title,
			body = EXCLUDED.body,
			embedding = EXCLUDED.embedding,
			updated_at = NOW()`,
		propertyID, tenantID, entityType, entityID, title, body, vec)
	return err
}

func (r *SearchRepo) DeleteDocument(ctx context.Context, propertyID uuid.UUID, entityType string, entityID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `DELETE FROM search_documents WHERE property_id=$1 AND entity_type=$2 AND entity_id=$3`,
		propertyID, entityType, entityID)
	return err
}

func (r *SearchRepo) ListIndexSources(ctx context.Context, propertyID uuid.UUID) ([]search.IndexSource, error) {
	var out []search.IndexSource

	// Inspection notes
	rows, err := r.db.Query(ctx, `
		SELECT i.property_id, NULL::uuid, 'inspection', i.id,
		       CONCAT('Inspection ', i.inspection_type), i.notes
		FROM inspections i WHERE i.property_id = $1 AND i.notes <> ''`, propertyID)
	if err != nil {
		return nil, err
	}
	out = append(out, scanSources(rows)...)

	rows2, err := r.db.Query(ctx, `
		SELECT h.property_id, h.reported_by_tenant_id, 'hazard', h.id,
		       CONCAT('Hazard ', h.category), h.description
		FROM hazards h WHERE h.property_id = $1`, propertyID)
	if err != nil {
		return nil, err
	}
	out = append(out, scanSources(rows2)...)

	rows3, err := r.db.Query(ctx, `
		SELECT v.property_id, v.tenant_id, 'violation', v.id,
		       CONCAT('Violation ', v.rule_code), v.description
		FROM violations v WHERE v.property_id = $1`, propertyID)
	if err != nil {
		return nil, err
	}
	out = append(out, scanSources(rows3)...)

	rows4, err := r.db.Query(ctx, `
		SELECT d.property_id, p.tenant_id, 'payment_note', p.id,
		       COALESCE(p.upi_txn_id, 'Payment'), COALESCE(p.raw_note, '')
		FROM payments p
		JOIN dues d ON d.id = p.due_id
		WHERE d.property_id = $1 AND COALESCE(p.raw_note, '') <> ''`, propertyID)
	if err != nil {
		return nil, err
	}
	out = append(out, scanSources(rows4)...)

	return out, nil
}

func scanSources(rows interface{ Next() bool; Scan(...any) error; Close() }) []search.IndexSource {
	defer rows.Close()
	var out []search.IndexSource
	for rows.Next() {
		var s search.IndexSource
		if err := rows.Scan(&s.PropertyID, &s.TenantID, &s.EntityType, &s.EntityID, &s.Title, &s.Body); err != nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (r *SearchRepo) searchTenants(ctx context.Context, propertyID uuid.UUID, like, upper string, limit int) ([]search.Result, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id::text, name, COALESCE(room_number,''), status
		FROM tenants
		WHERE property_id = $1
		  AND (
		    name ILIKE $2 ESCAPE '\'
		    OR COALESCE(room_number,'') ILIKE $2 ESCAPE '\'
		    OR COALESCE(phone,'') ILIKE $2 ESCAPE '\'
		    OR UPPER(COALESCE(phone,'')) = $3
		  )
		ORDER BY name
		LIMIT $4`, propertyID, like, upper, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTenantResults(rows, "/owner/tenants?q=")
}

func (r *SearchRepo) searchDues(ctx context.Context, propertyID uuid.UUID, tenantID *uuid.UUID, like, upper string, limit int) ([]search.Result, error) {
	q := `
		SELECT d.id::text, d.due_code, d.status, COALESCE(t.name,'')
		FROM dues d
		LEFT JOIN tenants t ON t.id = d.tenant_id
		WHERE d.property_id = $1`
	args := []any{propertyID}
	if tenantID != nil {
		q += ` AND d.tenant_id = $2`
		args = append(args, *tenantID)
		q += ` AND (
			d.due_code ILIKE $3 ESCAPE '\'
			OR UPPER(d.due_code) = $4
			OR COALESCE(t.name,'') ILIKE $3 ESCAPE '\'
		)
		ORDER BY d.due_date DESC
		LIMIT $5`
		args = append(args, like, upper, limit)
	} else {
		q += ` AND (
			d.due_code ILIKE $2 ESCAPE '\'
			OR UPPER(d.due_code) = $3
			OR COALESCE(t.name,'') ILIKE $2 ESCAPE '\'
		)
		ORDER BY d.due_date DESC
		LIMIT $4`
		args = append(args, like, upper, limit)
	}
	rows, err := r.db.Query(ctx, q, args...)
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
		path := "/owner/dues?q=" + code
		if tenantID != nil {
			path = "/tenant/dues?q=" + code
		}
		out = append(out, search.Result{
			Type:     search.TypeDue,
			ID:       id,
			Title:    "Due " + code,
			Subtitle: strings.TrimSpace(tenantName + " · " + status),
			Path:     path,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchPayments(ctx context.Context, propertyID uuid.UUID, tenantID *uuid.UUID, like, upper string, limit int) ([]search.Result, error) {
	q := `
		SELECT p.id::text, COALESCE(p.upi_txn_id,''), COALESCE(t.name,''), p.amount
		FROM payments p
		JOIN tenants t ON t.id = p.tenant_id
		WHERE t.property_id = $1`
	args := []any{propertyID, like, upper, limit}
	if tenantID != nil {
		q += ` AND p.tenant_id = $5`
		args = append(args, *tenantID)
	}
	q += ` AND (
			COALESCE(p.upi_txn_id,'') ILIKE $2 ESCAPE '\'
			OR UPPER(COALESCE(p.upi_txn_id,'')) = $3
			OR COALESCE(p.raw_note,'') ILIKE $2 ESCAPE '\'
			OR t.name ILIKE $2 ESCAPE '\'
		)
		ORDER BY p.matched_at DESC
		LIMIT $4`
	rows, err := r.db.Query(ctx, q, args...)
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
		path := "/owner/payments?q=" + utr
		if tenantID != nil {
			path = "/tenant/payments?q=" + utr
		}
		out = append(out, search.Result{
			Type:     search.TypePayment,
			ID:       id,
			Title:    utr,
			Subtitle: fmt.Sprintf("%s · ₹%d", tenantName, amount/100),
			Path:     path,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchPaymentReports(ctx context.Context, propertyID uuid.UUID, like, upper string, limit int) ([]search.Result, error) {
	rows, err := r.db.Query(ctx, `
		SELECT pr.id::text, pr.upi_txn_id, pr.status, COALESCE(t.name,'')
		FROM payment_reports pr
		JOIN tenants t ON t.id = pr.tenant_id
		WHERE pr.property_id = $1
		  AND (
		    pr.upi_txn_id ILIKE $2 ESCAPE '\'
		    OR UPPER(pr.upi_txn_id) = $3
		    OR COALESCE(pr.note,'') ILIKE $2 ESCAPE '\'
		  )
		ORDER BY pr.created_at DESC
		LIMIT $4`, propertyID, like, upper, limit)
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
		out = append(out, search.Result{
			Type:     search.TypePaymentReport,
			ID:       id,
			Title:    "UTR " + utr,
			Subtitle: tenantName + " · " + status,
			Path:     "/owner/reports?q=" + utr,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchJoinRequests(ctx context.Context, propertyID uuid.UUID, like string, limit int) ([]search.Result, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id::text, COALESCE(name, phone), status
		FROM join_requests
		WHERE property_id = $1
		  AND status IN ('pending', 'approved')
		  AND (name ILIKE $2 ESCAPE '\' OR phone ILIKE $2 ESCAPE '\')
		ORDER BY created_at DESC
		LIMIT $3`, propertyID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.Result
	for rows.Next() {
		var id, label, status string
		if err := rows.Scan(&id, &label, &status); err != nil {
			return nil, err
		}
		out = append(out, search.Result{
			Type:     search.TypeJoinRequest,
			ID:       id,
			Title:    label,
			Subtitle: "Join · " + status,
			Path:     "/owner/joins?q=" + label,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchEvents(ctx context.Context, propertyID uuid.UUID, like string, limit int) ([]search.Result, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id::text, event_type, occurred_at::text
		FROM events
		WHERE property_id = $1
		  AND event_type ILIKE $2 ESCAPE '\'
		ORDER BY occurred_at DESC
		LIMIT $3`, propertyID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.Result
	for rows.Next() {
		var id, evType, occurred string
		if err := rows.Scan(&id, &evType, &occurred); err != nil {
			return nil, err
		}
		out = append(out, search.Result{
			Type:     search.TypeEvent,
			ID:       id,
			Title:    evType,
			Subtitle: occurred,
			Path:     "/owner/events?q=" + evType,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchInspections(ctx context.Context, propertyID uuid.UUID, like string, limit int) ([]search.Result, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id::text, inspection_type, COALESCE(notes,'')
		FROM inspections
		WHERE property_id = $1
		  AND (inspection_type ILIKE $2 ESCAPE '\' OR notes ILIKE $2 ESCAPE '\')
		ORDER BY inspected_at DESC
		LIMIT $3`, propertyID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.Result
	for rows.Next() {
		var id, typ, notes string
		if err := rows.Scan(&id, &typ, &notes); err != nil {
			return nil, err
		}
		out = append(out, search.Result{
			Type:     search.TypeInspection,
			ID:       id,
			Title:    "Inspection " + typ,
			Subtitle: truncate(notes, 60),
			Path:     "/manager/inspections/" + id,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchHazards(ctx context.Context, propertyID uuid.UUID, like string, limit int) ([]search.Result, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id::text, category, description
		FROM hazards
		WHERE property_id = $1
		  AND (category ILIKE $2 ESCAPE '\' OR description ILIKE $2 ESCAPE '\')
		ORDER BY created_at DESC
		LIMIT $3`, propertyID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.Result
	for rows.Next() {
		var id, cat, desc string
		if err := rows.Scan(&id, &cat, &desc); err != nil {
			return nil, err
		}
		out = append(out, search.Result{
			Type:     search.TypeHazard,
			ID:       id,
			Title:    "Hazard " + cat,
			Subtitle: truncate(desc, 60),
			Path:     "/manager/hazards?q=" + cat,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchViolations(ctx context.Context, propertyID uuid.UUID, like string, limit int) ([]search.Result, error) {
	rows, err := r.db.Query(ctx, `
		SELECT v.id::text, v.rule_code, v.description, COALESCE(t.name,'')
		FROM violations v
		JOIN tenants t ON t.id = v.tenant_id
		WHERE v.property_id = $1
		  AND (v.rule_code ILIKE $2 ESCAPE '\' OR v.description ILIKE $2 ESCAPE '\' OR t.name ILIKE $2 ESCAPE '\')
		ORDER BY v.created_at DESC
		LIMIT $3`, propertyID, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.Result
	for rows.Next() {
		var id, code, desc, tenantName string
		if err := rows.Scan(&id, &code, &desc, &tenantName); err != nil {
			return nil, err
		}
		out = append(out, search.Result{
			Type:     search.TypeViolation,
			ID:       id,
			Title:    "Violation " + code,
			Subtitle: truncate(tenantName+" · "+desc, 60),
			Path:     "/manager/violations?q=" + code,
		})
	}
	return out, nil
}

func (r *SearchRepo) searchDocumentsLexical(ctx context.Context, propertyID uuid.UUID, tenantID *uuid.UUID, like string, limit int) ([]search.Result, error) {
	q := `
		SELECT entity_type, entity_id::text, title, body
		FROM search_documents
		WHERE property_id = $1
		  AND (title ILIKE $2 ESCAPE '\' OR body ILIKE $2 ESCAPE '\')`
	args := []any{propertyID, like, limit}
	if tenantID != nil {
		q += ` AND (tenant_id IS NULL OR tenant_id = $4)`
		args = append(args, *tenantID)
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
			Path:     documentPath(entityType, id),
		})
	}
	return out, nil
}

func scanTenantResults(rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
}, pathPrefix string) ([]search.Result, error) {
	defer rows.Close()
	var out []search.Result
	for rows.Next() {
		var id, name, room, status string
		if err := rows.Scan(&id, &name, &room, &status); err != nil {
			return nil, err
		}
		sub := status
		if room != "" {
			sub = "Room " + room + " · " + status
		}
		out = append(out, search.Result{
			Type:     search.TypeTenant,
			ID:       id,
			Title:    name,
			Subtitle: sub,
			Path:     pathPrefix + name,
		})
	}
	return out, nil
}

func documentPath(entityType, id string) string {
	switch entityType {
	case "inspection":
		return "/manager/inspections/" + id
	case "hazard":
		return "/manager/hazards"
	case "violation":
		return "/manager/violations"
	default:
		return "/owner/events"
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func pgVectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
