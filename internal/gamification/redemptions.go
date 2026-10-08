package gamification

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// RedeemReward executes a reward redemption with row locks, tenure checks, and step-3 violation checks.
func (s *Service) RedeemReward(ctx context.Context, tenantID uuid.UUID, rewardID uuid.UUID) (*domain.Redemption, error) {
	tenant, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return nil, ErrTenantNotFound
	}

	reward, err := s.store.GetRewardByID(ctx, rewardID)
	if err != nil {
		return nil, err
	}
	if !reward.IsActive {
		return nil, errors.New("reward is no longer active")
	}

	// Begin atomic transaction
	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	if tx != nil {
		defer func() { _ = tx.Rollback(ctx) }()
	}

	// Universal Lock Hierarchy Step 1: Lock tenant first
	if err := s.store.LockTenantTx(ctx, tx, tenantID); err != nil {
		return nil, fmt.Errorf("lock tenant for redemption: %w", err)
	}

	// Universal Lock Hierarchy Step 2: Lock tenant_streaks row for update to prevent concurrent double-spends
	streak, err := s.store.GetStreakForUpdate(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}

	// 1. Verify active unexpired points balance inside lock
	activeBalance, err := s.store.GetActiveBalanceTx(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	if activeBalance < reward.PointsCost {
		return nil, fmt.Errorf("%w: required %d, available %d", ErrInsufficientPoints, reward.PointsCost, activeBalance)
	}

	// 2. P1 Fix: Step-3 violation quarterly block on cash rent credit
	if reward.Category == "cash_credit" {
		step3Count, err := s.store.CountStep3ViolationsInQuarter(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		if step3Count > 0 {
			return nil, ErrStep3ViolationBlocked
		}

		// 3. Check tenure requirement (e.g. 3 consecutive on-time months)
		if streak.OnTimeMonths < reward.MinTenureMonths {
			return nil, fmt.Errorf("%w: requires %d consecutive on-time months, current streak is %d", ErrTenureRequirement, reward.MinTenureMonths, streak.OnTimeMonths)
		}
	}

	red := &domain.Redemption{
		TenantID:    tenantID,
		PropertyID:  tenant.PropertyID,
		RewardID:    reward.ID,
		PointsSpent: reward.PointsCost,
		Status:      "completed",
	}
	if reward.Category == "food_coupon" || reward.Category == "perk" {
		red.Status = "pending"
	}

	var cashDiscountPaise int64
	if reward.Category == "cash_credit" {
		// Fail closed: if the point value cannot be read, no cash credit is issued.
		settings, err := s.store.GetSettings(ctx, tenant.PropertyID)
		if err != nil {
			return nil, fmt.Errorf("load gamification settings for cash redemption: %w", err)
		}
		if settings == nil {
			return nil, ErrInvalidRewardValue
		}
		cashDiscountPaise, err = cashCreditValuePaise(reward.Metadata, reward.PointsCost, settings.PointValuePaise)
		if err != nil {
			return nil, err
		}
	}

	// Handle reward actions based on category
	switch reward.Category {
	case "cash_credit":
		// Apply credit to tenant.credit_balance_paise inside transaction (C-11)
		tenant.CreditBalancePaise += cashDiscountPaise
		if err := s.store.AddTenantCreditTx(ctx, tx, tenantID, cashDiscountPaise); err != nil {
			return nil, fmt.Errorf("failed to apply rent credit: %w", err)
		}

	case "food_coupon":
		code, err := randomCouponCode()
		if err != nil {
			return nil, err
		}
		red.CouponCode = &code

	case "perk":
		// Guest pass, late fee waiver, 48h deposit fast track, rent freeze:
		// Metadata records the redemption status
		red.Metadata = reward.Metadata
	}

	// Create redemption record in DB
	if err := s.store.CreateRedemptionTx(ctx, tx, red); err != nil {
		return nil, err
	}

	// Deduct points via ledger: negative delta, expires_at = nil (P0 invariant)
	refType := "redemption"
	refID := red.ID.String()
	ledgerEntry := &domain.PointsLedgerEntry{
		TenantID:   tenantID,
		PropertyID: tenant.PropertyID,
		RuleCode:   "REDEEM_" + reward.Code,
		Delta:      -reward.PointsCost,
		RefType:    &refType,
		RefID:      &refID,
		ExpiresAt:  nil, // Permanent deduction
	}
	if err := s.store.InsertLedgerEntryTx(ctx, tx, ledgerEntry); err != nil {
		return nil, err
	}

	// Update cached balance
	streak.CachedBalance = activeBalance - reward.PointsCost
	if err := s.store.UpsertStreakTx(ctx, tx, streak); err != nil {
		return nil, err
	}

	// In-transaction universal reward redeem ledger outbox enqueue (C-11)
	if reward.Category == "cash_credit" {
		outboxPayload, err := json.Marshal(domain.RewardRedeemMirrorPayload{
			PropertyID:   tenant.PropertyID,
			TenantID:     tenant.ID,
			RedemptionID: red.ID,
			PointsSpent:  red.PointsSpent,
			AmountPaise:  cashDiscountPaise,
			OccurredAt:   s.now().UTC(),
		})
		if err != nil {
			return nil, fmt.Errorf("marshal reward redeem outbox payload: %w", err)
		}
		outboxEvt := &domain.LedgerOutboxEvent{
			EventType:      domain.LedgerOutboxRewardRedeem,
			PropertyID:     tenant.PropertyID,
			SourceID:       red.ID,
			Payload:        outboxPayload,
			IdempotencyKey: fmt.Sprintf("reward_redeem:%s", red.ID),
		}
		if err := s.store.InsertOutboxEventTx(ctx, tx, outboxEvt); err != nil {
			return nil, fmt.Errorf("enqueue reward redeem outbox: %w", err)
		}
	}

	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
	}

	// Publish audit event
	_ = s.pub.Publish(ctx, domain.Event{
		TenantID:   &tenantID,
		PropertyID: tenant.PropertyID,
		EventType:  domain.EvtRewardRedeemed,
		OccurredAt: s.now().UTC(),
	})

	if s.onRedeem != nil && reward.Category == "cash_credit" {
		s.onRedeem(ctx, tenant, red, cashDiscountPaise)
	}

	return red, nil
}

