package postgres

import (
	"context"
	"errors"

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
			slot_invite_default_ttl_days, waitlist_enabled, profile_schema_version,
			require_kyc_before_pay, join_pending_ttl_days
		FROM property_onboarding_settings WHERE property_id=$1`, propertyID,
	).Scan(
		&s.PropertyID, &s.DefaultRentPaise, &s.DefaultDepositPaise, &s.DefaultDueDay,
		&s.AutoActivate, &s.ReserveBedsForOpenInvites, &s.MaxExtraBedsPerRoom,
		&s.SlotInviteDefaultTTLDays, &s.WaitlistEnabled, &s.ProfileSchemaVersion,
		&s.RequireKYCBeforePay, &s.JoinPendingTTLDays,
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
