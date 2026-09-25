package gamification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// EventConsumer handles billing events asynchronously without blocking payments.
type EventConsumer struct {
	svc    *Service
	logger *slog.Logger
}

func NewEventConsumer(svc *Service) *EventConsumer {
	return &EventConsumer{
		svc:    svc,
		logger: slog.Default(),
	}
}

// ProcessEventAsync handles event in a background goroutine with panic recovery.
func (c *EventConsumer) ProcessEventAsync(e domain.Event) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("gamification event consumer recovered panic", "panic", r, "event_type", e.EventType)
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := c.HandleEvent(ctx, e); err != nil {
			c.logger.Error("gamification failed to process event", "error", err, "event_type", e.EventType, "tenant_id", e.TenantID)
		}
	}()
}

// HandleEvent updates streaks, awards rent-on-time points, and awards referral bonuses.
func (c *EventConsumer) HandleEvent(ctx context.Context, e domain.Event) error {
	if e.TenantID == nil {
		return nil
	}
	tenantID := *e.TenantID

	// DPDP Section 9(3): Minor Protection & Gamification Opt-Out Gate
	tenant, err := c.svc.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return err
	}
	now := c.svc.now().UTC()
	if !tenant.GamificationActive(now) {
		c.logger.Info("gamification suppressed for minor / disabled tenant", "tenant_id", tenantID)
		return nil
	}

	// Replay Protection for streak events (guarantees single-shot execution per due)
	if e.DueID != nil {
		firstTime, err := c.svc.store.RecordStreakDueEvent(ctx, tenantID, *e.DueID)
		if err != nil {
			c.logger.Warn("could not record streak due event", "err", err, "tenant_id", tenantID, "due_id", *e.DueID)
		}
		if !firstTime {
			c.logger.Info("streak due event already processed, skipping streak mutation", "tenant_id", tenantID, "due_id", *e.DueID)
			return nil
		}
	}

	switch e.EventType {
	case domain.EvtDuePaidOnTime:
		var payload domain.DuePaidPayload
		_ = json.Unmarshal(e.Payload, &payload)

		// 1. Award on-time rent points (idempotent via ref_type and ref_id)
		refType := "due"
		refID := payload.DueID
		if refID == "" && e.DueID != nil {
			refID = e.DueID.String()
		}

		_, err := c.svc.AwardPoints(ctx, tenantID, "RENT_ON_TIME", &refType, &refID, nil)
		if err != nil && !errorsIsDuplicate(err) {
			c.logger.Warn("could not award on-time rent points", "err", err, "tenant_id", tenantID)
		}

		// 2. Increment on-time streak
		streak, err := c.svc.store.GetStreak(ctx, tenantID)
		if err != nil || streak == nil {
			streak = &domain.TenantStreak{
				TenantID:         tenantID,
				PropertyID:       e.PropertyID,
				FreezesAvailable: 1,
			}
		}
		streak.OnTimeMonths++
		if e.DueID != nil {
			streak.LastOnTimeDueID = e.DueID
		}
		_ = c.svc.store.UpsertStreak(ctx, streak)

		// 3. Milestone awards (3, 6, 12 months) with anti-farming protection
		if streak.OnTimeMonths == 3 || streak.OnTimeMonths == 6 || streak.OnTimeMonths == 12 {
			awarded, err := c.svc.store.RecordMilestoneAward(ctx, tenantID, streak.OnTimeMonths)
			if err == nil && awarded {
				milestoneRuleCode := fmt.Sprintf("MILESTONE_%d_MONTHS", streak.OnTimeMonths)
				mRefType := "milestone"
				mRefID := fmt.Sprintf("%s:%d", tenantID.String(), streak.OnTimeMonths)
				_, _ = c.svc.AwardPoints(ctx, tenantID, milestoneRuleCode, &mRefType, &mRefID, nil)
				c.logger.Info("awarded streak milestone points", "tenant_id", tenantID, "months", streak.OnTimeMonths)
			}
		}

		// 4. Check if this is the first rent payment for a referred tenant
		c.checkReferralReward(ctx, tenantID, e.PropertyID)

	case domain.EvtDuePaidLate:
		// Check freeze availability (1 freeze per 6 months)
		streak, err := c.svc.store.GetStreak(ctx, tenantID)
		if err == nil && streak != nil {
			canUseFreeze := streak.FreezesAvailable > 0 &&
				(streak.LastFreezeUsedAt == nil || now.Sub(*streak.LastFreezeUsedAt) > 180*24*time.Hour)

			if canUseFreeze {
				streak.FreezesAvailable--
				streak.LastFreezeUsedAt = &now
				c.logger.Info("tenant used streak freeze for late rent", "tenant_id", tenantID, "streak", streak.OnTimeMonths)
			} else {
				// Soft-landing: instead of resetting an 8-month streak directly to 0,
				// drop by 1 month (8 -> 7) to preserve tenant engagement.
				if streak.OnTimeMonths > 1 {
					streak.OnTimeMonths--
					c.logger.Info("tenant streak soft-landed due to late rent", "tenant_id", tenantID, "new_streak", streak.OnTimeMonths)
				} else {
					streak.OnTimeMonths = 0
					c.logger.Info("tenant streak reset to 0 due to late rent", "tenant_id", tenantID)
				}
			}
			_ = c.svc.store.UpsertStreak(ctx, streak)
		}
	}

	return nil
}

func (c *EventConsumer) checkReferralReward(ctx context.Context, referredTenantID, propertyID uuid.UUID) {
	tenant, err := c.svc.tenants.GetByID(ctx, referredTenantID)
	if err != nil || tenant.Phone == nil || *tenant.Phone == "" {
		return
	}

	ref, err := c.svc.store.GetReferralByPhone(ctx, propertyID, *tenant.Phone)
	if err != nil || ref == nil || ref.Status != "pending" {
		return
	}

	// Award REFERRAL_JOIN (500 points) to referrer
	refType := "referral"
	refID := ref.ID.String()
	awarded, err := c.svc.AwardPoints(ctx, ref.ReferrerTenantID, "REFERRAL_JOIN", &refType, &refID, nil)
	if err == nil {
		_ = c.svc.store.MarkReferralRewarded(ctx, ref.ID, referredTenantID, awarded)
		_ = c.svc.pub.Publish(ctx, domain.Event{
			TenantID:   &ref.ReferrerTenantID,
			PropertyID: propertyID,
			EventType:  domain.EvtReferralRewarded,
			OccurredAt: c.svc.now().UTC(),
		})
	}
}

func errorsIsDuplicate(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint")
}
