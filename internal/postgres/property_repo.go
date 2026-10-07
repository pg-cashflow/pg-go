package postgres

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
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

func (r *PropertyRepo) GetByOwnerEmail(ctx context.Context, email string) (*domain.Property, error) {
	clean := strings.ToLower(strings.TrimSpace(email))
	return scanProperty(r.db.QueryRow(ctx, `
		SELECT id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode, created_at
		FROM properties WHERE LOWER(owner_email)=$1
		ORDER BY created_at ASC LIMIT 1`, clean))
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

// ErrPropertyNotFound is returned when archiving or unarchiving a property that does not exist.
var ErrPropertyNotFound = errors.New("property not found")

// Archive retires a property without deleting it. Archived properties are hidden from List and keep
// all financial history (hard deletes are blocked once audit rows exist). Archiving is idempotent.
func (r *PropertyRepo) Archive(ctx context.Context, id uuid.UUID) error {
	cmd, err := r.db.Exec(ctx, `UPDATE properties SET archived_at = COALESCE(archived_at, NOW()) WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrPropertyNotFound
	}
	return nil
}

// Unarchive restores an archived property.
func (r *PropertyRepo) Unarchive(ctx context.Context, id uuid.UUID) error {
	cmd, err := r.db.Exec(ctx, `UPDATE properties SET archived_at = NULL WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrPropertyNotFound
	}
	return nil
}

func (r *PropertyRepo) List(ctx context.Context) ([]domain.Property, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode, created_at
		FROM properties WHERE archived_at IS NULL ORDER BY created_at`)
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

func (r *PropertyRepo) GetSettings(ctx context.Context, propertyID uuid.UUID) (*domain.PropertySettings, error) {
	var s domain.PropertySettings
	var rawModules []byte
	var offsets []int32
	err := r.db.QueryRow(ctx, `
		SELECT property_id, payout_auto_dispatch, reminder_offsets, reminder_catch_up_days,
		       active_modules, auto_apply_credit, created_at, updated_at
		FROM property_settings WHERE property_id=$1`, propertyID).Scan(
		&s.PropertyID, &s.PayoutAutoDispatch, &offsets, &s.ReminderCatchUpDays,
		&rawModules, &s.AutoApplyCredit, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			defaults := domain.DefaultPropertySettings(propertyID)
			return &defaults, nil
		}
		return nil, err
	}
	s.ReminderOffsets = make([]int, len(offsets))
	for i, o := range offsets {
		s.ReminderOffsets[i] = int(o)
	}
	if len(rawModules) > 0 {
		_ = json.Unmarshal(rawModules, &s.ActiveModules)
	}
	return &s, nil
}

func (r *PropertyRepo) UpsertSettings(ctx context.Context, s *domain.PropertySettings) error {
	rawModules, err := json.Marshal(s.ActiveModules)
	if err != nil {
		return err
	}
	offsets := make([]int32, len(s.ReminderOffsets))
	for i, o := range s.ReminderOffsets {
		offsets[i] = int32(o) // #nosec G115
	}
	s.UpdatedAt = time.Now().UTC()
	_, err = r.db.Exec(ctx, `
		INSERT INTO property_settings (property_id, payout_auto_dispatch, reminder_offsets,
		                               reminder_catch_up_days, active_modules, auto_apply_credit, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (property_id) DO UPDATE SET
			payout_auto_dispatch=EXCLUDED.payout_auto_dispatch,
			reminder_offsets=EXCLUDED.reminder_offsets,
			reminder_catch_up_days=EXCLUDED.reminder_catch_up_days,
			active_modules=EXCLUDED.active_modules,
			auto_apply_credit=EXCLUDED.auto_apply_credit,
			updated_at=EXCLUDED.updated_at`,
		s.PropertyID, s.PayoutAutoDispatch, offsets, s.ReminderCatchUpDays,
		rawModules, s.AutoApplyCredit, s.UpdatedAt)
	return err
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

const tenantCols = `id, property_id, name, phone, room_number, room_id, aadhaar_last4, rent_amount, due_day,
	notice_period_days, notice_given_at, credit_balance_paise, status,
	permanent_address, current_address, parent_name, emergency_phone, joined_on, has_id_photo,
	majority_date, guardian_name, guardian_phone, guardian_relation, guardian_kyc_reference_id, guardian_consent_verified_at, is_gamification_disabled,
	created_at, updated_at`

func scanTenant(row pgx.Row) (*domain.Tenant, error) {
	var t domain.Tenant
	err := row.Scan(
		&t.ID, &t.PropertyID, &t.Name, &t.Phone, &t.RoomNumber, &t.RoomID, &t.AadhaarLast4, &t.RentAmount, &t.DueDay,
		&t.NoticePeriodDays, &t.NoticeGivenAt, &t.CreditBalancePaise, &t.Status,
		&t.PermanentAddress, &t.CurrentAddress, &t.ParentName, &t.EmergencyPhone, &t.JoinedOn, &t.HasIDPhoto,
		&t.MajorityDate, &t.GuardianName, &t.GuardianPhone, &t.GuardianRelation, &t.GuardianKYCReferenceID, &t.GuardianConsentVerifiedAt, &t.IsGamificationDisabled,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullIfEmptyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (r *TenantRepo) Create(ctx context.Context, t *domain.Tenant) error {
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = domain.TenantStatusActive
	}
	t.HasIDPhoto = len(t.IDPhotoBytes) > 0
	return r.db.QueryRow(ctx, `
		INSERT INTO tenants (
			property_id, name, phone, room_number, room_id, aadhaar_last4, rent_amount, due_day,
			notice_period_days, notice_given_at, credit_balance_paise, status,
			permanent_address, current_address, parent_name, emergency_phone, joined_on,
			id_photo_bytes, has_id_photo,
			majority_date, guardian_name, guardian_phone, guardian_relation, guardian_kyc_reference_id, guardian_consent_verified_at, is_gamification_disabled,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)
		RETURNING id`,
		t.PropertyID, t.Name, t.Phone, t.RoomNumber, t.RoomID, t.AadhaarLast4, t.RentAmount, t.DueDay,
		t.NoticePeriodDays, t.NoticeGivenAt, t.CreditBalancePaise, t.Status,
		t.PermanentAddress, t.CurrentAddress, t.ParentName, t.EmergencyPhone, t.JoinedOn,
		nullIfEmptyBytes(t.IDPhotoBytes), t.HasIDPhoto,
		t.MajorityDate, t.GuardianName, t.GuardianPhone, t.GuardianRelation, t.GuardianKYCReferenceID, t.GuardianConsentVerifiedAt, t.IsGamificationDisabled,
		t.CreatedAt, t.UpdatedAt,
	).Scan(&t.ID)
}

func (r *TenantRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return scanTenant(r.db.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id=$1`, id))
}

