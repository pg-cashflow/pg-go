package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type GamificationRepo struct {
	pool *pgxpool.Pool
}

func NewGamificationRepo(pool *pgxpool.Pool) *GamificationRepo {
	return &GamificationRepo{pool: pool}
}

func (r *GamificationRepo) BeginTx(ctx context.Context) (pgx.Tx, error) {
	return r.pool.Begin(ctx)
}

// 1. Floors & Rooms
func (r *GamificationRepo) CreateFloor(ctx context.Context, f *domain.Floor) error {
	return r.pool.QueryRow(ctx, `
		INSERT INTO floors (property_id, floor_number, name)
		VALUES ($1, $2, $3)
		RETURNING id, created_at`,
		f.PropertyID, f.FloorNumber, f.Name,
	).Scan(&f.ID, &f.CreatedAt)
}

func (r *GamificationRepo) ListFloors(ctx context.Context, propertyID uuid.UUID) ([]domain.Floor, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, floor_number, name, created_at
		FROM floors WHERE property_id=$1 ORDER BY floor_number ASC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Floor
	for rows.Next() {
		var f domain.Floor
		if err := rows.Scan(&f.ID, &f.PropertyID, &f.FloorNumber, &f.Name, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) CreateRoom(ctx context.Context, rm *domain.Room) error {
	if rm.Capacity <= 0 {
		rm.Capacity = 2
	}
	if rm.IncludedUnits <= 0 {
		rm.IncludedUnits = 50
	}
	return r.pool.QueryRow(ctx, `
		INSERT INTO rooms (property_id, floor_id, room_number, capacity, included_units)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`,
		rm.PropertyID, rm.FloorId, rm.RoomNumber, rm.Capacity, rm.IncludedUnits,
	).Scan(&rm.ID, &rm.CreatedAt)
}

func (r *GamificationRepo) ListRooms(ctx context.Context, propertyID uuid.UUID) ([]domain.Room, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, floor_id, room_number, capacity, included_units, created_at
		FROM rooms WHERE property_id=$1 ORDER BY room_number ASC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Room
	for rows.Next() {
		var rm domain.Room
		if err := rows.Scan(&rm.ID, &rm.PropertyID, &rm.FloorId, &rm.RoomNumber, &rm.Capacity, &rm.IncludedUnits, &rm.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rm)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) GetRoomByID(ctx context.Context, id uuid.UUID) (*domain.Room, error) {
	var rm domain.Room
	err := r.pool.QueryRow(ctx, `
		SELECT id, property_id, floor_id, room_number, capacity, included_units, created_at
		FROM rooms WHERE id=$1`, id,
	).Scan(&rm.ID, &rm.PropertyID, &rm.FloorId, &rm.RoomNumber, &rm.Capacity, &rm.IncludedUnits, &rm.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &rm, nil
}

// 2. Property Settings & Rules
func (r *GamificationRepo) GetSettings(ctx context.Context, propertyID uuid.UUID) (*domain.PropertyGamificationSettings, error) {
	var s domain.PropertyGamificationSettings
	err := r.pool.QueryRow(ctx, `
		SELECT property_id, point_value_paise, monthly_budget_paise, earn_cap_per_tenant,
		       rsvp_sub_cap, expiry_days, floor_bonus_threshold, electricity_tariff_paise,
		       grace_days, late_penalty_points_per_day, late_penalty_max_points,
		       created_at, updated_at
		FROM property_gamification_settings WHERE property_id=$1`, propertyID,
	).Scan(
		&s.PropertyID, &s.PointValuePaise, &s.MonthlyBudgetPaise, &s.EarnCapPerTenant,
		&s.RSVPSubCap, &s.ExpiryDays, &s.FloorBonusThreshold, &s.ElectricityTariffPaise,
		&s.GraceDays, &s.LatePenaltyPointsPerDay, &s.LatePenaltyMaxPoints,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// return default
		return &domain.PropertyGamificationSettings{
			PropertyID:              propertyID,
			PointValuePaise:         100,
			MonthlyBudgetPaise:      1000000,
			EarnCapPerTenant:        200,
			RSVPSubCap:              60,
			ExpiryDays:              180,
			FloorBonusThreshold:     85,
			ElectricityTariffPaise:  1000,
			GraceDays:               2,
			LatePenaltyPointsPerDay: 2,
			LatePenaltyMaxPoints:    50,
			CreatedAt:               time.Now().UTC(),
			UpdatedAt:               time.Now().UTC(),
		}, nil
	}
	return &s, err
}

func (r *GamificationRepo) UpdateSettings(ctx context.Context, s *domain.PropertyGamificationSettings) error {
	s.UpdatedAt = time.Now().UTC()
	if s.GraceDays <= 0 {
		s.GraceDays = 2
	}
	if s.LatePenaltyPointsPerDay < 0 {
		s.LatePenaltyPointsPerDay = 2
	}
	if s.LatePenaltyMaxPoints <= 0 {
		s.LatePenaltyMaxPoints = 50
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO property_gamification_settings (
			property_id, point_value_paise, monthly_budget_paise, earn_cap_per_tenant,
			rsvp_sub_cap, expiry_days, floor_bonus_threshold, electricity_tariff_paise,
			grace_days, late_penalty_points_per_day, late_penalty_max_points,
			updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (property_id) DO UPDATE SET
			point_value_paise = EXCLUDED.point_value_paise,
			monthly_budget_paise = EXCLUDED.monthly_budget_paise,
			earn_cap_per_tenant = EXCLUDED.earn_cap_per_tenant,
			rsvp_sub_cap = EXCLUDED.rsvp_sub_cap,
			expiry_days = EXCLUDED.expiry_days,
			floor_bonus_threshold = EXCLUDED.floor_bonus_threshold,
			electricity_tariff_paise = EXCLUDED.electricity_tariff_paise,
			grace_days = EXCLUDED.grace_days,
			late_penalty_points_per_day = EXCLUDED.late_penalty_points_per_day,
			late_penalty_max_points = EXCLUDED.late_penalty_max_points,
			updated_at = EXCLUDED.updated_at`,
		s.PropertyID, s.PointValuePaise, s.MonthlyBudgetPaise, s.EarnCapPerTenant,
		s.RSVPSubCap, s.ExpiryDays, s.FloorBonusThreshold, s.ElectricityTariffPaise,
		s.GraceDays, s.LatePenaltyPointsPerDay, s.LatePenaltyMaxPoints,
		s.UpdatedAt,
	)
	return err
}

func (r *GamificationRepo) RecordStreakDueEvent(ctx context.Context, tenantID, dueID uuid.UUID) (bool, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO tenant_streak_due_events (tenant_id, due_id)
		VALUES ($1, $2)
		ON CONFLICT (tenant_id, due_id) DO NOTHING
		RETURNING id`, tenantID, dueID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // already processed
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *GamificationRepo) RecordMilestoneAward(ctx context.Context, tenantID uuid.UUID, months int) (bool, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO tenant_milestone_awards (tenant_id, milestone_months)
		VALUES ($1, $2)
		ON CONFLICT (tenant_id, milestone_months) DO NOTHING
		RETURNING id`, tenantID, months,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // already awarded
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *GamificationRepo) ListPointRules(ctx context.Context, propertyID uuid.UUID) ([]domain.PointRule, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, code, name, description, points, monthly_cap, is_rsvp, active, created_at
		FROM point_rules WHERE property_id=$1 ORDER BY points DESC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PointRule
	for rows.Next() {
		var pr domain.PointRule
		if err := rows.Scan(&pr.ID, &pr.PropertyID, &pr.Code, &pr.Name, &pr.Description, &pr.Points, &pr.MonthlyCap, &pr.IsRSVP, &pr.Active, &pr.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) GetPointRuleByCode(ctx context.Context, propertyID uuid.UUID, code string) (*domain.PointRule, error) {
	var pr domain.PointRule
	err := r.pool.QueryRow(ctx, `
		SELECT id, property_id, code, name, description, points, monthly_cap, is_rsvp, active, created_at
		FROM point_rules WHERE property_id=$1 AND code=$2`, propertyID, code,
	).Scan(&pr.ID, &pr.PropertyID, &pr.Code, &pr.Name, &pr.Description, &pr.Points, &pr.MonthlyCap, &pr.IsRSVP, &pr.Active, &pr.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

// 3. Points Ledger & Streaks
// P0 Invariant: all negative deltas permanently reduce balance and never expire.
func (r *GamificationRepo) GetActiveBalance(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var balance int
	err := r.pool.QueryRow(ctx, `
		SELECT GREATEST(0, COALESCE(
			SUM(CASE WHEN delta > 0 AND (expires_at IS NULL OR expires_at > NOW()) THEN delta ELSE 0 END)
			+ SUM(CASE WHEN delta < 0 THEN delta ELSE 0 END),
			0
		))
		FROM points_ledger
		WHERE tenant_id = $1`, tenantID,
	).Scan(&balance)
	return balance, err
}

func (r *GamificationRepo) GetActiveBalanceTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (int, error) {
	var balance int
	err := tx.QueryRow(ctx, `
		SELECT GREATEST(0, COALESCE(
			SUM(CASE WHEN delta > 0 AND (expires_at IS NULL OR expires_at > NOW()) THEN delta ELSE 0 END)
			+ SUM(CASE WHEN delta < 0 THEN delta ELSE 0 END),
			0
		))
		FROM points_ledger
		WHERE tenant_id = $1`, tenantID,
	).Scan(&balance)
	return balance, err
}

func (r *GamificationRepo) GetExpiringSoon(ctx context.Context, tenantID uuid.UUID, withinDays int) (int, time.Time, error) {
	now := time.Now().UTC()
	cutoff := now.AddDate(0, 0, withinDays)
	var expiring int
	var minExpires sql.NullTime
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(delta), 0), MIN(expires_at)
		FROM points_ledger
		WHERE tenant_id=$1 AND delta > 0 AND expires_at > $2 AND expires_at <= $3`,
		tenantID, now, cutoff,
	).Scan(&expiring, &minExpires)
	var earliest time.Time
	if minExpires.Valid {
		earliest = minExpires.Time
	}
	return expiring, earliest, err
}

func (r *GamificationRepo) GetTenantMonthPoints(ctx context.Context, tenantID uuid.UUID, monthYear string, isRSVP bool) (int, error) {
	var sum int
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(l.delta), 0)
		FROM points_ledger l
		JOIN point_rules r ON r.property_id = l.property_id AND r.code = l.rule_code
		WHERE l.tenant_id=$1 AND l.delta > 0
		  AND r.is_rsvp = $2
		  AND TO_CHAR(l.created_at, 'YYYY-MM') = $3`,
		tenantID, isRSVP, monthYear,
	).Scan(&sum)
	return sum, err
}

func (r *GamificationRepo) GetPropertyMonthPoints(ctx context.Context, propertyID uuid.UUID, monthYear string) (int, error) {
	var sum int
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(delta), 0)
		FROM points_ledger
		WHERE property_id=$1 AND delta > 0 AND TO_CHAR(created_at, 'YYYY-MM') = $2`,
		propertyID, monthYear,
	).Scan(&sum)
	return sum, err
}

func (r *GamificationRepo) GetRuleMonthPoints(ctx context.Context, tenantID uuid.UUID, ruleCode string, monthYear string) (int, error) {
	var sum int
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(delta), 0)
		FROM points_ledger
		WHERE tenant_id=$1 AND rule_code=$2 AND delta > 0 AND TO_CHAR(created_at, 'YYYY-MM') = $3`,
		tenantID, ruleCode, monthYear,
	).Scan(&sum)
	return sum, err
}

func (r *GamificationRepo) InsertLedgerEntry(ctx context.Context, entry *domain.PointsLedgerEntry) error {
	// Negative deltas must never have expires_at
	if entry.Delta < 0 {
		entry.ExpiresAt = nil
	}
	now := time.Now().UTC()
	entry.CreatedAt = now
	return r.pool.QueryRow(ctx, `
		INSERT INTO points_ledger (
			tenant_id, property_id, rule_code, delta, ref_type, ref_id, expires_at, created_by, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		entry.TenantID, entry.PropertyID, entry.RuleCode, entry.Delta,
		entry.RefType, entry.RefID, entry.ExpiresAt, entry.CreatedBy, entry.CreatedAt,
	).Scan(&entry.ID)
}

func (r *GamificationRepo) InsertLedgerEntryTx(ctx context.Context, tx pgx.Tx, entry *domain.PointsLedgerEntry) error {
	if entry.Delta < 0 {
		entry.ExpiresAt = nil
	}
	now := time.Now().UTC()
	entry.CreatedAt = now
	return tx.QueryRow(ctx, `
		INSERT INTO points_ledger (
			tenant_id, property_id, rule_code, delta, ref_type, ref_id, expires_at, created_by, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		entry.TenantID, entry.PropertyID, entry.RuleCode, entry.Delta,
		entry.RefType, entry.RefID, entry.ExpiresAt, entry.CreatedBy, entry.CreatedAt,
	).Scan(&entry.ID)
}

func (r *GamificationRepo) ListLedgerByTenant(ctx context.Context, tenantID uuid.UUID, limit int) ([]domain.PointsLedgerEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, property_id, rule_code, delta, ref_type, ref_id, expires_at, created_by, created_at
		FROM points_ledger WHERE tenant_id=$1 ORDER BY created_at DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PointsLedgerEntry
	for rows.Next() {
		var e domain.PointsLedgerEntry
		if err := rows.Scan(&e.ID, &e.TenantID, &e.PropertyID, &e.RuleCode, &e.Delta, &e.RefType, &e.RefID, &e.ExpiresAt, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) GetStreak(ctx context.Context, tenantID uuid.UUID) (*domain.TenantStreak, error) {
	var s domain.TenantStreak
	err := r.pool.QueryRow(ctx, `
		SELECT tenant_id, property_id, on_time_months, cached_balance, last_on_time_due_id,
		       freezes_available, last_freeze_used_at, updated_at
		FROM tenant_streaks WHERE tenant_id=$1`, tenantID,
	).Scan(&s.TenantID, &s.PropertyID, &s.OnTimeMonths, &s.CachedBalance, &s.LastOnTimeDueID, &s.FreezesAvailable, &s.LastFreezeUsedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.TenantStreak{
			TenantID:         tenantID,
			FreezesAvailable: 1,
			UpdatedAt:        time.Now().UTC(),
		}, nil
	}
	return &s, err
}

// LockTenantTx acquires row lock on tenants inside transaction (Universal Lock Hierarchy step 1)
func (r *GamificationRepo) LockTenantTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	var exists int
	return tx.QueryRow(ctx, `SELECT 1 FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&exists)
}

// P0 Fix: Acquire row lock on tenant_streaks inside transaction
func (r *GamificationRepo) GetStreakForUpdate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (*domain.TenantStreak, error) {
	var s domain.TenantStreak
	err := tx.QueryRow(ctx, `
		SELECT tenant_id, property_id, on_time_months, cached_balance, last_on_time_due_id,
		       freezes_available, last_freeze_used_at, updated_at
		FROM tenant_streaks WHERE tenant_id=$1 FOR UPDATE`, tenantID,
	).Scan(&s.TenantID, &s.PropertyID, &s.OnTimeMonths, &s.CachedBalance, &s.LastOnTimeDueID, &s.FreezesAvailable, &s.LastFreezeUsedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("tenant streak not initialized")
	}
	return &s, err
}

func (r *GamificationRepo) UpsertStreak(ctx context.Context, s *domain.TenantStreak) error {
	s.UpdatedAt = time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO tenant_streaks (
			tenant_id, property_id, on_time_months, cached_balance, last_on_time_due_id,
			freezes_available, last_freeze_used_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id) DO UPDATE SET
			on_time_months = EXCLUDED.on_time_months,
			cached_balance = EXCLUDED.cached_balance,
			last_on_time_due_id = EXCLUDED.last_on_time_due_id,
			freezes_available = EXCLUDED.freezes_available,
			last_freeze_used_at = EXCLUDED.last_freeze_used_at,
			updated_at = EXCLUDED.updated_at`,
		s.TenantID, s.PropertyID, s.OnTimeMonths, s.CachedBalance, s.LastOnTimeDueID,
		s.FreezesAvailable, s.LastFreezeUsedAt, s.UpdatedAt,
	)
	return err
}

func (r *GamificationRepo) UpsertStreakTx(ctx context.Context, tx pgx.Tx, s *domain.TenantStreak) error {
	s.UpdatedAt = time.Now().UTC()
	_, err := tx.Exec(ctx, `
		INSERT INTO tenant_streaks (
			tenant_id, property_id, on_time_months, cached_balance, last_on_time_due_id,
			freezes_available, last_freeze_used_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id) DO UPDATE SET
			on_time_months = EXCLUDED.on_time_months,
			cached_balance = EXCLUDED.cached_balance,
			last_on_time_due_id = EXCLUDED.last_on_time_due_id,
			freezes_available = EXCLUDED.freezes_available,
			last_freeze_used_at = EXCLUDED.last_freeze_used_at,
			updated_at = EXCLUDED.updated_at`,
		s.TenantID, s.PropertyID, s.OnTimeMonths, s.CachedBalance, s.LastOnTimeDueID,
		s.FreezesAvailable, s.LastFreezeUsedAt, s.UpdatedAt,
	)
	return err
}

// 4. Rewards & Redemptions
func (r *GamificationRepo) ListRewardsCatalog(ctx context.Context, propertyID uuid.UUID) ([]domain.RewardsCatalogItem, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, code, title, description, category, points_cost, min_tenure_months, is_active, metadata, created_at
		FROM rewards_catalog WHERE property_id=$1 AND is_active=true ORDER BY points_cost ASC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RewardsCatalogItem
	for rows.Next() {
		var item domain.RewardsCatalogItem
		if err := rows.Scan(&item.ID, &item.PropertyID, &item.Code, &item.Title, &item.Description, &item.Category, &item.PointsCost, &item.MinTenureMonths, &item.IsActive, &item.Metadata, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) GetRewardByID(ctx context.Context, id uuid.UUID) (*domain.RewardsCatalogItem, error) {
	var item domain.RewardsCatalogItem
	err := r.pool.QueryRow(ctx, `
		SELECT id, property_id, code, title, description, category, points_cost, min_tenure_months, is_active, metadata, created_at
		FROM rewards_catalog WHERE id=$1`, id,
	).Scan(&item.ID, &item.PropertyID, &item.Code, &item.Title, &item.Description, &item.Category, &item.PointsCost, &item.MinTenureMonths, &item.IsActive, &item.Metadata, &item.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// P1 Fix: Count step-3 violations in active quarter to gate cash redemptions
func (r *GamificationRepo) CountStep3ViolationsInQuarter(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM violations
		WHERE tenant_id = $1 AND step >= 3 AND created_at >= DATE_TRUNC('quarter', NOW())`, tenantID,
	).Scan(&count)
	return count, err
}

func (r *GamificationRepo) CreateRedemptionTx(ctx context.Context, tx pgx.Tx, red *domain.Redemption) error {
	now := time.Now().UTC()
	red.CreatedAt = now
	red.UpdatedAt = now
	if red.Status == "" {
		red.Status = "completed"
	}
	return tx.QueryRow(ctx, `
		INSERT INTO redemptions (
			tenant_id, property_id, reward_id, points_spent, status, applied_due_id, coupon_code, metadata, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		red.TenantID, red.PropertyID, red.RewardID, red.PointsSpent, red.Status,
		red.AppliedDueID, red.CouponCode, red.Metadata, red.CreatedAt, red.UpdatedAt,
	).Scan(&red.ID)
}

func (r *GamificationRepo) ListRedemptionsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Redemption, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, property_id, reward_id, points_spent, status, applied_due_id, coupon_code, metadata, created_at, updated_at
		FROM redemptions WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Redemption
	for rows.Next() {
		var red domain.Redemption
		if err := rows.Scan(&red.ID, &red.TenantID, &red.PropertyID, &red.RewardID, &red.PointsSpent, &red.Status, &red.AppliedDueID, &red.CouponCode, &red.Metadata, &red.CreatedAt, &red.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, red)
	}
	return out, rows.Err()
}

// 5. Inspections
func (r *GamificationRepo) CreateInspection(ctx context.Context, insp *domain.Inspection) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := time.Now().UTC()
	insp.CreatedAt = now
	if insp.InspectedAt.IsZero() {
		insp.InspectedAt = now
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO inspections (
			property_id, room_id, floor_id, inspector_user_id, inspection_type,
			score_percent, passed, notes, inspected_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		insp.PropertyID, insp.RoomID, insp.FloorID, insp.InspectorUserID,
		insp.InspectionType, insp.ScorePercent, insp.Passed, insp.Notes,
		insp.InspectedAt, insp.CreatedAt,
	).Scan(&insp.ID)
	if err != nil {
		return err
	}

	for i := range insp.Items {
		item := &insp.Items[i]
		item.InspectionID = insp.ID
		if item.ResolutionStatus == "" {
			item.ResolutionStatus = "none"
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO inspection_items (
				inspection_id, item_key, description, passed, photo_bytes, notes, resolution_status
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id`,
			item.InspectionID, item.ItemKey, item.Description, item.Passed,
			nullIfEmptyBytes(item.PhotoBytes), item.Notes, item.ResolutionStatus,
		).Scan(&item.ID)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (r *GamificationRepo) ListInspections(ctx context.Context, propertyID uuid.UUID, roomID *uuid.UUID, floorID *uuid.UUID, limit int) ([]domain.Inspection, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT id, property_id, room_id, floor_id, inspector_user_id, inspection_type, score_percent, passed, notes, inspected_at, created_at
	      FROM inspections WHERE property_id=$1`
	args := []any{propertyID}
	if roomID != nil {
		args = append(args, *roomID)
		q += ` AND room_id=$` + string(rune('0'+len(args)))
	}
	if floorID != nil {
		args = append(args, *floorID)
		q += ` AND floor_id=$` + string(rune('0'+len(args)))
	}
	q += ` ORDER BY inspected_at DESC LIMIT $` + string(rune('0'+len(args)+1))
	args = append(args, limit)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Inspection
	for rows.Next() {
		var insp domain.Inspection
		if err := rows.Scan(&insp.ID, &insp.PropertyID, &insp.RoomID, &insp.FloorID, &insp.InspectorUserID, &insp.InspectionType, &insp.ScorePercent, &insp.Passed, &insp.Notes, &insp.InspectedAt, &insp.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, insp)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) GetInspectionByID(ctx context.Context, id uuid.UUID) (*domain.Inspection, error) {
	var insp domain.Inspection
	err := r.pool.QueryRow(ctx, `
		SELECT id, property_id, room_id, floor_id, inspector_user_id, inspection_type, score_percent, passed, notes, inspected_at, created_at
		FROM inspections WHERE id=$1`, id,
	).Scan(&insp.ID, &insp.PropertyID, &insp.RoomID, &insp.FloorID, &insp.InspectorUserID, &insp.InspectionType, &insp.ScorePercent, &insp.Passed, &insp.Notes, &insp.InspectedAt, &insp.CreatedAt)
	if err != nil {
		return nil, err
	}

	rows, err := r.pool.Query(ctx, `
		SELECT id, inspection_id, item_key, description, passed, (photo_bytes IS NOT NULL), notes, disputed_at, dispute_note, resolved_at, resolved_by, resolution_status
		FROM inspection_items WHERE inspection_id=$1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it domain.InspectionItem
		if err := rows.Scan(&it.ID, &it.InspectionID, &it.ItemKey, &it.Description, &it.Passed, &it.HasPhoto, &it.Notes, &it.DisputedAt, &it.DisputeNote, &it.ResolvedAt, &it.ResolvedBy, &it.ResolutionStatus); err != nil {
			return nil, err
		}
		insp.Items = append(insp.Items, it)
	}
	return &insp, nil
}

func (r *GamificationRepo) DisputeInspectionItem(ctx context.Context, itemID uuid.UUID, disputeNote string) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		UPDATE inspection_items
		SET disputed_at = $2, dispute_note = $3, resolution_status = 'disputed'
		WHERE id = $1 AND resolution_status = 'none'`,
		itemID, now, disputeNote,
	)
	return err
}

func (r *GamificationRepo) ResolveInspectionItem(ctx context.Context, itemID uuid.UUID, resolvedBy uuid.UUID, status string) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		UPDATE inspection_items
		SET resolved_at = $2, resolved_by = $3, resolution_status = $4
		WHERE id = $1`,
		itemID, now, resolvedBy, status,
	)
	return err
}

