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
			slot_invite_default_ttl_days, waitlist_enabled, waitlist_offer_ttl_hours, profile_schema_version,
			require_kyc_before_pay, join_pending_ttl_days,
			referral_enabled, joiner_discount_bps, referrer_reward_bps
		FROM property_onboarding_settings WHERE property_id=$1`, propertyID,
	).Scan(
		&s.PropertyID, &s.DefaultRentPaise, &s.DefaultDepositPaise, &s.DefaultDueDay,
		&s.AutoActivate, &s.ReserveBedsForOpenInvites, &s.MaxExtraBedsPerRoom,
		&s.SlotInviteDefaultTTLDays, &s.WaitlistEnabled, &s.WaitlistOfferTTLHours, &s.ProfileSchemaVersion,
		&s.RequireKYCBeforePay, &s.JoinPendingTTLDays,
		&s.ReferralEnabled, &s.JoinerDiscountBps, &s.ReferrerRewardBps,
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
	if patch.DefaultRentPaise != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET default_rent_paise=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.DefaultRentPaise)
		if err != nil {
			return nil, err
		}
	}
	if patch.DefaultDepositPaise != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET default_deposit_paise=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.DefaultDepositPaise)
		if err != nil {
			return nil, err
		}
	}
	if patch.DefaultDueDay != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET default_due_day=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.DefaultDueDay)
		if err != nil {
			return nil, err
		}
	}
	if patch.AutoActivate != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET auto_activate=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.AutoActivate)
		if err != nil {
			return nil, err
		}
	}
	if patch.ReserveBedsForOpenInvites != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET reserve_beds_for_open_invites=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.ReserveBedsForOpenInvites)
		if err != nil {
			return nil, err
		}
	}
	if patch.MaxExtraBedsPerRoom != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET max_extra_beds_per_room=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.MaxExtraBedsPerRoom)
		if err != nil {
			return nil, err
		}
	}
	if patch.SlotInviteDefaultTTLDays != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET slot_invite_default_ttl_days=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.SlotInviteDefaultTTLDays)
		if err != nil {
			return nil, err
		}
	}
	if patch.WaitlistEnabled != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET waitlist_enabled=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.WaitlistEnabled)
		if err != nil {
			return nil, err
		}
	}
	if patch.WaitlistOfferTTLHours != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET waitlist_offer_ttl_hours=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.WaitlistOfferTTLHours)
		if err != nil {
			return nil, err
		}
	}
	if patch.RequireKYCBeforePay != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET require_kyc_before_pay=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.RequireKYCBeforePay)
		if err != nil {
			return nil, err
		}
	}
	if patch.JoinPendingTTLDays != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET join_pending_ttl_days=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.JoinPendingTTLDays)
		if err != nil {
			return nil, err
		}
	}
	if patch.ReferralEnabled != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET referral_enabled=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.ReferralEnabled)
		if err != nil {
			return nil, err
		}
	}
	if patch.JoinerDiscountBps != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET joiner_discount_bps=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.JoinerDiscountBps)
		if err != nil {
			return nil, err
		}
	}
	if patch.ReferrerRewardBps != nil {
		_, err = r.db.Exec(ctx, `UPDATE property_onboarding_settings SET referrer_reward_bps=$2, updated_at=NOW() WHERE property_id=$1`, propertyID, *patch.ReferrerRewardBps)
		if err != nil {
			return nil, err
		}
	}
	return r.Get(ctx, propertyID)
}
