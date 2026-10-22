package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func (r *FinanceRepo) GetTrialBalance(ctx context.Context, propertyID uuid.UUID, to time.Time) ([]domain.TrialBalanceLine, error) {
	rows, err := r.db.Query(ctx, `
		SELECT account_code, account_class, debit_paise, credit_paise, balance_paise
		FROM ledger_trial_balance($1, $2)`, propertyID, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TrialBalanceLine
	for rows.Next() {
		var l domain.TrialBalanceLine
		if err := rows.Scan(&l.AccountCode, &l.AccountClass, &l.DebitPaise, &l.CreditPaise, &l.BalancePaise); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) GetIncomeStatement(ctx context.Context, propertyID uuid.UUID, from, to time.Time) ([]domain.StatementLine, error) {
	rows, err := r.db.Query(ctx, `
		SELECT section, account_code, amount_paise, sort_order
		FROM ledger_income_statement($1, $2, $3)`, propertyID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.StatementLine
	for rows.Next() {
		var l domain.StatementLine
		if err := rows.Scan(&l.Section, &l.AccountCode, &l.AmountPaise, &l.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) GetBalanceSheet(ctx context.Context, propertyID uuid.UUID, to time.Time) ([]domain.StatementLine, error) {
	rows, err := r.db.Query(ctx, `
		SELECT section, account_code, amount_paise, sort_order
		FROM ledger_balance_sheet($1, $2)`, propertyID, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.StatementLine
	for rows.Next() {
		var l domain.StatementLine
		if err := rows.Scan(&l.Section, &l.AccountCode, &l.AmountPaise, &l.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) GetCashFlow(ctx context.Context, propertyID uuid.UUID, from, to time.Time) ([]domain.CashFlowLine, error) {
	rows, err := r.db.Query(ctx, `
		SELECT section, label, amount_paise, sort_order
		FROM ledger_cash_flow($1, $2, $3)`, propertyID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CashFlowLine
	for rows.Next() {
		var l domain.CashFlowLine
		if err := rows.Scan(&l.Section, &l.Label, &l.AmountPaise, &l.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) GetReconcilingItems(ctx context.Context, propertyID *uuid.UUID, asOf time.Time) ([]domain.ReconcilingItem, error) {
	var dateArg any
	if !asOf.IsZero() {
		dateArg = asOf.Format("2006-01-02")
	}
	rows, err := r.db.Query(ctx, `
		SELECT item_type, property_id, ref, amount_paise, originated_on::text, age_days, age_bucket, category, escalation
		FROM ledger_reconciling_items($1, COALESCE($2::date, (now() AT TIME ZONE 'UTC')::date))`, propertyID, dateArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ReconcilingItem
	for rows.Next() {
		var item domain.ReconcilingItem
		if err := rows.Scan(&item.ItemType, &item.PropertyID, &item.Ref, &item.AmountPaise, &item.OriginatedOn, &item.AgeDays, &item.AgeBucket, &item.Category, &item.Escalation); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return []byte(`{}`)
	}
	return b
}

func (r *FinanceRepo) UpsertDailyFinancialRollup(ctx context.Context, ro *domain.DailyFinancialRollup) error {
	now := time.Now().UTC()
	if ro.ID == uuid.Nil {
		ro.ID = uuid.New()
	}
	if ro.CreatedAt.IsZero() {
		ro.CreatedAt = now
	}
	ro.UpdatedAt = now
	_, err := r.db.Exec(ctx, `
		INSERT INTO daily_financial_rollups (
			id, property_id, rollup_date, collected_paise, due_paise, expense_paise,
			net_cash_flow_paise, total_rooms, occupied_rooms, capacity_beds, occupied_beds,
			occupancy_rate_pct, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (property_id, rollup_date) DO UPDATE SET
			collected_paise = EXCLUDED.collected_paise,
			due_paise = EXCLUDED.due_paise,
			expense_paise = EXCLUDED.expense_paise,
			net_cash_flow_paise = EXCLUDED.net_cash_flow_paise,
			total_rooms = EXCLUDED.total_rooms,
			occupied_rooms = EXCLUDED.occupied_rooms,
			capacity_beds = EXCLUDED.capacity_beds,
			occupied_beds = EXCLUDED.occupied_beds,
			occupancy_rate_pct = EXCLUDED.occupancy_rate_pct,
			updated_at = EXCLUDED.updated_at`,
		ro.ID, ro.PropertyID, ro.RollupDate, ro.CollectedPaise, ro.DuePaise, ro.ExpensePaise,
		ro.NetCashFlowPaise, ro.TotalRooms, ro.OccupiedRooms, ro.CapacityBeds, ro.OccupiedBeds,
		ro.OccupancyRatePct, ro.CreatedAt, ro.UpdatedAt,
	)
	return err
}

func (r *FinanceRepo) ListDailyFinancialRollups(ctx context.Context, propertyID uuid.UUID, from, to time.Time) ([]domain.DailyFinancialRollup, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, rollup_date, collected_paise, due_paise, expense_paise,
		       net_cash_flow_paise, total_rooms, occupied_rooms, capacity_beds, occupied_beds,
		       occupancy_rate_pct, created_at, updated_at
		FROM daily_financial_rollups
		WHERE property_id = $1 AND rollup_date >= $2 AND rollup_date <= $3
		ORDER BY rollup_date ASC`,
		propertyID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DailyFinancialRollup
	for rows.Next() {
		var ro domain.DailyFinancialRollup
		if err := rows.Scan(
			&ro.ID, &ro.PropertyID, &ro.RollupDate, &ro.CollectedPaise, &ro.DuePaise, &ro.ExpensePaise,
			&ro.NetCashFlowPaise, &ro.TotalRooms, &ro.OccupiedRooms, &ro.CapacityBeds, &ro.OccupiedBeds,
			&ro.OccupancyRatePct, &ro.CreatedAt, &ro.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, ro)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) CollectDailyMetrics(ctx context.Context, propertyID uuid.UUID, day time.Time) (*domain.DailyFinancialRollup, error) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	dayIST := day.In(loc)
	startIST := time.Date(dayIST.Year(), dayIST.Month(), dayIST.Day(), 0, 0, 0, 0, loc)
	endIST := startIST.AddDate(0, 0, 1)
	startUTC := startIST.UTC()
	endUTC := endIST.UTC()
	rollupDate := time.Date(dayIST.Year(), dayIST.Month(), dayIST.Day(), 0, 0, 0, 0, time.UTC)

	var collectedPaise int64
	if err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(p.amount), 0)
		FROM payments p
		LEFT JOIN dues d ON p.due_id = d.id
		WHERE (p.property_id = $1 OR d.property_id = $1)
		  AND p.matched_at >= $2 AND p.matched_at < $3`,
		propertyID, startUTC, endUTC).Scan(&collectedPaise); err != nil {
		return nil, fmt.Errorf("collect daily metrics (payments): %w", err)
	}

	var duePaise int64
	if err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM dues
		WHERE property_id = $1
		  AND due_date >= $2 AND due_date < $3`,
		propertyID, startUTC, endUTC).Scan(&duePaise); err != nil {
		return nil, fmt.Errorf("collect daily metrics (dues): %w", err)
	}

	var expensePaise int64
	if err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_paise), 0)
		FROM expenses
		WHERE property_id = $1
		  AND occurred_at >= $2 AND occurred_at < $3`,
		propertyID, startUTC, endUTC).Scan(&expensePaise); err != nil {
		return nil, fmt.Errorf("collect daily metrics (expenses): %w", err)
	}

	netCashFlow := collectedPaise - expensePaise

	var totalRooms int
	var capacityBeds int
	if err := r.db.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(capacity), 0)
		FROM rooms
		WHERE property_id = $1`,
		propertyID).Scan(&totalRooms, &capacityBeds); err != nil {
		return nil, fmt.Errorf("collect daily metrics (rooms): %w", err)
	}

	var occupiedBeds int
	var occupiedRooms int
	if err := r.db.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT room_id)
		FROM tenants
		WHERE property_id = $1 AND status = 'active'`,
		propertyID).Scan(&occupiedBeds, &occupiedRooms); err != nil {
		return nil, fmt.Errorf("collect daily metrics (tenants): %w", err)
	}

	if capacityBeds == 0 && occupiedBeds > 0 {
		capacityBeds = occupiedBeds
	}

	occRatePct := 0.0
	if capacityBeds > 0 {
		occRatePct = float64(occupiedBeds) / float64(capacityBeds) * 100.0
	}

	now := time.Now().UTC()
	return &domain.DailyFinancialRollup{
		ID:               uuid.New(),
		PropertyID:       propertyID,
		RollupDate:       rollupDate,
		CollectedPaise:   collectedPaise,
		DuePaise:         duePaise,
		ExpensePaise:     expensePaise,
		NetCashFlowPaise: netCashFlow,
		TotalRooms:       totalRooms,
		OccupiedRooms:    occupiedRooms,
		CapacityBeds:     capacityBeds,
		OccupiedBeds:     occupiedBeds,
		OccupancyRatePct: occRatePct,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}
