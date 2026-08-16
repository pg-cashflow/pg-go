package postgres

import (
	"context"
	"crypto/rand"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type PropertyRepo struct{ db DBTX }

func NewPropertyRepo(db DBTX) *PropertyRepo { return &PropertyRepo{db: db} }

func (r *PropertyRepo) WithTx(tx pgx.Tx) *PropertyRepo { return &PropertyRepo{db: tx} }

func (r *PropertyRepo) Create(ctx context.Context, p *domain.Property) error {
	if p.PaymentMode == "" {
		p.PaymentMode = domain.PaymentModeManual
	}
	if p.InviteCode == "" {
		code, err := randomInviteCode()
		if err != nil {
			return err
		}
		p.InviteCode = code
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO properties (name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id, created_at`,
		p.Name, p.Address, p.OwnerPhone, p.UPIVPA, p.OwnerName, p.OwnerEmail, p.InviteCode, p.PaymentMode,
	).Scan(&p.ID, &p.CreatedAt)
}

func (r *PropertyRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Property, error) {
	return scanProperty(r.db.QueryRow(ctx, `
		SELECT id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode, created_at
		FROM properties WHERE id=$1`, id))
}

func (r *PropertyRepo) GetByOwnerPhone(ctx context.Context, phone string) (*domain.Property, error) {
	return scanProperty(r.db.QueryRow(ctx, `
		SELECT id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode, created_at
		FROM properties WHERE owner_phone=$1
		ORDER BY created_at ASC LIMIT 1`, phone))
}

func (r *PropertyRepo) GetByInviteCode(ctx context.Context, code string) (*domain.Property, error) {
	return scanProperty(r.db.QueryRow(ctx, `
		SELECT id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode, created_at
		FROM properties WHERE invite_code=$1`, code))
}

func (r *PropertyRepo) SetInviteCode(ctx context.Context, id uuid.UUID, code string) error {
	_, err := r.db.Exec(ctx, `UPDATE properties SET invite_code=$2 WHERE id=$1`, id, code)
	return err
}

func (r *PropertyRepo) List(ctx context.Context) ([]domain.Property, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode, created_at
		FROM properties ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Property
	for rows.Next() {
		p, err := scanProperty(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func scanProperty(row pgx.Row) (*domain.Property, error) {
	var p domain.Property
	err := row.Scan(&p.ID, &p.Name, &p.Address, &p.OwnerPhone, &p.UPIVPA, &p.OwnerName, &p.OwnerEmail, &p.InviteCode, &p.PaymentMode, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	if p.PaymentMode == "" {
		p.PaymentMode = domain.PaymentModeManual
	}
	return &p, nil
}

func randomInviteCode() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	out := make([]byte, 8)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out), nil
}

type TenantRepo struct{ db DBTX }

func NewTenantRepo(db DBTX) *TenantRepo { return &TenantRepo{db: db} }

func (r *TenantRepo) WithTx(tx pgx.Tx) *TenantRepo { return &TenantRepo{db: tx} }

func (r *TenantRepo) Create(ctx context.Context, t *domain.Tenant) error {
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = domain.TenantStatusActive
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO tenants (
			property_id, name, phone, room_number, aadhaar_last4, rent_amount, due_day,
			notice_period_days, notice_given_at, credit_balance_paise, status, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id`,
		t.PropertyID, t.Name, t.Phone, t.RoomNumber, t.AadhaarLast4, t.RentAmount, t.DueDay,
		t.NoticePeriodDays, t.NoticeGivenAt, t.CreditBalancePaise, t.Status, t.CreatedAt, t.UpdatedAt,
	).Scan(&t.ID)
}

func (r *TenantRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	var t domain.Tenant
	err := r.db.QueryRow(ctx, `
		SELECT id, property_id, name, phone, room_number, aadhaar_last4, rent_amount, due_day,
			notice_period_days, notice_given_at, credit_balance_paise, status, created_at, updated_at
		FROM tenants WHERE id=$1`, id).Scan(
		&t.ID, &t.PropertyID, &t.Name, &t.Phone, &t.RoomNumber, &t.AadhaarLast4, &t.RentAmount, &t.DueDay,
		&t.NoticePeriodDays, &t.NoticeGivenAt, &t.CreditBalancePaise, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *TenantRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.Tenant, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, name, phone, room_number, aadhaar_last4, rent_amount, due_day,
			notice_period_days, notice_given_at, credit_balance_paise, status, created_at, updated_at
		FROM tenants WHERE property_id=$1 ORDER BY created_at DESC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tenant
	for rows.Next() {
		var t domain.Tenant
		if err := rows.Scan(
			&t.ID, &t.PropertyID, &t.Name, &t.Phone, &t.RoomNumber, &t.AadhaarLast4, &t.RentAmount, &t.DueDay,
			&t.NoticePeriodDays, &t.NoticeGivenAt, &t.CreditBalancePaise, &t.Status, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *TenantRepo) Update(ctx context.Context, t *domain.Tenant) error {
	t.UpdatedAt = time.Now().UTC()
	_, err := r.db.Exec(ctx, `
		UPDATE tenants SET name=$2, phone=$3, room_number=$4, aadhaar_last4=$5, rent_amount=$6,
			due_day=$7, notice_period_days=$8, notice_given_at=$9, credit_balance_paise=$10,
			status=$11, updated_at=$12
		WHERE id=$1`,
		t.ID, t.Name, t.Phone, t.RoomNumber, t.AadhaarLast4, t.RentAmount,
		t.DueDay, t.NoticePeriodDays, t.NoticeGivenAt, t.CreditBalancePaise,
		t.Status, t.UpdatedAt,
	)
	return err
}

func (r *TenantRepo) ListActiveByDueDay(ctx context.Context, dueDay int, propertyID *uuid.UUID) ([]domain.Tenant, error) {
	q := `
		SELECT id, property_id, name, phone, room_number, aadhaar_last4, rent_amount, due_day,
			notice_period_days, notice_given_at, credit_balance_paise, status, created_at, updated_at
		FROM tenants WHERE status='active' AND due_day=$1`
	args := []any{dueDay}
	if propertyID != nil {
		q += ` AND property_id=$2`
		args = append(args, *propertyID)
	}
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tenant
	for rows.Next() {
		var t domain.Tenant
		if err := rows.Scan(
			&t.ID, &t.PropertyID, &t.Name, &t.Phone, &t.RoomNumber, &t.AadhaarLast4, &t.RentAmount, &t.DueDay,
			&t.NoticePeriodDays, &t.NoticeGivenAt, &t.CreditBalancePaise, &t.Status, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *TenantRepo) GetByPhone(ctx context.Context, phone string) (*domain.Tenant, error) {
	var t domain.Tenant
	err := r.db.QueryRow(ctx, `
		SELECT id, property_id, name, phone, room_number, aadhaar_last4, rent_amount, due_day,
			notice_period_days, notice_given_at, credit_balance_paise, status, created_at, updated_at
		FROM tenants WHERE phone=$1`, phone).Scan(
		&t.ID, &t.PropertyID, &t.Name, &t.Phone, &t.RoomNumber, &t.AadhaarLast4, &t.RentAmount, &t.DueDay,
		&t.NoticePeriodDays, &t.NoticeGivenAt, &t.CreditBalancePaise, &t.Status, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