// GetByIDs loads many tenants in a single round trip (WHERE id = ANY(...)).
// IDs with no matching row are simply absent from the returned map. Intended for
// batch jobs (e.g. reminders) that would otherwise issue one GetByID per row.
func (r *TenantRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*domain.Tenant, error) {
	out := make(map[uuid.UUID]*domain.Tenant, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	rows, err := r.db.Query(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id = ANY($1::uuid[])`, strs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out[t.ID] = t
	}
	return out, rows.Err()
}

func (r *TenantRepo) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return scanTenant(r.db.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id=$1 FOR UPDATE`, id))
}

func (r *TenantRepo) AddCredit(ctx context.Context, tenantID uuid.UUID, deltaPaise int64) error {
	_, err := r.db.Exec(ctx, `
		UPDATE tenants 
		SET credit_balance_paise = GREATEST(0, credit_balance_paise + $2),
		    updated_at = NOW()
		WHERE id = $1`, tenantID, deltaPaise)
	return err
}

func (r *TenantRepo) DeductCredit(ctx context.Context, tenantID uuid.UUID, amountPaise int64) (int64, error) {
	var remaining int64
	err := r.db.QueryRow(ctx, `
		UPDATE tenants
		SET credit_balance_paise = GREATEST(0, credit_balance_paise - $2),
		    updated_at = NOW()
		WHERE id = $1
		RETURNING credit_balance_paise`, tenantID, amountPaise).Scan(&remaining)
	return remaining, err
}

func (r *TenantRepo) GetIDPhoto(ctx context.Context, id uuid.UUID) ([]byte, error) {
	var b []byte
	err := r.db.QueryRow(ctx, `SELECT id_photo_bytes FROM tenants WHERE id=$1`, id).Scan(&b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (r *TenantRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.Tenant, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+tenantCols+` FROM tenants WHERE property_id=$1 ORDER BY created_at DESC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (r *TenantRepo) Update(ctx context.Context, t *domain.Tenant) error {
	t.UpdatedAt = time.Now().UTC()
	_, err := r.db.Exec(ctx, `
		UPDATE tenants SET name=$2, phone=$3, room_number=$4, room_id=$5, aadhaar_last4=$6, rent_amount=$7,
			due_day=$8, notice_period_days=$9, notice_given_at=$10, credit_balance_paise=$11,
			status=$12, permanent_address=$13, current_address=$14, parent_name=$15,
			emergency_phone=$16, joined_on=$17,
			majority_date=$18, guardian_name=$19, guardian_phone=$20, guardian_relation=$21,
			guardian_kyc_reference_id=$22, guardian_consent_verified_at=$23, is_gamification_disabled=$24,
			updated_at=$25
		WHERE id=$1`,
		t.ID, t.Name, t.Phone, t.RoomNumber, t.RoomID, t.AadhaarLast4, t.RentAmount,
		t.DueDay, t.NoticePeriodDays, t.NoticeGivenAt, t.CreditBalancePaise,
		t.Status, t.PermanentAddress, t.CurrentAddress, t.ParentName,
		t.EmergencyPhone, t.JoinedOn,
		t.MajorityDate, t.GuardianName, t.GuardianPhone, t.GuardianRelation,
		t.GuardianKYCReferenceID, t.GuardianConsentVerifiedAt, t.IsGamificationDisabled,
		t.UpdatedAt,
	)
	return err
}

func (r *TenantRepo) ListActiveByDueDay(ctx context.Context, dueDay int, propertyID *uuid.UUID) ([]domain.Tenant, error) {
	q := `SELECT ` + tenantCols + ` FROM tenants WHERE status='active' AND due_day=$1`
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
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (r *TenantRepo) GetByPhone(ctx context.Context, phone string) (*domain.Tenant, error) {
	return scanTenant(r.db.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE phone=$1`, phone))
}