func randomCouponCode() (string, error) {
	const chars = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ" // unambiguous chars
	out := make([]byte, 8)
	max := big.NewInt(int64(len(chars)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = chars[idx.Int64()]
	}
	return "FOOD-" + strings.ToUpper(string(out)), nil
}

// cashCreditValuePaise returns the rent credit for a cash reward.
// The credit never exceeds pointsCost x pointValuePaise (the value of the points spent).
// With no discount in the metadata, the credit equals that maximum.
// Bad metadata, a negative discount, or a non-positive cost or point value is an error.
func cashCreditValuePaise(metadata json.RawMessage, pointsCost int, pointValuePaise int64) (int64, error) {
	maxPaise := int64(pointsCost) * pointValuePaise
	if pointsCost <= 0 || pointValuePaise <= 0 || maxPaise <= 0 {
		return 0, ErrInvalidRewardValue
	}
	if len(metadata) == 0 {
		return maxPaise, nil
	}
	var m struct {
		DiscountPaise int64 `json:"discount_paise"`
	}
	if err := json.Unmarshal(metadata, &m); err != nil {
		return 0, fmt.Errorf("%w: unreadable metadata: %v", ErrInvalidRewardValue, err)
	}
	switch {
	case m.DiscountPaise < 0:
		return 0, fmt.Errorf("%w: negative discount", ErrInvalidRewardValue)
	case m.DiscountPaise == 0, m.DiscountPaise > maxPaise:
		return maxPaise, nil
	default:
		return m.DiscountPaise, nil
	}
}

// ListRedemptionsByProperty returns redemptions for a property filtered optionally by status.
func (s *Service) ListRedemptionsByProperty(ctx context.Context, propertyID uuid.UUID, status string) ([]domain.Redemption, error) {
	return s.store.ListRedemptionsByProperty(ctx, propertyID, status)
}

// FulfilRedemption transitions a pending coupon or perk redemption to fulfilled.
func (s *Service) FulfilRedemption(ctx context.Context, propertyID uuid.UUID, redemptionID uuid.UUID) (*domain.Redemption, error) {
	red, err := s.store.GetRedemptionByID(ctx, propertyID, redemptionID)
	if err != nil {
		return nil, fmt.Errorf("redemption not found: %w", err)
	}
	if red.Status != "pending" {
		return nil, fmt.Errorf("cannot fulfil redemption with status: %s", red.Status)
	}
	if err := s.store.FulfilRedemption(ctx, propertyID, redemptionID); err != nil {
		return nil, err
	}
	red.Status = "fulfilled"
	red.UpdatedAt = s.now().UTC()
	return red, nil
}

