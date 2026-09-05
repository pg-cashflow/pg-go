package gamification

import (
	"context"
	"encoding/json"
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

		// 3. Check if this is the first rent payment for a referred tenant
		c.checkReferralReward(ctx, tenantID, e.PropertyID)

	case domain.EvtDuePaidLate:
		// Check freeze availability (1 freeze per 6 months)
		streak, err := c.svc.store.GetStreak(ctx, tenantID)
		if err == nil && streak != nil {
			now := c.svc.now().UTC()
			canUseFreeze := streak.FreezesAvailable > 0 &&
				(streak.LastFreezeUsedAt == nil || now.Sub(*streak.LastFreezeUsedAt) > 180*24*time.Hour)

			if canUseFreeze {
				streak.FreezesAvailable--
				streak.LastFreezeUsedAt = &now
				c.logger.Info("tenant used streak freeze for late rent", "tenant_id", tenantID, "streak", streak.OnTimeMonths)
			} else {
				// Reset streak
				streak.OnTimeMonths = 0
				c.logger.Info("tenant streak reset due to late rent", "tenant_id", tenantID)
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
