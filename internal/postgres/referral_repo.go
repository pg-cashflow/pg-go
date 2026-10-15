package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrReferralNotFound      = errors.New("referral: not found")
	ErrReferralNotOpen       = errors.New("referral: not open")
	ErrReferralSelf          = errors.New("referral: self referral")
	ErrReferralDisabled      = errors.New("referral: disabled")
	ErrReferralAlreadyBound  = errors.New("referral: already bound")
)

type ReferralRepo struct{ db DBTX }

func NewReferralRepo(db DBTX) *ReferralRepo { return &ReferralRepo{db: db} }

func (r *ReferralRepo) WithTx(tx pgx.Tx) *ReferralRepo { return &ReferralRepo{db: tx} }

func scanReferral(row pgx.Row) (*domain.ReferralAdmission, error) {
	var ref domain.ReferralAdmission
	var referrerID *uuid.UUID
	var referredID *uuid.UUID
	var dest, setBy *string
	var destAt, boundAt, rewardedAt *time.Time
	var firstDue, rewardedPay *uuid.UUID
	var code *string
	var extPhone, extName *string
	err := row.Scan(
		&ref.ID, &ref.PropertyID, &referrerID, &referredID, &ref.Phone, &ref.Name, &ref.Status,
		&ref.PointsAwarded, &ref.CreatedAt, &rewardedAt,
		&code, &ref.JoinerDiscountBps, &ref.ReferrerRewardBps, &dest, &setBy, &destAt,
		&extPhone, &extName,
		&boundAt, &firstDue, &rewardedPay,
	)
	if err != nil {
		return nil, err
	}
	if extPhone != nil {
		ref.ReferrerExternalPhone = *extPhone
	}
	if extName != nil {
		ref.ReferrerExternalName = *extName
	}
	ref.ReferrerTenantID = referrerID
	ref.ReferredTenantID = referredID
	if code != nil {
		ref.ReferralCode = *code
	}
	if dest != nil {
		d := domain.ReferralRewardDestination(*dest)
		ref.RewardDestination = &d
	}
	if setBy != nil {
		s := domain.ReferralDestinationSetBy(*setBy)
		ref.DestinationSetBy = &s
	}
	ref.DestinationSetAt = destAt
	ref.BoundAt = boundAt
	ref.FirstRentDueID = firstDue
	ref.RewardedPaymentID = rewardedPay
	ref.RewardedAt = rewardedAt
	return &ref, nil
}

const referralCols = `id, property_id, referrer_tenant_id, referred_tenant_id, phone, name, status,
	points_awarded, created_at, rewarded_at,
	referral_code, joiner_discount_bps, referrer_reward_bps, reward_destination, destination_set_by, destination_set_at,
	referrer_external_phone, referrer_external_name,
	bound_at, first_rent_due_id, rewarded_payment_id`

func (r *ReferralRepo) GetByCode(ctx context.Context, code string) (*domain.ReferralAdmission, error) {
	return scanReferral(r.db.QueryRow(ctx, `SELECT `+referralCols+` FROM referrals WHERE referral_code=$1`, code))
}

func (r *ReferralRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.ReferralAdmission, error) {
	return scanReferral(r.db.QueryRow(ctx, `SELECT `+referralCols+` FROM referrals WHERE id=$1`, id))
}

func (r *ReferralRepo) GetBoundForTenant(ctx context.Context, tenantID uuid.UUID) (*domain.ReferralAdmission, error) {
	return scanReferral(r.db.QueryRow(ctx, `SELECT `+referralCols+` FROM referrals WHERE referred_tenant_id=$1 AND status IN ('bound','rewarded') ORDER BY bound_at DESC LIMIT 1`, tenantID))
}

func (r *ReferralRepo) PropertyReferralSettings(ctx context.Context, propertyID uuid.UUID) (enabled bool, joinerBps, referrerBps int, err error) {
	err = r.db.QueryRow(ctx, `
		SELECT referral_enabled, joiner_discount_bps, referrer_reward_bps
		FROM property_onboarding_settings WHERE property_id=$1`, propertyID,
	).Scan(&enabled, &joinerBps, &referrerBps)
	return enabled, joinerBps, referrerBps, err
}

type CreateReferralInput struct {
	PropertyID            uuid.UUID
	ReferrerTenantID      *uuid.UUID
	ReferrerExternalPhone string
	ReferrerExternalName  string
	Phone                 string
	Name                  string
	Code                  string
	JoinerDiscountBps     int
	ReferrerRewardBps     int
	Destination           domain.ReferralRewardDestination
	SetBy                 domain.ReferralDestinationSetBy
}

