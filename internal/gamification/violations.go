package gamification

import (
	"context"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// LogViolation records a violation, automatically computing the 90-day anchored step.
func (s *Service) LogViolation(ctx context.Context, tenantID uuid.UUID, ruleCode, severity, description string, evidence []byte, createdBy uuid.UUID) (*domain.Violation, error) {
	tenant, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return nil, ErrTenantNotFound
	}

	// P2 Fix: Count violations in 90-day anchored window
	recentCount, err := s.store.CountRecentViolations(ctx, tenantID, 90)
	if err != nil {
		return nil, err
	}

	step := int16(recentCount + 1)
	if step > 3 {
		step = 3 // capped at step 3 formal warning ladder
	}

	v := &domain.Violation{
		TenantID:      tenantID,
		PropertyID:    tenant.PropertyID,
		RuleCode:      ruleCode,
		Severity:      severity,
		Step:          step,
		Description:   description,
		EvidenceBytes: evidence,
		CreatedBy:     createdBy,
	}

	if err := s.store.CreateViolation(ctx, v); err != nil {
		return nil, err
	}

	// Action based on step:
	switch step {
	case 1:
		// Step 1: Private notice logged in app. No points deducted.
		s.logger.Info("step-1 violation notice logged", "tenant_id", tenantID, "rule", ruleCode)

	case 2:
		// Step 2: 50 points deduction and streak resets
		refType := "violation"
		refID := v.ID.String()
		_ = s.DeductPoints(ctx, tenantID, "VIOLATION_PENALTY", 50, &refType, &refID, &createdBy)

		// Reset on-time streak
		streak, _ := s.store.GetStreak(ctx, tenantID)
		if streak != nil {
			streak.OnTimeMonths = 0
			_ = s.store.UpsertStreak(ctx, streak)
		}
		s.logger.Warn("step-2 violation applied: -50 points and streak reset", "tenant_id", tenantID)

	case 3:
		// Step 3: Formal warning. Ineligibility for rent credit that quarter is checked in RedeemReward.
		refType := "violation"
		refID := v.ID.String()
		_ = s.DeductPoints(ctx, tenantID, "VIOLATION_PENALTY", 50, &refType, &refID, &createdBy)

		streak, _ := s.store.GetStreak(ctx, tenantID)
		if streak != nil {
			streak.OnTimeMonths = 0
			_ = s.store.UpsertStreak(ctx, streak)
		}
		s.logger.Warn("step-3 formal warning issued: cash credit redemption suspended for quarter", "tenant_id", tenantID)
	}

	_ = s.pub.Publish(ctx, domain.Event{
		TenantID:   &tenantID,
		PropertyID: tenant.PropertyID,
		EventType:  domain.EvtViolationIssued,
		OccurredAt: s.now().UTC(),
	})

	return v, nil
}

// ReportHazard records a private hazard report.
func (s *Service) ReportHazard(ctx context.Context, tenantID uuid.UUID, category, description string, photo []byte) (*domain.HazardReport, error) {
	tenant, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return nil, ErrTenantNotFound
	}

	if len(photo) > 0 {
		if _, err := s.blobs.ValidateImage(photo); err != nil {
			return nil, err
		}
	}

	h := &domain.HazardReport{
		PropertyID:         tenant.PropertyID,
		ReportedByTenantID: tenantID,
		Category:           category,
		Description:        description,
		PhotoBytes:         photo,
	}

	if err := s.store.CreateHazard(ctx, h); err != nil {
		return nil, err
	}

	_ = s.pub.Publish(ctx, domain.Event{
		PropertyID: tenant.PropertyID,
		EventType:  domain.EvtHazardReported,
		OccurredAt: s.now().UTC(),
	})

	return h, nil
}

// ResolveHazard marks a hazard resolved and awards HAZARD_REPORT (25 points) to the anonymous reporter.
func (s *Service) ResolveHazard(ctx context.Context, hazardID uuid.UUID, resolvedBy uuid.UUID, status string) error {
	h, err := s.store.GetHazardByID(ctx, hazardID)
	if err != nil {
		return err
	}

	if err := s.store.ResolveHazard(ctx, hazardID, resolvedBy, status); err != nil {
		return err
	}

	// If resolved and points not already awarded, award 25 points to reporter
	if status == "resolved" && !h.PointsAwarded {
		refType := "hazard"
		refID := h.ID.String()
		_, _ = s.AwardPoints(ctx, h.ReportedByTenantID, "HAZARD_REPORT", &refType, &refID, &resolvedBy)
	}

	_ = s.pub.Publish(ctx, domain.Event{
		PropertyID: h.PropertyID,
		EventType:  domain.EvtHazardResolved,
		OccurredAt: s.now().UTC(),
	})

	return nil
}
