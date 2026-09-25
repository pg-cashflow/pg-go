package gamification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
)

var (
	ErrTenantNotFound         = errors.New("tenant not found")
	ErrRuleNotFound           = errors.New("point rule not found or inactive")
	ErrMonthlyCapExceeded     = errors.New("monthly earn cap exceeded")
	ErrPropertyBudgetExceeded = errors.New("property monthly reward budget exceeded")
	ErrInsufficientPoints     = errors.New("insufficient points balance")
	ErrTenureRequirement     = errors.New("tenure requirement not met for this reward")
	ErrStep3ViolationBlocked  = errors.New("cash rent credit redemption suspended for this quarter due to a step-3 house rule violation")
	ErrInvalidDelta           = errors.New("invalid point delta")
	ErrMeterReadingDecreased  = errors.New("new meter reading cannot be lower than previous reading")
	ErrMeterReadingCeiling    = errors.New("meter reading delta exceeds maximum safety ceiling")
	ErrDisputeWindowExpired   = errors.New("inspection dispute window (48 hours) has expired")
	ErrRSVPCutoffPassed       = errors.New("meal RSVP cutoff has passed for this slot (must confirm before 8:00 PM the previous day)")
	ErrPhotoRequiredOnFail    = errors.New("photo evidence is mandatory for any failed inspection item")
)

