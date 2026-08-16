package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type DueRepo struct{ db DBTX }

func NewDueRepo(db DBTX) *DueRepo { return &DueRepo{db: db} }

func (r *DueRepo) WithTx(tx pgx.Tx) *DueRepo { return &DueRepo{db: tx} }

func (r *DueRepo) Create(ctx context.Context, d *domain.Due) error {
	now := time.Now().UTC()
	d.CreatedAt = now
	d.UpdatedAt = now
	if d.Status == "" {
		d.Status = domain.DueStatusPending
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO dues (
			due_code, tenant_id, property_id, kind, amount, original_amount,
			period_start, period_end, due_date, status, paid_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id`,
		d.DueCode, d.TenantID, d.PropertyID, d.Kind, d.Amount, d.OriginalAmount,
		d.PeriodStart, d.PeriodEnd, d.DueDate, d.Status, d.PaidAt, d.CreatedAt, d.UpdatedAt,
	).Scan(&d.ID)
}

func (r *DueRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Due, error) {
	return r.scanOne(ctx, `SELECT `+dueCols+` FROM dues WHERE id=$1`, id)
}

func (r *DueRepo) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Due, error) {
	return r.scanOne(ctx, `SELECT `+dueCols+` FROM dues WHERE id=$1 FOR UPDATE`, id)
}

func (r *DueRepo) GetByDueCode(ctx context.Context, code string) (*domain.Due, error) {
	return r.scanOne(ctx, `SELECT `+dueCols+` FROM dues WHERE due_code=$1`, code)
}

const dueCols = `id, due_code, tenant_id, property_id, kind, amount, original_amount,
	period_start, period_end, due_date, status, paid_at, created_at, updated_at`

func (r *DueRepo) scanOne(ctx context.Context, q string, args ...any) (*domain.Due, error) {
	var d domain.Due
	err := r.db.QueryRow(ctx, q, args...).Scan(
		&d.ID, &d.DueCode, &d.TenantID, &d.PropertyID, &d.Kind, &d.Amount, &d.OriginalAmount,
		&d.PeriodStart, &d.PeriodEnd, &d.DueDate, &d.Status, &d.PaidAt, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *DueRepo) Update(ctx context.Context, d *domain.Due) error {
	d.UpdatedAt = time.Now().UTC()
	_, err := r.db.Exec(ctx, `
		UPDATE dues SET amount=$2, status=$3, paid_at=$4, updated_at=$5 WHERE id=$1`,
		d.ID, d.Amount, d.Status, d.PaidAt, d.UpdatedAt,
	)
	return err
}

func (r *DueRepo) HasOpenRentDue(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM dues
			WHERE tenant_id=$1 AND kind='rent' AND status IN ('pending','partial')
		)`, tenantID).Scan(&exists)
	return exists, err
}

type DueListFilter struct {
	PropertyID uuid.UUID
	TenantID   *uuid.UUID
	Kind       *domain.DueKind
	Status     *domain.DueStatus
}

func (r *DueRepo) List(ctx context.Context, f DueListFilter) ([]domain.Due, error) {
	var b strings.Builder
	b.WriteString(`SELECT ` + dueCols + ` FROM dues WHERE property_id=$1`)
	args := []any{f.PropertyID}
	n := 2
	if f.TenantID != nil {
		fmt.Fprintf(&b, ` AND tenant_id=$%d`, n)
		args = append(args, *f.TenantID)
		n++
	}
	if f.Kind != nil {
		fmt.Fprintf(&b, ` AND kind=$%d`, n)
		args = append(args, *f.Kind)
		n++
	}
	if f.Status != nil {
		fmt.Fprintf(&b, ` AND status=$%d`, n)
		args = append(args, *f.Status)
		n++
	}
	b.WriteString(` ORDER BY due_date DESC`)
	rows, err := r.db.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDues(rows)
}

func (r *DueRepo) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Due, error) {
	rows, err := r.db.Query(ctx, `SELECT `+dueCols+` FROM dues WHERE tenant_id=$1 ORDER BY due_date DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDues(rows)
}

// ActivePendingRentDues joins tenants.status=active — used by reminder/billing jobs.
func (r *DueRepo) ActivePendingRentDues(ctx context.Context) ([]domain.Due, error) {
	rows, err := r.db.Query(ctx, `
		SELECT d.id, d.due_code, d.tenant_id, d.property_id, d.kind, d.amount, d.original_amount,
			d.period_start, d.period_end, d.due_date, d.status, d.paid_at, d.created_at, d.updated_at
		FROM dues d
		JOIN tenants t ON t.id = d.tenant_id
		WHERE d.status IN ('pending', 'partial')
		  AND d.kind = 'rent'
		  AND t.status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDues(rows)
}

func (r *DueRepo) FindByAmountAndDateWindow(ctx context.Context, propertyID uuid.UUID, amount int, from, to time.Time) ([]domain.Due, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+dueCols+` FROM dues
		WHERE property_id=$1 AND amount=$2 AND status IN ('pending','partial')
		  AND due_date >= $3 AND due_date <= $4`, propertyID, amount, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDues(rows)
}

func scanDues(rows pgx.Rows) ([]domain.Due, error) {
	var out []domain.Due
	for rows.Next() {
		var d domain.Due
		if err := rows.Scan(
			&d.ID, &d.DueCode, &d.TenantID, &d.PropertyID, &d.Kind, &d.Amount, &d.OriginalAmount,
			&d.PeriodStart, &d.PeriodEnd, &d.DueDate, &d.Status, &d.PaidAt, &d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