func (r *GamificationRepo) GetFloorCleanlinessAverage(ctx context.Context, floorID uuid.UUID, monthYear string) (float64, error) {
	var avg sql.NullFloat64
	err := r.pool.QueryRow(ctx, `
		SELECT AVG(score_percent)
		FROM inspections
		WHERE floor_id = $1 AND TO_CHAR(inspected_at, 'YYYY-MM') = $2`,
		floorID, monthYear,
	).Scan(&avg)
	if err != nil || !avg.Valid {
		return 0, err
	}
	return avg.Float64, nil
}

func (r *GamificationRepo) CreateVendorInspection(ctx context.Context, vi *domain.VendorInspection) error {
	now := time.Now().UTC()
	vi.CreatedAt = now
	if vi.InspectedAt.IsZero() {
		vi.InspectedAt = now
	}
	vi.HasPhoto = len(vi.PhotoBytes) > 0
	return r.pool.QueryRow(ctx, `
		INSERT INTO vendor_inspections (
			property_id, inspector_user_id, vendor_name, inspection_type, score_percent, notes, photo_bytes, penalty_paise, inspected_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		vi.PropertyID, vi.InspectorUserID, vi.VendorName, vi.InspectionType, vi.ScorePercent, vi.Notes,
		nullIfEmptyBytes(vi.PhotoBytes), vi.PenaltyPaise, vi.InspectedAt, vi.CreatedAt,
	).Scan(&vi.ID)
}

func (r *GamificationRepo) ListVendorInspections(ctx context.Context, propertyID uuid.UUID, limit int) ([]domain.VendorInspection, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, inspector_user_id, vendor_name, inspection_type, score_percent, notes, (photo_bytes IS NOT NULL), penalty_paise, inspected_at, created_at
		FROM vendor_inspections WHERE property_id=$1 ORDER BY inspected_at DESC LIMIT $2`, propertyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.VendorInspection
	for rows.Next() {
		var vi domain.VendorInspection
		if err := rows.Scan(&vi.ID, &vi.PropertyID, &vi.InspectorUserID, &vi.VendorName, &vi.InspectionType, &vi.ScorePercent, &vi.Notes, &vi.HasPhoto, &vi.PenaltyPaise, &vi.InspectedAt, &vi.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, vi)
	}
	return out, rows.Err()
}

// 6. Violations & Hazards
// P2 Fix: explicit 90-day anchored window
func (r *GamificationRepo) CountRecentViolations(ctx context.Context, tenantID uuid.UUID, windowDays int) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM violations
		WHERE tenant_id = $1 AND created_at >= NOW() - ($2 || ' days')::INTERVAL`,
		tenantID, windowDays,
	).Scan(&count)
	return count, err
}

func (r *GamificationRepo) CreateViolation(ctx context.Context, v *domain.Violation) error {
	now := time.Now().UTC()
	v.CreatedAt = now
	v.HasEvidence = len(v.EvidenceBytes) > 0
	return r.pool.QueryRow(ctx, `
		INSERT INTO violations (
			tenant_id, property_id, rule_code, severity, step, description, evidence_bytes, created_by, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		v.TenantID, v.PropertyID, v.RuleCode, v.Severity, v.Step, v.Description,
		nullIfEmptyBytes(v.EvidenceBytes), v.CreatedBy, v.CreatedAt,
	).Scan(&v.ID)
}