// Store defines repository requirements for the gamification engine.
type Store interface {
	BeginTx(ctx context.Context) (pgx.Tx, error)
	LockTenantTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error

	// Floors & Rooms
	CreateFloor(ctx context.Context, f *domain.Floor) error
	ListFloors(ctx context.Context, propertyID uuid.UUID) ([]domain.Floor, error)
	CreateRoom(ctx context.Context, rm *domain.Room) error
	ListRooms(ctx context.Context, propertyID uuid.UUID) ([]domain.Room, error)
	GetRoomByID(ctx context.Context, id uuid.UUID) (*domain.Room, error)

	// Settings & Rules
	GetSettings(ctx context.Context, propertyID uuid.UUID) (*domain.PropertyGamificationSettings, error)
	UpdateSettings(ctx context.Context, s *domain.PropertyGamificationSettings) error
	ListPointRules(ctx context.Context, propertyID uuid.UUID) ([]domain.PointRule, error)
	GetPointRuleByCode(ctx context.Context, propertyID uuid.UUID, code string) (*domain.PointRule, error)

	// Points Ledger & Streaks
	GetActiveBalance(ctx context.Context, tenantID uuid.UUID) (int, error)
	GetActiveBalanceTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (int, error)
	GetExpiringSoon(ctx context.Context, tenantID uuid.UUID, withinDays int) (int, time.Time, error)
	GetTenantMonthPoints(ctx context.Context, tenantID uuid.UUID, monthYear string, isRSVP bool) (int, error)
	GetPropertyMonthPoints(ctx context.Context, propertyID uuid.UUID, monthYear string) (int, error)
	GetRuleMonthPoints(ctx context.Context, tenantID uuid.UUID, ruleCode string, monthYear string) (int, error)
	InsertLedgerEntry(ctx context.Context, entry *domain.PointsLedgerEntry) error
	InsertLedgerEntryTx(ctx context.Context, tx pgx.Tx, entry *domain.PointsLedgerEntry) error
	ListLedgerByTenant(ctx context.Context, tenantID uuid.UUID, limit int) ([]domain.PointsLedgerEntry, error)
	GetStreak(ctx context.Context, tenantID uuid.UUID) (*domain.TenantStreak, error)
	GetStreakForUpdate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (*domain.TenantStreak, error)
	UpsertStreak(ctx context.Context, s *domain.TenantStreak) error
	UpsertStreakTx(ctx context.Context, tx pgx.Tx, s *domain.TenantStreak) error
	RecordStreakDueEvent(ctx context.Context, tenantID, dueID uuid.UUID) (bool, error)
	RecordMilestoneAward(ctx context.Context, tenantID uuid.UUID, months int) (bool, error)

	// Rewards & Redemptions
	ListRewardsCatalog(ctx context.Context, propertyID uuid.UUID) ([]domain.RewardsCatalogItem, error)
	GetRewardByID(ctx context.Context, id uuid.UUID) (*domain.RewardsCatalogItem, error)
	CountStep3ViolationsInQuarter(ctx context.Context, tenantID uuid.UUID) (int, error)
	CreateRedemptionTx(ctx context.Context, tx pgx.Tx, red *domain.Redemption) error
	ListRedemptionsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Redemption, error)

	// Inspections
	CreateInspection(ctx context.Context, insp *domain.Inspection) error
	ListInspections(ctx context.Context, propertyID uuid.UUID, roomID *uuid.UUID, floorID *uuid.UUID, limit int) ([]domain.Inspection, error)
	GetInspectionByID(ctx context.Context, id uuid.UUID) (*domain.Inspection, error)
	DisputeInspectionItem(ctx context.Context, itemID uuid.UUID, disputeNote string) error
	ResolveInspectionItem(ctx context.Context, itemID uuid.UUID, resolvedBy uuid.UUID, status string) error
	GetFloorCleanlinessAverage(ctx context.Context, floorID uuid.UUID, monthYear string) (float64, error)
	CreateVendorInspection(ctx context.Context, vi *domain.VendorInspection) error
	ListVendorInspections(ctx context.Context, propertyID uuid.UUID, limit int) ([]domain.VendorInspection, error)

	// Violations & Hazards
	CountRecentViolations(ctx context.Context, tenantID uuid.UUID, windowDays int) (int, error)
	CreateViolation(ctx context.Context, v *domain.Violation) error
	ListViolationsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Violation, error)
	ListViolationsByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.Violation, error)
	CreateHazard(ctx context.Context, h *domain.HazardReport) error
	ListHazards(ctx context.Context, propertyID uuid.UUID, status string) ([]domain.HazardReport, error)
	GetHazardByID(ctx context.Context, id uuid.UUID) (*domain.HazardReport, error)
	ResolveHazard(ctx context.Context, id uuid.UUID, resolvedBy uuid.UUID, status string) error

	// Meter Readings
	GetLatestMeterReading(ctx context.Context, propertyID uuid.UUID, roomID *uuid.UUID, floorID *uuid.UUID, kind string) (*domain.MeterReading, error)
	CreateMeterReading(ctx context.Context, mr *domain.MeterReading) error
	ListMeterReadings(ctx context.Context, propertyID uuid.UUID, roomID *uuid.UUID, floorID *uuid.UUID, limit int) ([]domain.MeterReading, error)
	GetFloorWaterConsumption(ctx context.Context, floorID uuid.UUID, startDate, endDate time.Time) (float64, error)
	CountFloorActiveTenants(ctx context.Context, floorID uuid.UUID) (int, error)

	// Meal RSVPs & Menu Polls
	UpsertMealRSVP(ctx context.Context, rsvp *domain.MealRSVP) error
	ListMealRSVPsByDate(ctx context.Context, propertyID uuid.UUID, date time.Time) ([]domain.MealRSVP, error)
	GetTenantMealRSVP(ctx context.Context, tenantID uuid.UUID, date time.Time, slot string) (*domain.MealRSVP, error)
	GetActiveMenuPoll(ctx context.Context, propertyID uuid.UUID, monthYear string) (*domain.MenuPoll, error)
	CreateMenuPoll(ctx context.Context, poll *domain.MenuPoll) error
	VoteMenuPoll(ctx context.Context, vote *domain.MenuVote) error
	GetMenuPollVotes(ctx context.Context, pollID uuid.UUID) (map[string]int, error)

	// Referrals
	CreateReferral(ctx context.Context, ref *domain.Referral) error
	GetReferralByPhone(ctx context.Context, propertyID uuid.UUID, phone string) (*domain.Referral, error)
	MarkReferralRewarded(ctx context.Context, id uuid.UUID, referredTenantID uuid.UUID, points int) error
	ListReferralsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Referral, error)

	// Leaderboards
	GetTopStreaks(ctx context.Context, propertyID uuid.UUID, limit int) ([]domain.TenantStreak, error)
	GetFloorCleanScores(ctx context.Context, propertyID uuid.UUID, monthYear string) (map[uuid.UUID]float64, error)
}

type TenantReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	Update(ctx context.Context, t *domain.Tenant) error
}

