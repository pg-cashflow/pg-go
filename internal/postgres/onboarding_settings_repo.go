package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type OnboardingSettingsRepo struct{ db DBTX }

func NewOnboardingSettingsRepo(db DBTX) *OnboardingSettingsRepo {
	return &OnboardingSettingsRepo{db: db}
}

func (r *OnboardingSettingsRepo) Get(ctx context.Context, propertyID uuid.UUID) (*domain.PropertyOnboardingSettings, error) {
	var s domain.PropertyOnboardingSettings
	err := r.db.QueryRow(ctx, `
		SELECT property_id, default_rent_paise, default_deposit_paise, default_due_day,
			auto_activate, reserve_beds_for_open_invites, max_extra_beds_per_room,
			slot_invite_default_ttl_days, waitlist_enabled, waitlist_offer_ttl_hours, profile_schema_version,
			require_kyc_before_pay, join_pending_ttl_days,
			referral_enabled, joiner_discount_bps, referrer_reward_bps,
			COALESCE(allow_manager_refunds, false), manager_refund_limit_paise
		FROM property_onboarding_settings WHERE property_id=$1`, propertyID,
	).Scan(
		&s.PropertyID, &s.DefaultRentPaise, &s.DefaultDepositPaise, &s.DefaultDueDay,
		&s.AutoActivate, &s.ReserveBedsForOpenInvites, &s.MaxExtraBedsPerRoom,
		&s.SlotInviteDefaultTTLDays, &s.WaitlistEnabled, &s.WaitlistOfferTTLHours, &s.ProfileSchemaVersion,
		&s.RequireKYCBeforePay, &s.JoinPendingTTLDays,
		&s.ReferralEnabled, &s.JoinerDiscountBps, &s.ReferrerRewardBps,
		&s.AllowManagerRefunds, &s.ManagerRefundLimitPaise,
	)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *OnboardingSettingsRepo) EnsureDefaults(ctx context.Context, propertyID uuid.UUID) (*domain.PropertyOnboardingSettings, error) {
	s, err := r.Get(ctx, propertyID)
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	_, err = r.db.Exec(ctx, `INSERT INTO property_onboarding_settings (property_id) VALUES ($1) ON CONFLICT DO NOTHING`, propertyID)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, propertyID)
}

func (r *OnboardingSettingsRepo) Update(ctx context.Context, propertyID uuid.UUID, patch OnboardingSettingsPatch) (*domain.PropertyOnboardingSettings, error) {
	if err := patch.Validate(); err != nil {
		return nil, err
	}
	_, err := r.EnsureDefaults(ctx, propertyID)
	if err != nil {
		return nil, err
	}

	setClauses := make([]string, 0, 16)
	args := []any{propertyID}

	add := func(col string, val any) {
		args = append(args, val)
		setClauses = append(setClauses, fmt.Sprintf("%s=$%d", col, len(args)))
	}

	if patch.DefaultRentPaise != nil {
		add("default_rent_paise", *patch.DefaultRentPaise)
	}
	if patch.DefaultDepositPaise != nil {
		add("default_deposit_paise", *patch.DefaultDepositPaise)
	}
	if patch.DefaultDueDay != nil {
		add("default_due_day", *patch.DefaultDueDay)
	}
	if patch.AutoActivate != nil {
		add("auto_activate", *patch.AutoActivate)
	}
	if patch.ReserveBedsForOpenInvites != nil {
		add("reserve_beds_for_open_invites", *patch.ReserveBedsForOpenInvites)
	}
	if patch.MaxExtraBedsPerRoom != nil {
		add("max_extra_beds_per_room", *patch.MaxExtraBedsPerRoom)
	}
	if patch.SlotInviteDefaultTTLDays != nil {
		add("slot_invite_default_ttl_days", *patch.SlotInviteDefaultTTLDays)
	}
	if patch.WaitlistEnabled != nil {
		add("waitlist_enabled", *patch.WaitlistEnabled)
	}
	if patch.WaitlistOfferTTLHours != nil {
		add("waitlist_offer_ttl_hours", *patch.WaitlistOfferTTLHours)
	}
	if patch.RequireKYCBeforePay != nil {
		add("require_kyc_before_pay", *patch.RequireKYCBeforePay)
	}
	if patch.JoinPendingTTLDays != nil {
		add("join_pending_ttl_days", *patch.JoinPendingTTLDays)
	}
	if patch.ReferralEnabled != nil {
		add("referral_enabled", *patch.ReferralEnabled)
	}
	if patch.JoinerDiscountBps != nil {
		add("joiner_discount_bps", *patch.JoinerDiscountBps)
	}
	if patch.ReferrerRewardBps != nil {
		add("referrer_reward_bps", *patch.ReferrerRewardBps)
	}
	if patch.AllowManagerRefunds != nil {
		add("allow_manager_refunds", *patch.AllowManagerRefunds)
	}
	if patch.ManagerRefundLimitPaise.Set {
		add("manager_refund_limit_paise", patch.ManagerRefundLimitPaise.Value)
	}

	if len(setClauses) > 0 {
		setClauses = append(setClauses, "updated_at=NOW()")
		query := fmt.Sprintf("UPDATE property_onboarding_settings SET %s WHERE property_id=$1", strings.Join(setClauses, ", "))
		if _, err := r.db.Exec(ctx, query, args...); err != nil {
			return nil, err
		}
	}
	return r.Get(ctx, propertyID)
}