func (r *GamificationRepo) ListViolationsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Violation, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, property_id, rule_code, severity, step, description, (evidence_bytes IS NOT NULL), acknowledged_at, created_by, created_at
		FROM violations WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Violation
	for rows.Next() {
		var v domain.Violation
		if err := rows.Scan(&v.ID, &v.TenantID, &v.PropertyID, &v.RuleCode, &v.Severity, &v.Step, &v.Description, &v.HasEvidence, &v.AcknowledgedAt, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) ListViolationsByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.Violation, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, property_id, rule_code, severity, step, description, (evidence_bytes IS NOT NULL), acknowledged_at, created_by, created_at
		FROM violations WHERE property_id=$1 ORDER BY created_at DESC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Violation
	for rows.Next() {
		var v domain.Violation
		if err := rows.Scan(&v.ID, &v.TenantID, &v.PropertyID, &v.RuleCode, &v.Severity, &v.Step, &v.Description, &v.HasEvidence, &v.AcknowledgedAt, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) CreateHazard(ctx context.Context, h *domain.HazardReport) error {
	now := time.Now().UTC()
	h.CreatedAt = now
	h.Status = "open"
	h.HasPhoto = len(h.PhotoBytes) > 0
	return r.pool.QueryRow(ctx, `
		INSERT INTO hazards (
			property_id, reported_by_tenant_id, category, description, photo_bytes, status, points_awarded, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		h.PropertyID, h.ReportedByTenantID, h.Category, h.Description,
		nullIfEmptyBytes(h.PhotoBytes), h.Status, h.PointsAwarded, h.CreatedAt,
	).Scan(&h.ID)
}

func (r *GamificationRepo) ListHazards(ctx context.Context, propertyID uuid.UUID, status string) ([]domain.HazardReport, error) {
	q := `SELECT id, property_id, reported_by_tenant_id, category, description, (photo_bytes IS NOT NULL), status, resolved_at, resolved_by, points_awarded, created_at
	      FROM hazards WHERE property_id=$1`
	args := []any{propertyID}
	if status != "" {
		args = append(args, status)
		q += ` AND status=$2`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.HazardReport
	for rows.Next() {
		var h domain.HazardReport
		if err := rows.Scan(&h.ID, &h.PropertyID, &h.ReportedByTenantID, &h.Category, &h.Description, &h.HasPhoto, &h.Status, &h.ResolvedAt, &h.ResolvedBy, &h.PointsAwarded, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) GetHazardByID(ctx context.Context, id uuid.UUID) (*domain.HazardReport, error) {
	var h domain.HazardReport
	err := r.pool.QueryRow(ctx, `
		SELECT id, property_id, reported_by_tenant_id, category, description, (photo_bytes IS NOT NULL), status, resolved_at, resolved_by, points_awarded, created_at
		FROM hazards WHERE id=$1`, id,
	).Scan(&h.ID, &h.PropertyID, &h.ReportedByTenantID, &h.Category, &h.Description, &h.HasPhoto, &h.Status, &h.ResolvedAt, &h.ResolvedBy, &h.PointsAwarded, &h.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (r *GamificationRepo) ResolveHazard(ctx context.Context, id uuid.UUID, resolvedBy uuid.UUID, status string) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		UPDATE hazards
		SET status=$2, resolved_at=$3, resolved_by=$4, points_awarded=true
		WHERE id=$1`,
		id, status, now, resolvedBy,
	)
	return err
}

// 7. Meter Readings
func (r *GamificationRepo) GetLatestMeterReading(ctx context.Context, propertyID uuid.UUID, roomID *uuid.UUID, floorID *uuid.UUID, kind string) (*domain.MeterReading, error) {
	q := `SELECT id, property_id, room_id, floor_id, kind, reading_value, reading_at, source, recorded_by, created_at
	      FROM meter_readings WHERE property_id=$1 AND kind=$2`
	args := []any{propertyID, kind}
	if roomID != nil {
		args = append(args, *roomID)
		q += ` AND room_id=$` + string(rune('0'+len(args)))
	}
	if floorID != nil {
		args = append(args, *floorID)
		q += ` AND floor_id=$` + string(rune('0'+len(args)))
	}
	q += ` ORDER BY reading_at DESC LIMIT 1`

	var mr domain.MeterReading
	err := r.pool.QueryRow(ctx, q, args...).Scan(
		&mr.ID, &mr.PropertyID, &mr.RoomID, &mr.FloorID, &mr.Kind, &mr.ReadingValue,
		&mr.ReadingAt, &mr.Source, &mr.RecordedBy, &mr.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mr, nil
}

func (r *GamificationRepo) CreateMeterReading(ctx context.Context, mr *domain.MeterReading) error {
	now := time.Now().UTC()
	mr.CreatedAt = now
	if mr.ReadingAt.IsZero() {
		mr.ReadingAt = now
	}
	if mr.Source == "" {
		mr.Source = "manual"
	}
	return r.pool.QueryRow(ctx, `
		INSERT INTO meter_readings (
			property_id, room_id, floor_id, kind, reading_value, reading_at, source, recorded_by, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		mr.PropertyID, mr.RoomID, mr.FloorID, mr.Kind, mr.ReadingValue, mr.ReadingAt,
		mr.Source, mr.RecordedBy, mr.CreatedAt,
	).Scan(&mr.ID)
}

func (r *GamificationRepo) ListMeterReadings(ctx context.Context, propertyID uuid.UUID, roomID *uuid.UUID, floorID *uuid.UUID, limit int) ([]domain.MeterReading, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT id, property_id, room_id, floor_id, kind, reading_value, reading_at, source, recorded_by, created_at
	      FROM meter_readings WHERE property_id=$1`
	args := []any{propertyID}
	if roomID != nil {
		args = append(args, *roomID)
		q += ` AND room_id=$` + string(rune('0'+len(args)))
	}
	if floorID != nil {
		args = append(args, *floorID)
		q += ` AND floor_id=$` + string(rune('0'+len(args)))
	}
	q += ` ORDER BY reading_at DESC LIMIT $` + string(rune('0'+len(args)+1))
	args = append(args, limit)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MeterReading
	for rows.Next() {
		var mr domain.MeterReading
		if err := rows.Scan(&mr.ID, &mr.PropertyID, &mr.RoomID, &mr.FloorID, &mr.Kind, &mr.ReadingValue, &mr.ReadingAt, &mr.Source, &mr.RecordedBy, &mr.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, mr)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) GetFloorWaterConsumption(ctx context.Context, floorID uuid.UUID, startDate, endDate time.Time) (float64, error) {
	var maxR, minR sql.NullFloat64
	err := r.pool.QueryRow(ctx, `
		SELECT MAX(reading_value), MIN(reading_value)
		FROM meter_readings
		WHERE floor_id=$1 AND kind='water' AND reading_at BETWEEN $2 AND $3`,
		floorID, startDate, endDate,
	).Scan(&maxR, &minR)
	if err != nil || !maxR.Valid || !minR.Valid {
		return 0, err
	}
	return maxR.Float64 - minR.Float64, nil
}

func (r *GamificationRepo) CountFloorActiveTenants(ctx context.Context, floorID uuid.UUID) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(t.id)
		FROM tenants t
		JOIN rooms rm ON rm.id = t.room_id
		WHERE rm.floor_id = $1 AND t.status = 'active'`,
		floorID,
	).Scan(&count)
	return count, err
}

// 8. Meal RSVPs & Menu Polls
func (r *GamificationRepo) UpsertMealRSVP(ctx context.Context, rsvp *domain.MealRSVP) error {
	now := time.Now().UTC()
	rsvp.CreatedAt = now
	rsvp.UpdatedAt = now
	return r.pool.QueryRow(ctx, `
		INSERT INTO meal_rsvps (
			tenant_id, property_id, meal_date, meal_slot, attending, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, meal_date, meal_slot) DO UPDATE SET
			attending = EXCLUDED.attending,
			updated_at = EXCLUDED.updated_at
		RETURNING id`,
		rsvp.TenantID, rsvp.PropertyID, rsvp.MealDate, rsvp.MealSlot, rsvp.Attending, rsvp.CreatedAt, rsvp.UpdatedAt,
	).Scan(&rsvp.ID)
}

func (r *GamificationRepo) ListMealRSVPsByDate(ctx context.Context, propertyID uuid.UUID, date time.Time) ([]domain.MealRSVP, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, property_id, meal_date, meal_slot, attending, created_at, updated_at
		FROM meal_rsvps WHERE property_id=$1 AND meal_date=$2`, propertyID, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MealRSVP
	for rows.Next() {
		var mr domain.MealRSVP
		if err := rows.Scan(&mr.ID, &mr.TenantID, &mr.PropertyID, &mr.MealDate, &mr.MealSlot, &mr.Attending, &mr.CreatedAt, &mr.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, mr)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) GetTenantMealRSVP(ctx context.Context, tenantID uuid.UUID, date time.Time, slot string) (*domain.MealRSVP, error) {
	var mr domain.MealRSVP
	err := r.pool.QueryRow(ctx, `
		SELECT id, tenant_id, property_id, meal_date, meal_slot, attending, created_at, updated_at
		FROM meal_rsvps WHERE tenant_id=$1 AND meal_date=$2 AND meal_slot=$3`,
		tenantID, date, slot,
	).Scan(&mr.ID, &mr.TenantID, &mr.PropertyID, &mr.MealDate, &mr.MealSlot, &mr.Attending, &mr.CreatedAt, &mr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mr, nil
}

func (r *GamificationRepo) GetActiveMenuPoll(ctx context.Context, propertyID uuid.UUID, monthYear string) (*domain.MenuPoll, error) {
	var mp domain.MenuPoll
	err := r.pool.QueryRow(ctx, `
		SELECT id, property_id, month_year, title, options, closed_at, created_at
		FROM menu_polls
		WHERE property_id=$1 AND month_year=$2 AND (closed_at IS NULL OR closed_at > NOW())
		ORDER BY created_at DESC LIMIT 1`,
		propertyID, monthYear,
	).Scan(&mp.ID, &mp.PropertyID, &mp.MonthYear, &mp.Title, &mp.Options, &mp.ClosedAt, &mp.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mp, nil
}

func (r *GamificationRepo) CreateMenuPoll(ctx context.Context, poll *domain.MenuPoll) error {
	now := time.Now().UTC()
	poll.CreatedAt = now
	return r.pool.QueryRow(ctx, `
		INSERT INTO menu_polls (property_id, month_year, title, options, closed_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		poll.PropertyID, poll.MonthYear, poll.Title, poll.Options, poll.ClosedAt, poll.CreatedAt,
	).Scan(&poll.ID)
}

func (r *GamificationRepo) VoteMenuPoll(ctx context.Context, vote *domain.MenuVote) error {
	now := time.Now().UTC()
	vote.CreatedAt = now
	return r.pool.QueryRow(ctx, `
		INSERT INTO menu_votes (poll_id, tenant_id, option_id, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (poll_id, tenant_id) DO UPDATE SET
			option_id = EXCLUDED.option_id,
			created_at = EXCLUDED.created_at
		RETURNING id`,
		vote.PollID, vote.TenantID, vote.OptionID, vote.CreatedAt,
	).Scan(&vote.ID)
}

func (r *GamificationRepo) GetMenuPollVotes(ctx context.Context, pollID uuid.UUID) (map[string]int, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT option_id, COUNT(*)
		FROM menu_votes WHERE poll_id=$1 GROUP BY option_id`, pollID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var opt string
		var count int
		if err := rows.Scan(&opt, &count); err != nil {
			return nil, err
		}
		out[opt] = count
	}
	return out, rows.Err()
}

// 9. Referrals
func (r *GamificationRepo) CreateReferral(ctx context.Context, ref *domain.Referral) error {
	now := time.Now().UTC()
	ref.CreatedAt = now
	ref.Status = "pending"
	return r.pool.QueryRow(ctx, `
		INSERT INTO referrals (property_id, referrer_tenant_id, phone, name, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		ref.PropertyID, ref.ReferrerTenantID, ref.Phone, ref.Name, ref.Status, ref.CreatedAt,
	).Scan(&ref.ID)
}

func (r *GamificationRepo) GetReferralByPhone(ctx context.Context, propertyID uuid.UUID, phone string) (*domain.Referral, error) {
	var ref domain.Referral
	err := r.pool.QueryRow(ctx, `
		SELECT id, property_id, referrer_tenant_id, referred_tenant_id, phone, name, status, points_awarded, created_at, rewarded_at
		FROM referrals WHERE property_id=$1 AND phone=$2 AND status='pending' LIMIT 1`,
		propertyID, phone,
	).Scan(&ref.ID, &ref.PropertyID, &ref.ReferrerTenantID, &ref.ReferredTenantID, &ref.Phone, &ref.Name, &ref.Status, &ref.PointsAwarded, &ref.CreatedAt, &ref.RewardedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func (r *GamificationRepo) MarkReferralRewarded(ctx context.Context, id uuid.UUID, referredTenantID uuid.UUID, points int) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		UPDATE referrals
		SET referred_tenant_id=$2, status='rewarded', points_awarded=$3, rewarded_at=$4
		WHERE id=$1`,
		id, referredTenantID, points, now,
	)
	return err
}

func (r *GamificationRepo) ListReferralsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Referral, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, referrer_tenant_id, referred_tenant_id, phone, name, status, points_awarded, created_at, rewarded_at
		FROM referrals WHERE referrer_tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Referral
	for rows.Next() {
		var ref domain.Referral
		if err := rows.Scan(&ref.ID, &ref.PropertyID, &ref.ReferrerTenantID, &ref.ReferredTenantID, &ref.Phone, &ref.Name, &ref.Status, &ref.PointsAwarded, &ref.CreatedAt, &ref.RewardedAt); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// 10. Leaderboards
func (r *GamificationRepo) GetTopStreaks(ctx context.Context, propertyID uuid.UUID, limit int) ([]domain.TenantStreak, error) {
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `
		SELECT tenant_id, property_id, on_time_months, cached_balance, last_on_time_due_id, freezes_available, last_freeze_used_at, updated_at
		FROM tenant_streaks
		WHERE property_id=$1
		ORDER BY on_time_months DESC, cached_balance DESC
		LIMIT $2`, propertyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TenantStreak
	for rows.Next() {
		var s domain.TenantStreak
		if err := rows.Scan(&s.TenantID, &s.PropertyID, &s.OnTimeMonths, &s.CachedBalance, &s.LastOnTimeDueID, &s.FreezesAvailable, &s.LastFreezeUsedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *GamificationRepo) GetFloorCleanScores(ctx context.Context, propertyID uuid.UUID, monthYear string) (map[uuid.UUID]float64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT floor_id, AVG(score_percent)
		FROM inspections
		WHERE property_id=$1 AND floor_id IS NOT NULL AND TO_CHAR(inspected_at, 'YYYY-MM') = $2
		GROUP BY floor_id`, propertyID, monthYear)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]float64)
	for rows.Next() {
		var fid uuid.UUID
		var avg float64
		if err := rows.Scan(&fid, &avg); err != nil {
			return nil, err
		}
		out[fid] = avg
	}
	return out, rows.Err()
}