type DueWriter interface {
	Create(ctx context.Context, d *domain.Due) error
}

type Service struct {
	store      Store
	tenants    TenantReader
	dues       DueWriter
	pub        events.Publisher
	blobs      BlobStore
	logger     *slog.Logger
	now        func() time.Time
	onPoints   func(ctx context.Context, tenant *domain.Tenant, entry *domain.PointsLedgerEntry, pointValuePaise int)
	onRedeem   func(ctx context.Context, tenant *domain.Tenant, red *domain.Redemption, amountPaise int64)
}

func NewService(store Store, tenants TenantReader, dues DueWriter, pub events.Publisher, blobs BlobStore) *Service {
	return &Service{
		store:   store,
		tenants: tenants,
		dues:    dues,
		pub:     pub,
		blobs:   blobs,
		logger:  slog.Default(),
		now:     time.Now,
	}
}

// AwardPoints evaluates caps, sets expiry (6 months), and logs to ledger.
func (s *Service) AwardPoints(ctx context.Context, tenantID uuid.UUID, ruleCode string, refType, refID *string, createdBy *uuid.UUID) (int, error) {
	tenant, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrTenantNotFound, err)
	}

	if !tenant.GamificationActive(s.now().UTC()) {
		return 0, nil // Gamification disabled under DPDP Act for minors or opted-out tenants
	}

	settings, err := s.store.GetSettings(ctx, tenant.PropertyID)
	if err != nil {
		return 0, err
	}

	rule, err := s.store.GetPointRuleByCode(ctx, tenant.PropertyID, ruleCode)
	if err != nil {
		return 0, fmt.Errorf("%w: code=%s", ErrRuleNotFound, ruleCode)
	}
	if !rule.Active {
		return 0, ErrRuleNotFound
	}

	monthYear := s.now().UTC().Format("2006-01")

	// 1. Check rule monthly cap if configured
	if rule.MonthlyCap > 0 {
		ruleEarned, err := s.store.GetRuleMonthPoints(ctx, tenantID, ruleCode, monthYear)
		if err != nil {
			return 0, err
		}
		if ruleEarned+rule.Points > rule.MonthlyCap {
			return 0, fmt.Errorf("%w: rule %s reached limit %d", ErrMonthlyCapExceeded, ruleCode, rule.MonthlyCap)
		}
	}

	// 2. Check tenant monthly earn cap:
	// P1: Meal RSVP has an isolated sub-cap (settings.RSVPSubCap) and does NOT crowd out general cap
	if rule.IsRSVP {
		rsvpEarned, err := s.store.GetTenantMonthPoints(ctx, tenantID, monthYear, true)
		if err != nil {
			return 0, err
		}
		if rsvpEarned+rule.Points > settings.RSVPSubCap {
			return 0, fmt.Errorf("%w: RSVP sub-cap %d points reached for month", ErrMonthlyCapExceeded, settings.RSVPSubCap)
		}
	} else {
		generalEarned, err := s.store.GetTenantMonthPoints(ctx, tenantID, monthYear, false)
		if err != nil {
			return 0, err
		}
		if generalEarned+rule.Points > settings.EarnCapPerTenant {
			return 0, fmt.Errorf("%w: monthly tenant cap of %d points reached", ErrMonthlyCapExceeded, settings.EarnCapPerTenant)
		}
	}

	// 3. Check property monthly budget cap
	propEarned, err := s.store.GetPropertyMonthPoints(ctx, tenant.PropertyID, monthYear)
	if err != nil {
		return 0, err
	}
	budgetPoints := settings.MonthlyBudgetPaise / settings.PointValuePaise
	if propEarned+rule.Points > budgetPoints {
		return 0, fmt.Errorf("%w: property monthly cap %d points reached", ErrPropertyBudgetExceeded, budgetPoints)
	}

	// 4. Calculate expiry (positive deltas expire in expiry_days, e.g. 180 days)
	expiresAt := s.now().UTC().AddDate(0, 0, settings.ExpiryDays)

	entry := &domain.PointsLedgerEntry{
		TenantID:   tenantID,
		PropertyID: tenant.PropertyID,
		RuleCode:   ruleCode,
		Delta:      rule.Points,
		RefType:    refType,
		RefID:      refID,
		ExpiresAt:  &expiresAt,
		CreatedBy:  createdBy,
	}

	if err := s.store.InsertLedgerEntry(ctx, entry); err != nil {
		return 0, err
	}

	if s.onPoints != nil && entry.Delta > 0 {
		s.onPoints(ctx, tenant, entry, settings.PointValuePaise)
	}

	// Update cached balance in tenant_streaks
	streak, _ := s.store.GetStreak(ctx, tenantID)
	if streak != nil {
		streak.CachedBalance += rule.Points
		_ = s.store.UpsertStreak(ctx, streak)
	}

	// Publish audit event
	_ = s.pub.Publish(ctx, domain.Event{
		TenantID:   &tenantID,
		PropertyID: tenant.PropertyID,
		EventType:  domain.EvtPointsAwarded,
		OccurredAt: s.now().UTC(),
	})

	return rule.Points, nil
}