func (r *ReferralRepo) CreatePending(ctx context.Context, in CreateReferralInput) (*domain.ReferralAdmission, error) {
	dest := string(in.Destination)
	setBy := string(in.SetBy)
	now := time.Now().UTC()
	return scanReferral(r.db.QueryRow(ctx, `
		INSERT INTO referrals (
			property_id, referrer_tenant_id, phone, name, status, created_at,
			referral_code, joiner_discount_bps, referrer_reward_bps,
			reward_destination, destination_set_by, destination_set_at,
			referrer_external_phone, referrer_external_name
		) VALUES ($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+referralCols,
		in.PropertyID, in.ReferrerTenantID, in.Phone, in.Name, now,
		in.Code, in.JoinerDiscountBps, in.ReferrerRewardBps,
		dest, setBy, now,
		nullIfEmptyStr(in.ReferrerExternalPhone), in.ReferrerExternalName,
	))
}

func (r *ReferralRepo) BindToTenant(ctx context.Context, referralID, propertyID, tenantID uuid.UUID, joinerPhone string) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE referrals SET
			referred_tenant_id=$3, status='bound', bound_at=NOW(),
			reward_destination=COALESCE(reward_destination, 'referrer_credit')
		WHERE id=$1 AND property_id=$2 AND status='pending' AND phone=$4`,
		referralID, propertyID, tenantID, joinerPhone)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrReferralNotOpen
	}
	return nil
}

func (r *ReferralRepo) CountPaidRentDues(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM dues WHERE tenant_id=$1 AND kind='rent' AND status='paid'`, tenantID).Scan(&n)
	return n, err
}

func (r *ReferralRepo) InsertDueAdjustment(ctx context.Context, dueID, referralID uuid.UUID, joinerDisc, passPaise, rentBase int64) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO due_referral_adjustments (due_id, referral_id, joiner_discount_paise, referrer_pass_paise, rent_base_paise)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (due_id) DO NOTHING`,
		dueID, referralID, joinerDisc, passPaise, rentBase)
	return err
}

func (r *ReferralRepo) SetFirstRentDue(ctx context.Context, referralID, dueID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE referrals SET first_rent_due_id=$2 WHERE id=$1`, referralID, dueID)
	return err
}

func (r *ReferralRepo) MarkRewarded(ctx context.Context, referralID uuid.UUID, paymentID *uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE referrals SET status='rewarded', rewarded_at=NOW(), rewarded_payment_id=$2
		WHERE id=$1 AND status='bound' AND rewarded_at IS NULL`, referralID, paymentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("referral: reward idempotent skip")
	}
	return nil
}

func (r *ReferralRepo) InsertCreditEntry(ctx context.Context, propertyID uuid.UUID, tenantID *uuid.UUID, externalPhone *string, referralID uuid.UUID, entryType string, amountPaise int64, idemKey string, dueID, paymentID *uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO referral_credit_entries (
			property_id, tenant_id, external_phone, referral_id, entry_type, amount_paise, idempotency_key, due_id, payment_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		propertyID, tenantID, externalPhone, referralID, entryType, amountPaise, idemKey, dueID, paymentID)
	return err
}

func (r *ReferralRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.ReferralAdmission, error) {
	rows, err := r.db.Query(ctx, `SELECT `+referralCols+` FROM referrals WHERE property_id=$1 ORDER BY created_at DESC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ReferralAdmission
	for rows.Next() {
		ref, err := scanReferral(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ref)
	}
	return out, rows.Err()
}

func (r *ReferralRepo) GetAdjustmentByDueID(ctx context.Context, dueID uuid.UUID) (referralID uuid.UUID, joinerDisc, passPaise, rentBase int64, err error) {
	err = r.db.QueryRow(ctx, `
		SELECT referral_id, joiner_discount_paise, referrer_pass_paise, rent_base_paise
		FROM due_referral_adjustments WHERE due_id=$1`, dueID,
	).Scan(&referralID, &joinerDisc, &passPaise, &rentBase)
	return referralID, joinerDisc, passPaise, rentBase, err
}

func (r *ReferralRepo) AddTenantCreditBalance(ctx context.Context, tenantID uuid.UUID, amountPaise int64) error {
	_, err := r.db.Exec(ctx, `
		UPDATE tenants SET credit_balance_paise = credit_balance_paise + $2 WHERE id=$1`, tenantID, amountPaise)
	return err
}

func (r *ReferralRepo) SubtractTenantCreditBalance(ctx context.Context, tenantID uuid.UUID, amountPaise int64) error {
	_, err := r.db.Exec(ctx, `
		UPDATE tenants SET credit_balance_paise = GREATEST(0, credit_balance_paise - $2) WHERE id=$1`, tenantID, amountPaise)
	return err
}

func (r *ReferralRepo) GetEarnForReferral(ctx context.Context, referralID uuid.UUID) (amount int64, tenantID *uuid.UUID, err error) {
	err = r.db.QueryRow(ctx, `
		SELECT amount_paise, tenant_id FROM referral_credit_entries
		WHERE referral_id=$1 AND entry_type='earn' ORDER BY created_at DESC LIMIT 1`, referralID,
	).Scan(&amount, &tenantID)
	return amount, tenantID, err
}

func (r *ReferralRepo) ResetRewarded(ctx context.Context, referralID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE referrals SET status='bound', rewarded_at=NULL, rewarded_payment_id=NULL
		WHERE id=$1 AND status='rewarded'`, referralID)
	return err
}

func (r *ReferralRepo) ListByReferrerTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.ReferralAdmission, error) {
	rows, err := r.db.Query(ctx, `SELECT `+referralCols+` FROM referrals WHERE referrer_tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ReferralAdmission
	for rows.Next() {
		ref, err := scanReferral(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ref)
	}
	return out, rows.Err()
}