func (s *Service) SetFinanceHooks(
	onPoints func(ctx context.Context, tenant *domain.Tenant, entry *domain.PointsLedgerEntry, pointValuePaise int),
	onRedeem func(ctx context.Context, tenant *domain.Tenant, red *domain.Redemption, amountPaise int64),
) {
	s.onPoints = onPoints
	s.onRedeem = onRedeem
}

// DeductPoints writes an unexpiring negative delta, guaranteeing points never drop below 0.
func (s *Service) DeductPoints(ctx context.Context, tenantID uuid.UUID, ruleCode string, delta int, refType, refID *string, createdBy *uuid.UUID) error {
	if delta <= 0 {
		return ErrInvalidDelta
	}

	tenant, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return ErrTenantNotFound
	}

	tx, err := s.store.BeginTx(ctx)
	if err != nil {
		return err
	}
	if tx != nil {
		defer func() { _ = tx.Rollback(ctx) }()
	}

	// Universal Lock Hierarchy Step 1: Lock tenant first
	if err := s.store.LockTenantTx(ctx, tx, tenantID); err != nil {
		return fmt.Errorf("lock tenant for point deduction: %w", err)
	}

	// Universal Lock Hierarchy Step 2: Lock streak row for atomic balance check
	streak, err := s.store.GetStreakForUpdate(ctx, tx, tenantID)
	if err != nil {
		// initialize if missing
		streak = &domain.TenantStreak{
			TenantID:         tenantID,
			PropertyID:       tenant.PropertyID,
			FreezesAvailable: 1,
		}
	}

	activeBalance, err := s.store.GetActiveBalanceTx(ctx, tx, tenantID)
	if err != nil {
		return err
	}

	// Floor deduction at current active balance (never negative)
	deductAmount := delta
	if deductAmount > activeBalance {
		deductAmount = activeBalance
	}
	if deductAmount <= 0 {
		return nil
	}

	// Negative deltas MUST have expires_at = nil
	entry := &domain.PointsLedgerEntry{
		TenantID:   tenantID,
		PropertyID: tenant.PropertyID,
		RuleCode:   ruleCode,
		Delta:      -deductAmount,
		RefType:    refType,
		RefID:      refID,
		ExpiresAt:  nil,
		CreatedBy:  createdBy,
	}

	if err := s.store.InsertLedgerEntryTx(ctx, tx, entry); err != nil {
		return err
	}

	streak.CachedBalance = activeBalance - deductAmount
	if err := s.store.UpsertStreakTx(ctx, tx, streak); err != nil {
		return err
	}

	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}

	_ = s.pub.Publish(ctx, domain.Event{
		TenantID:   &tenantID,
		PropertyID: tenant.PropertyID,
		EventType:  domain.EvtPointsDeducted,
		OccurredAt: s.now().UTC(),
	})

	return nil
}

// GetBalance returns the active unexpired balance and points expiring within 30 days.
func (s *Service) GetBalance(ctx context.Context, tenantID uuid.UUID) (int, int, time.Time, error) {
	balance, err := s.store.GetActiveBalance(ctx, tenantID)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	expiringSoon, earliest, err := s.store.GetExpiringSoon(ctx, tenantID, 30)
	if err != nil {
		return balance, 0, time.Time{}, err
	}
	return balance, expiringSoon, earliest, nil
}
