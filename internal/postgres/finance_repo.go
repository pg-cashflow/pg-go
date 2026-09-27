package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type FinanceRepo struct{ db DBTX }

func NewFinanceRepo(db DBTX) *FinanceRepo { return &FinanceRepo{db: db} }

func isUnique(err error) bool {
	var e *pgconn.PgError
	return errors.As(err, &e) && e.Code == "23505"
}

func (r *FinanceRepo) EnsureDefaults(ctx context.Context, propertyID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `INSERT INTO property_finance_settings (property_id) VALUES ($1) ON CONFLICT DO NOTHING`, propertyID)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `INSERT INTO approval_policies (property_id) VALUES ($1) ON CONFLICT DO NOTHING`, propertyID)
	return err
}

func (r *FinanceRepo) GetPolicy(ctx context.Context, propertyID uuid.UUID) (domain.ApprovalPolicy, error) {
	_ = r.EnsureDefaults(ctx, propertyID)
	var p domain.ApprovalPolicy
	p.PropertyID = propertyID
	err := r.db.QueryRow(ctx, `
		SELECT manager_daily_limit_paise, single_expense_limit_paise, manager_monthly_limit_paise,
		       owner_approval_threshold_paise, reimbursement_threshold_paise, emergency_bypass_enabled
		FROM approval_policies WHERE property_id=$1`, propertyID).Scan(
		&p.ManagerDailyLimitPaise, &p.SingleExpenseLimitPaise, &p.ManagerMonthlyLimitPaise,
		&p.OwnerApprovalThresholdPaise, &p.ReimbursementThresholdPaise, &p.EmergencyBypassEnabled)
	return p, err
}

func (r *FinanceRepo) SavePolicy(ctx context.Context, p domain.ApprovalPolicy) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO approval_policies (property_id, manager_daily_limit_paise, single_expense_limit_paise,
			manager_monthly_limit_paise, owner_approval_threshold_paise, reimbursement_threshold_paise, emergency_bypass_enabled, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NOW())
		ON CONFLICT (property_id) DO UPDATE SET
			manager_daily_limit_paise=EXCLUDED.manager_daily_limit_paise,
			single_expense_limit_paise=EXCLUDED.single_expense_limit_paise,
			manager_monthly_limit_paise=EXCLUDED.manager_monthly_limit_paise,
			owner_approval_threshold_paise=EXCLUDED.owner_approval_threshold_paise,
			reimbursement_threshold_paise=EXCLUDED.reimbursement_threshold_paise,
			emergency_bypass_enabled=EXCLUDED.emergency_bypass_enabled,
			updated_at=NOW()`,
		p.PropertyID, p.ManagerDailyLimitPaise, p.SingleExpenseLimitPaise, p.ManagerMonthlyLimitPaise,
		p.OwnerApprovalThresholdPaise, p.ReimbursementThresholdPaise, p.EmergencyBypassEnabled)
	return err
}

func (r *FinanceRepo) GetSettings(ctx context.Context, propertyID uuid.UUID) (domain.PropertyFinanceSettings, error) {
	_ = r.EnsureDefaults(ctx, propertyID)
	var s domain.PropertyFinanceSettings
	s.PropertyID = propertyID
	err := r.db.QueryRow(ctx, `
		SELECT fiscal_month_start_day, manager_can_view_capital, manager_can_view_roi, manager_can_view_leakage,
		       tdr_effective_bps, tdr_is_estimated
		FROM property_finance_settings WHERE property_id=$1`, propertyID).Scan(
		&s.FiscalMonthStartDay, &s.ManagerCanViewCapital, &s.ManagerCanViewROI, &s.ManagerCanViewLeakage,
		&s.TDREffectiveBPS, &s.TDRIsEstimated)
	return s, err
}

func (r *FinanceRepo) SaveSettings(ctx context.Context, s domain.PropertyFinanceSettings) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO property_finance_settings (property_id, fiscal_month_start_day, manager_can_view_capital,
			manager_can_view_roi, manager_can_view_leakage, tdr_effective_bps, tdr_is_estimated, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NOW())
		ON CONFLICT (property_id) DO UPDATE SET
			fiscal_month_start_day=EXCLUDED.fiscal_month_start_day,
			manager_can_view_capital=EXCLUDED.manager_can_view_capital,
			manager_can_view_roi=EXCLUDED.manager_can_view_roi,
			manager_can_view_leakage=EXCLUDED.manager_can_view_leakage,
			tdr_effective_bps=EXCLUDED.tdr_effective_bps,
			tdr_is_estimated=EXCLUDED.tdr_is_estimated,
			updated_at=NOW()`,
		s.PropertyID, s.FiscalMonthStartDay, s.ManagerCanViewCapital, s.ManagerCanViewROI,
		s.ManagerCanViewLeakage, s.TDREffectiveBPS, s.TDRIsEstimated)
	return err
}

func (r *FinanceRepo) SaveUnifiedSettings(ctx context.Context, propertyID uuid.UUID, settings *domain.PropertyFinanceSettings, policy *domain.ApprovalPolicy, loyalty *domain.PropertyGamificationSettings) error {
	type txBeginner interface {
		Begin(ctx context.Context) (pgx.Tx, error)
	}

	exec := func(tx DBTX) error {
		if settings != nil {
			settings.PropertyID = propertyID
			_, err := tx.Exec(ctx, `
				INSERT INTO property_finance_settings (property_id, fiscal_month_start_day, manager_can_view_capital,
					manager_can_view_roi, manager_can_view_leakage, tdr_effective_bps, tdr_is_estimated, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,NOW())
				ON CONFLICT (property_id) DO UPDATE SET
					fiscal_month_start_day=EXCLUDED.fiscal_month_start_day,
					manager_can_view_capital=EXCLUDED.manager_can_view_capital,
					manager_can_view_roi=EXCLUDED.manager_can_view_roi,
					manager_can_view_leakage=EXCLUDED.manager_can_view_leakage,
					tdr_effective_bps=EXCLUDED.tdr_effective_bps,
					tdr_is_estimated=EXCLUDED.tdr_is_estimated,
					updated_at=NOW()`,
				settings.PropertyID, settings.FiscalMonthStartDay, settings.ManagerCanViewCapital,
				settings.ManagerCanViewROI, settings.ManagerCanViewLeakage, settings.TDREffectiveBPS, settings.TDRIsEstimated)
			if err != nil {
				return fmt.Errorf("save finance settings: %w", err)
			}
		}

		if policy != nil {
			policy.PropertyID = propertyID
			_, err := tx.Exec(ctx, `
				INSERT INTO approval_policies (property_id, manager_daily_limit_paise, single_expense_limit_paise,
					manager_monthly_limit_paise, owner_approval_threshold_paise, reimbursement_threshold_paise, emergency_bypass_enabled, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,NOW())
				ON CONFLICT (property_id) DO UPDATE SET
					manager_daily_limit_paise=EXCLUDED.manager_daily_limit_paise,
					single_expense_limit_paise=EXCLUDED.single_expense_limit_paise,
					manager_monthly_limit_paise=EXCLUDED.manager_monthly_limit_paise,
					owner_approval_threshold_paise=EXCLUDED.owner_approval_threshold_paise,
					reimbursement_threshold_paise=EXCLUDED.reimbursement_threshold_paise,
					emergency_bypass_enabled=EXCLUDED.emergency_bypass_enabled,
					updated_at=NOW()`,
				policy.PropertyID, policy.ManagerDailyLimitPaise, policy.SingleExpenseLimitPaise, policy.ManagerMonthlyLimitPaise,
				policy.OwnerApprovalThresholdPaise, policy.ReimbursementThresholdPaise, policy.EmergencyBypassEnabled)
			if err != nil {
				return fmt.Errorf("save approval policy: %w", err)
			}
		}

		if loyalty != nil {
			loyalty.PropertyID = propertyID
			loyalty.UpdatedAt = time.Now().UTC()
			_, err := tx.Exec(ctx, `
				INSERT INTO property_gamification_settings (
					property_id, point_value_paise, monthly_budget_paise, earn_cap_per_tenant,
					rsvp_sub_cap, expiry_days, floor_bonus_threshold, electricity_tariff_paise, updated_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				ON CONFLICT (property_id) DO UPDATE SET
					point_value_paise = EXCLUDED.point_value_paise,
					monthly_budget_paise = EXCLUDED.monthly_budget_paise,
					earn_cap_per_tenant = EXCLUDED.earn_cap_per_tenant,
					rsvp_sub_cap = EXCLUDED.rsvp_sub_cap,
					expiry_days = EXCLUDED.expiry_days,
					floor_bonus_threshold = EXCLUDED.floor_bonus_threshold,
					electricity_tariff_paise = EXCLUDED.electricity_tariff_paise,
					updated_at = EXCLUDED.updated_at`,
				loyalty.PropertyID, loyalty.PointValuePaise, loyalty.MonthlyBudgetPaise, loyalty.EarnCapPerTenant,
				loyalty.RSVPSubCap, loyalty.ExpiryDays, loyalty.FloorBonusThreshold, loyalty.ElectricityTariffPaise, loyalty.UpdatedAt)
			if err != nil {
				return fmt.Errorf("save loyalty settings: %w", err)
			}
		}
		return nil
	}

	if b, ok := r.db.(txBeginner); ok {
		tx, err := b.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := exec(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	return exec(r.db)
}

func (r *FinanceRepo) InsertCapital(ctx context.Context, tx *domain.CapitalTransaction) error {
	err := r.db.QueryRow(ctx, `
		INSERT INTO capital_transactions (id, property_id, owner_user_id, kind, amount_paise, purpose, reference, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`,
		tx.ID, tx.PropertyID, tx.OwnerUserID, tx.Kind, tx.AmountPaise, tx.Purpose, tx.Reference, tx.IdempotencyKey, tx.OccurredAt,
	).Scan(&tx.CreatedAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) ListCapital(ctx context.Context, propertyID uuid.UUID) ([]domain.CapitalTransaction, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, owner_user_id, kind, amount_paise, COALESCE(purpose,''), reference, occurred_at, created_at
		FROM capital_transactions WHERE property_id=$1 ORDER BY occurred_at`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CapitalTransaction
	for rows.Next() {
		var c domain.CapitalTransaction
		if err := rows.Scan(&c.ID, &c.PropertyID, &c.OwnerUserID, &c.Kind, &c.AmountPaise, &c.Purpose, &c.Reference, &c.OccurredAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) CountCapital(ctx context.Context, propertyID uuid.UUID) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM capital_transactions WHERE property_id=$1`, propertyID).Scan(&n)
	return n, err
}

func (r *FinanceRepo) InsertExpense(ctx context.Context, e *domain.Expense) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO expenses (id, property_id, category_code, vendor_name, description, amount_paise, status, emergency, room_id, created_by, created_by_role, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		e.ID, e.PropertyID, e.CategoryCode, nullIfEmpty(e.VendorName), nullIfEmpty(e.Description), e.AmountPaise, e.Status, e.Emergency, e.RoomID, e.CreatedBy, e.CreatedByRole, e.IdempotencyKey, e.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) GetExpense(ctx context.Context, id uuid.UUID) (*domain.Expense, error) {
	e := &domain.Expense{}
	var vendor, desc *string
	err := r.db.QueryRow(ctx, `
		SELECT id, property_id, category_code, vendor_name, description, amount_paise, status, emergency, room_id, created_by, created_by_role, occurred_at, created_at
		FROM expenses WHERE id=$1`, id).Scan(
		&e.ID, &e.PropertyID, &e.CategoryCode, &vendor, &desc, &e.AmountPaise, &e.Status, &e.Emergency, &e.RoomID, &e.CreatedBy, &e.CreatedByRole, &e.OccurredAt, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if vendor != nil {
		e.VendorName = *vendor
	}
	if desc != nil {
		e.Description = *desc
	}
	return e, err
}

func (r *FinanceRepo) ListExpenses(ctx context.Context, propertyID uuid.UUID) ([]domain.Expense, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, category_code, COALESCE(vendor_name,''), COALESCE(description,''), amount_paise, status, emergency, room_id, created_by, created_by_role, occurred_at, created_at
		FROM expenses WHERE property_id=$1 ORDER BY occurred_at DESC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Expense
	for rows.Next() {
		var e domain.Expense
		if err := rows.Scan(&e.ID, &e.PropertyID, &e.CategoryCode, &e.VendorName, &e.Description, &e.AmountPaise, &e.Status, &e.Emergency, &e.RoomID, &e.CreatedBy, &e.CreatedByRole, &e.OccurredAt, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) UpdateExpenseStatus(ctx context.Context, id uuid.UUID, status domain.ExpenseStatus) error {
	_, err := r.db.Exec(ctx, `UPDATE expenses SET status=$2 WHERE id=$1`, id, status)
	return err
}

func (r *FinanceRepo) InsertExpensePayment(ctx context.Context, p *domain.ExpensePayment) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO expense_payments (id, expense_id, property_id, amount_paise, payer_role, payer_user_id, method, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		p.ID, p.ExpenseID, p.PropertyID, p.AmountPaise, p.PayerRole, p.PayerUserID, p.Method, p.IdempotencyKey, p.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) ListExpensePayments(ctx context.Context, expenseID uuid.UUID) ([]domain.ExpensePayment, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, expense_id, property_id, amount_paise, payer_role, payer_user_id, method, occurred_at
		FROM expense_payments WHERE expense_id=$1 ORDER BY occurred_at`, expenseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ExpensePayment
	for rows.Next() {
		var p domain.ExpensePayment
		if err := rows.Scan(&p.ID, &p.ExpenseID, &p.PropertyID, &p.AmountPaise, &p.PayerRole, &p.PayerUserID, &p.Method, &p.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) SumExpensePayments(ctx context.Context, expenseID uuid.UUID) (int64, error) {
	var s int64
	err := r.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM expense_payments WHERE expense_id=$1`, expenseID).Scan(&s)
	return s, err
}

func (r *FinanceRepo) SumManagerSpend(ctx context.Context, propertyID, managerID uuid.UUID, from, to time.Time) (int64, error) {
	var s int64
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_paise),0) FROM expense_payments
		WHERE property_id=$1 AND payer_user_id=$2 AND payer_role='manager' AND occurred_at >= $3 AND occurred_at < $4`,
		propertyID, managerID, from, to).Scan(&s)
	return s, err
}

func (r *FinanceRepo) InsertAdvance(ctx context.Context, a *domain.ManagerAdvance) error {
	key := a.ExpensePaymentID.String()
	_, err := r.db.Exec(ctx, `
		INSERT INTO manager_advances (id, property_id, manager_user_id, expense_payment_id, amount_paise, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		a.ID, a.PropertyID, a.ManagerUserID, a.ExpensePaymentID, a.AmountPaise, key, a.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) InsertReimbursement(ctx context.Context, rm *domain.ManagerReimbursement) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO manager_reimbursements (id, property_id, manager_user_id, amount_paise, recorded_by, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		rm.ID, rm.PropertyID, rm.ManagerUserID, rm.AmountPaise, rm.RecordedBy, rm.IdempotencyKey, rm.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) AdvanceOutstanding(ctx context.Context, propertyID uuid.UUID) (int64, error) {
	var adv, re int64
	if err := r.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_advances WHERE property_id=$1`, propertyID).Scan(&adv); err != nil {
		return 0, err
	}
	if err := r.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_reimbursements WHERE property_id=$1`, propertyID).Scan(&re); err != nil {
		return 0, err
	}
	return adv - re, nil
}

func (r *FinanceRepo) ListAdvances(ctx context.Context, propertyID uuid.UUID) ([]domain.ManagerAdvance, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, manager_user_id, expense_payment_id, amount_paise, occurred_at
		FROM manager_advances WHERE property_id=$1 ORDER BY occurred_at DESC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ManagerAdvance
	for rows.Next() {
		var a domain.ManagerAdvance
		if err := rows.Scan(&a.ID, &a.PropertyID, &a.ManagerUserID, &a.ExpensePaymentID, &a.AmountPaise, &a.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) InsertJournal(ctx context.Context, lines []domain.JournalLine) error {
	if len(lines) == 0 {
		return nil
	}

	execLines := func(execer DBTX) error {
		for _, l := range lines {
			_, err := execer.Exec(ctx, `
				INSERT INTO financial_journal_entries (id, property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				l.ID, l.PropertyID, l.AccountCode, l.DebitPaise, l.CreditPaise, l.SourceType, l.SourceID, l.LineKind, l.OccurredAt)
			if isUnique(err) {
				return domain.ErrDuplicateIdempotency
			}
			if err != nil {
				return err
			}
		}
		return nil
	}

	if pool, ok := r.db.(*pgxpool.Pool); ok {
		return WithinTx(ctx, pool, func(tx pgx.Tx) error {
			return execLines(tx)
		})
	}
	return execLines(r.db)
}

func (r *FinanceRepo) ListJournal(ctx context.Context, propertyID uuid.UUID, from, to time.Time, account string) ([]domain.JournalLine, error) {
	q := `SELECT id, property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at
		FROM financial_journal_entries WHERE property_id=$1`
	args := []any{propertyID}
	n := 2
	if !from.IsZero() {
		q += ` AND occurred_at >= $` + itoa(n)
		args = append(args, from)
		n++
	}
	if !to.IsZero() {
		q += ` AND occurred_at < $` + itoa(n)
		args = append(args, to)
		n++
	}
	if account != "" {
		q += ` AND account_code=$` + itoa(n)
		args = append(args, account)
	}
	q += ` ORDER BY occurred_at`
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.JournalLine
	for rows.Next() {
		var l domain.JournalLine
		if err := rows.Scan(&l.ID, &l.PropertyID, &l.AccountCode, &l.DebitPaise, &l.CreditPaise, &l.SourceType, &l.SourceID, &l.LineKind, &l.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) SumAccount(ctx context.Context, propertyID uuid.UUID, account string, from, to time.Time) (int64, int64, error) {
	var d, c int64
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_paise),0), COALESCE(SUM(credit_paise),0)
		FROM financial_journal_entries
		WHERE property_id=$1 AND account_code=$2 AND occurred_at >= $3 AND occurred_at < $4`,
		propertyID, account, from, to).Scan(&d, &c)
	return d, c, err
}

func (r *FinanceRepo) SumAccountNetCredit(ctx context.Context, propertyID uuid.UUID, account string, from, to time.Time) (int64, error) {
	d, c, err := r.SumAccount(ctx, propertyID, account, from, to)
	return c - d, err
}

func (r *FinanceRepo) UpsertBudget(ctx context.Context, b *domain.Budget) error {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	err := r.db.QueryRow(ctx, `
		INSERT INTO budgets (id, property_id, category_code, period_month, amount_paise)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (property_id, category_code, period_month) DO UPDATE SET amount_paise=EXCLUDED.amount_paise, updated_at=NOW()
		RETURNING id`, b.ID, b.PropertyID, b.CategoryCode, b.PeriodMonth, b.AmountPaise).Scan(&b.ID)
	return err
}

func (r *FinanceRepo) GetBudget(ctx context.Context, propertyID uuid.UUID, category, period string) (*domain.Budget, error) {
	b := &domain.Budget{}
	err := r.db.QueryRow(ctx, `
		SELECT id, property_id, category_code, period_month, amount_paise FROM budgets
		WHERE property_id=$1 AND category_code=$2 AND period_month=$3`, propertyID, category, period).
		Scan(&b.ID, &b.PropertyID, &b.CategoryCode, &b.PeriodMonth, &b.AmountPaise)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return b, err
}

func (r *FinanceRepo) ListBudgets(ctx context.Context, propertyID uuid.UUID, period string) ([]domain.Budget, error) {
	rows, err := r.db.Query(ctx, `SELECT id, property_id, category_code, period_month, amount_paise FROM budgets WHERE property_id=$1 AND ($2='' OR period_month=$2)`, propertyID, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Budget
	for rows.Next() {
		var b domain.Budget
		if err := rows.Scan(&b.ID, &b.PropertyID, &b.CategoryCode, &b.PeriodMonth, &b.AmountPaise); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) InsertRewardLiability(ctx context.Context, t *domain.RewardLiabilityTxn) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO reward_liability_transactions (id, property_id, tenant_id, kind, points, amount_paise, source_type, source_id, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		t.ID, t.PropertyID, t.TenantID, t.Kind, t.Points, t.AmountPaise, t.SourceType, t.SourceID, t.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) SumRewardLiability(ctx context.Context, propertyID uuid.UUID, kind string, from, to time.Time) (int64, error) {
	var s int64
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_paise),0) FROM reward_liability_transactions
		WHERE property_id=$1 AND ($2='' OR kind=$2) AND occurred_at >= $3 AND occurred_at < $4`,
		propertyID, kind, from, to).Scan(&s)
	return s, err
}

func (r *FinanceRepo) SumRewardPointsIssued(ctx context.Context, propertyID uuid.UUID, from, to time.Time) (int, error) {
	var s int
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(points),0) FROM reward_liability_transactions
		WHERE property_id=$1 AND kind='issued' AND occurred_at >= $2 AND occurred_at < $3`,
		propertyID, from, to).Scan(&s)
	return s, err
}

func (r *FinanceRepo) GetTieOut(ctx context.Context, propertyID uuid.UUID, period string) (*domain.PeriodTieOut, error) {
	t := &domain.PeriodTieOut{}
	var raw []byte
	err := r.db.QueryRow(ctx, `
		SELECT id, property_id, period_month, recon_total_paise, ledger_total_paise, difference_paise, bridge_json, status, closed_at
		FROM period_tie_outs WHERE property_id=$1 AND period_month=$2`, propertyID, period).
		Scan(&t.ID, &t.PropertyID, &t.PeriodMonth, &t.ReconTotalPaise, &t.LedgerTotalPaise, &t.DifferencePaise, &raw, &t.Status, &t.ClosedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(raw, &t.Items)
	return t, nil
}

func (r *FinanceRepo) SaveTieOut(ctx context.Context, t *domain.PeriodTieOut) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	b, _ := json.Marshal(t.Items)
	_, err := r.db.Exec(ctx, `
		INSERT INTO period_tie_outs (id, property_id, period_month, recon_total_paise, ledger_total_paise, difference_paise, bridge_json, status, closed_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NOW())
		ON CONFLICT (property_id, period_month) DO UPDATE SET
			recon_total_paise=EXCLUDED.recon_total_paise,
			ledger_total_paise=EXCLUDED.ledger_total_paise,
			difference_paise=EXCLUDED.difference_paise,
			bridge_json=EXCLUDED.bridge_json,
			status=EXCLUDED.status,
			closed_at=EXCLUDED.closed_at,
			updated_at=NOW()`,
		t.ID, t.PropertyID, t.PeriodMonth, t.ReconTotalPaise, t.LedgerTotalPaise, t.DifferencePaise, b, t.Status, t.ClosedAt)
	return err
}

func (r *FinanceRepo) ListTieOuts(ctx context.Context, propertyID uuid.UUID, limit int) ([]domain.PeriodTieOut, error) {
	if limit <= 0 {
		limit = 12
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, period_month, recon_total_paise, ledger_total_paise, difference_paise, bridge_json, status, closed_at
		FROM period_tie_outs WHERE property_id=$1 ORDER BY period_month DESC LIMIT $2`, propertyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PeriodTieOut
	for rows.Next() {
		var t domain.PeriodTieOut
		var raw []byte
		if err := rows.Scan(&t.ID, &t.PropertyID, &t.PeriodMonth, &t.ReconTotalPaise, &t.LedgerTotalPaise, &t.DifferencePaise, &raw, &t.Status, &t.ClosedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &t.Items)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) InsertApproval(ctx context.Context, a *domain.ApprovalRequest) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO approval_requests (id, property_id, kind, subject_id, amount_paise, requested_by, status, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		a.ID, a.PropertyID, a.Kind, a.SubjectID, a.AmountPaise, a.RequestedBy, a.Status, nullIfEmpty(a.Note))
	return err
}

func (r *FinanceRepo) GetApproval(ctx context.Context, id uuid.UUID) (*domain.ApprovalRequest, error) {
	a := &domain.ApprovalRequest{}
	var note *string
	err := r.db.QueryRow(ctx, `
		SELECT id, property_id, kind, subject_id, amount_paise, requested_by, status, decided_by, decided_at, note, created_at
		FROM approval_requests WHERE id=$1`, id).Scan(
		&a.ID, &a.PropertyID, &a.Kind, &a.SubjectID, &a.AmountPaise, &a.RequestedBy, &a.Status, &a.DecidedBy, &a.DecidedAt, &note, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if note != nil {
		a.Note = *note
	}
	return a, err
}

func (r *FinanceRepo) ListApprovals(ctx context.Context, propertyID uuid.UUID, status string) ([]domain.ApprovalRequest, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, kind, subject_id, amount_paise, requested_by, status, decided_by, decided_at, COALESCE(note,''), created_at
		FROM approval_requests WHERE property_id=$1 AND ($2='' OR status=$2) ORDER BY created_at DESC`, propertyID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ApprovalRequest
	for rows.Next() {
		var a domain.ApprovalRequest
		if err := rows.Scan(&a.ID, &a.PropertyID, &a.Kind, &a.SubjectID, &a.AmountPaise, &a.RequestedBy, &a.Status, &a.DecidedBy, &a.DecidedAt, &a.Note, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) UpdateApproval(ctx context.Context, a *domain.ApprovalRequest) error {
	_, err := r.db.Exec(ctx, `
		UPDATE approval_requests SET status=$2, decided_by=$3, decided_at=$4, note=$5 WHERE id=$1`,
		a.ID, a.Status, a.DecidedBy, a.DecidedAt, nullIfEmpty(a.Note))
	return err
}

func (r *FinanceRepo) InsertKPI(ctx context.Context, s *domain.KPISnapshot) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO kpi_snapshots (property_id, period_month, snapshot_date, occupancy_bps, occupied_beds, capacity_beds,
			contribution_per_bed_paise, opex_paise, ocf_paise, leakage_total_paise, variance_bridge_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (property_id, snapshot_date) DO UPDATE SET
			occupancy_bps=EXCLUDED.occupancy_bps, occupied_beds=EXCLUDED.occupied_beds, capacity_beds=EXCLUDED.capacity_beds,
			contribution_per_bed_paise=EXCLUDED.contribution_per_bed_paise, opex_paise=EXCLUDED.opex_paise, ocf_paise=EXCLUDED.ocf_paise,
			leakage_total_paise=EXCLUDED.leakage_total_paise`,
		s.PropertyID, s.PeriodMonth, s.SnapshotDate, s.OccupancyBPS, s.OccupiedBeds, s.CapacityBeds,
		s.ContributionPerBedPaise, s.OpexPaise, s.OCFPaise, s.LeakageTotalPaise, nullJSON(s.VarianceBridge))
	return err
}

func (r *FinanceRepo) InsertROI(ctx context.Context, s *domain.ROISnapshot) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO roi_snapshots (property_id, period_month, snapshot_date, capital_invested_paise, capital_recovered_paise,
			unrecovered_paise, tbe_months_milli, break_even_occupancy_bps, official)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (property_id, snapshot_date) DO UPDATE SET
			capital_invested_paise=EXCLUDED.capital_invested_paise, capital_recovered_paise=EXCLUDED.capital_recovered_paise,
			unrecovered_paise=EXCLUDED.unrecovered_paise, tbe_months_milli=EXCLUDED.tbe_months_milli,
			break_even_occupancy_bps=EXCLUDED.break_even_occupancy_bps, official=EXCLUDED.official`,
		s.PropertyID, s.PeriodMonth, s.SnapshotDate, s.CapitalInvestedPaise, s.CapitalRecoveredPaise,
		s.UnrecoveredPaise, s.TBEMonthsMilli, s.BreakEvenOccupancyBPS, s.Official)
	return err
}

func (r *FinanceRepo) LatestROI(ctx context.Context, propertyID uuid.UUID) (*domain.ROISnapshot, error) {
	s := &domain.ROISnapshot{}
	err := r.db.QueryRow(ctx, `
		SELECT property_id, period_month, snapshot_date, capital_invested_paise, capital_recovered_paise, unrecovered_paise,
			tbe_months_milli, break_even_occupancy_bps, official
		FROM roi_snapshots WHERE property_id=$1 ORDER BY snapshot_date DESC LIMIT 1`, propertyID).
		Scan(&s.PropertyID, &s.PeriodMonth, &s.SnapshotDate, &s.CapitalInvestedPaise, &s.CapitalRecoveredPaise, &s.UnrecoveredPaise,
			&s.TBEMonthsMilli, &s.BreakEvenOccupancyBPS, &s.Official)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return s, err
}

func (r *FinanceRepo) InsertLeakage(ctx context.Context, e *domain.LeakageEvent) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO leakage_events (id, property_id, category, estimated_paise, confidence_bps, evidence_json, severity, status, detected_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.ID, e.PropertyID, e.Category, e.EstimatedPaise, e.ConfidenceBPS, nullJSON(e.Evidence), e.Severity, e.Status, e.DetectedAt)
	return err
}

func (r *FinanceRepo) ListLeakage(ctx context.Context, propertyID uuid.UUID) ([]domain.LeakageEvent, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, category, estimated_paise, confidence_bps, evidence_json, severity, status, detected_at
		FROM leakage_events WHERE property_id=$1 ORDER BY detected_at DESC LIMIT 100`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.LeakageEvent
	for rows.Next() {
		var e domain.LeakageEvent
		if err := rows.Scan(&e.ID, &e.PropertyID, &e.Category, &e.EstimatedPaise, &e.ConfidenceBPS, &e.Evidence, &e.Severity, &e.Status, &e.DetectedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) GetLeakage(ctx context.Context, id uuid.UUID) (*domain.LeakageEvent, error) {
	e := &domain.LeakageEvent{}
	err := r.db.QueryRow(ctx, `
		SELECT id, property_id, category, estimated_paise, confidence_bps, evidence_json, severity, status, detected_at
		FROM leakage_events WHERE id=$1`, id).Scan(
		&e.ID, &e.PropertyID, &e.Category, &e.EstimatedPaise, &e.ConfidenceBPS, &e.Evidence, &e.Severity, &e.Status, &e.DetectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return e, err
}

func (r *FinanceRepo) InsertRecommendation(ctx context.Context, rec *domain.Recommendation) error {
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO recommendations (id, property_id, leakage_event_id, issue, suggested_action, expected_savings_paise,
			effort, risk, confidence_bps, safety_ok, quality_ok, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		rec.ID, rec.PropertyID, rec.LeakageEventID, rec.Issue, rec.SuggestedAction, rec.ExpectedSavingsPaise,
		rec.Effort, rec.Risk, rec.ConfidenceBPS, rec.SafetyOK, rec.QualityOK, rec.Status)
	return err
}

func (r *FinanceRepo) ListRecommendations(ctx context.Context, propertyID uuid.UUID) ([]domain.Recommendation, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, leakage_event_id, issue, suggested_action, expected_savings_paise, effort, risk,
			confidence_bps, safety_ok, quality_ok, status, realized_savings_paise, created_at
		FROM recommendations WHERE property_id=$1 ORDER BY created_at DESC LIMIT 100`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Recommendation
	for rows.Next() {
		var rec domain.Recommendation
		if err := rows.Scan(&rec.ID, &rec.PropertyID, &rec.LeakageEventID, &rec.Issue, &rec.SuggestedAction, &rec.ExpectedSavingsPaise,
			&rec.Effort, &rec.Risk, &rec.ConfidenceBPS, &rec.SafetyOK, &rec.QualityOK, &rec.Status, &rec.RealizedSavingsPaise, &rec.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) GetRecommendation(ctx context.Context, id uuid.UUID) (*domain.Recommendation, error) {
	rec := &domain.Recommendation{}
	err := r.db.QueryRow(ctx, `
		SELECT id, property_id, leakage_event_id, issue, suggested_action, expected_savings_paise, effort, risk,
			confidence_bps, safety_ok, quality_ok, status, realized_savings_paise, created_at
		FROM recommendations WHERE id=$1`, id).Scan(
		&rec.ID, &rec.PropertyID, &rec.LeakageEventID, &rec.Issue, &rec.SuggestedAction, &rec.ExpectedSavingsPaise,
		&rec.Effort, &rec.Risk, &rec.ConfidenceBPS, &rec.SafetyOK, &rec.QualityOK, &rec.Status, &rec.RealizedSavingsPaise, &rec.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return rec, err
}

func (r *FinanceRepo) UpdateRecommendation(ctx context.Context, rec *domain.Recommendation) error {
	_, err := r.db.Exec(ctx, `
		UPDATE recommendations SET status=$2, realized_savings_paise=$3, accepted_at=$4, completed_at=$5 WHERE id=$1`,
		rec.ID, rec.Status, rec.RealizedSavingsPaise, rec.AcceptedAt, rec.CompletedAt)
	return err
}

func (r *FinanceRepo) InsertForecast(ctx context.Context, f *domain.ForecastSnapshot) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO forecast_snapshots (property_id, horizon_days, as_of, payload_json)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (property_id, horizon_days, as_of) DO UPDATE SET payload_json=EXCLUDED.payload_json`,
		f.PropertyID, f.HorizonDays, f.AsOf, nullJSON(f.Payload))
	return err
}

func (r *FinanceRepo) LatestForecast(ctx context.Context, propertyID uuid.UUID, horizon int) (*domain.ForecastSnapshot, error) {
	f := &domain.ForecastSnapshot{}
	err := r.db.QueryRow(ctx, `
		SELECT property_id, horizon_days, as_of, payload_json FROM forecast_snapshots
		WHERE property_id=$1 AND horizon_days=$2 ORDER BY as_of DESC LIMIT 1`, propertyID, horizon).
		Scan(&f.PropertyID, &f.HorizonDays, &f.AsOf, &f.Payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return f, err
}

func (r *FinanceRepo) UpsertImportSuggestion(ctx context.Context, s *domain.ExpenseImportSuggestion) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO expense_import_suggestions (id, property_id, txn_id, amount_paise, txn_date, note, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (property_id, txn_id) DO UPDATE SET amount_paise=EXCLUDED.amount_paise, note=EXCLUDED.note`,
		s.ID, s.PropertyID, s.TxnID, s.AmountPaise, s.TxnDate, s.Note, s.Status)
	return err
}

func (r *FinanceRepo) ListImportSuggestions(ctx context.Context, propertyID uuid.UUID) ([]domain.ExpenseImportSuggestion, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, txn_id, amount_paise, txn_date, COALESCE(note,''), status
		FROM expense_import_suggestions WHERE property_id=$1 AND status='pending'`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ExpenseImportSuggestion
	for rows.Next() {
		var s domain.ExpenseImportSuggestion
		if err := rows.Scan(&s.ID, &s.PropertyID, &s.TxnID, &s.AmountPaise, &s.TxnDate, &s.Note, &s.Status); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *FinanceRepo) UpsertMealPrep(ctx context.Context, m *domain.MealPrepActual) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO meal_prep_actuals (property_id, meal_date, meal_slot, prepared_count, discarded_count)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (property_id, meal_date, meal_slot) DO UPDATE SET prepared_count=EXCLUDED.prepared_count, discarded_count=EXCLUDED.discarded_count`,
		m.PropertyID, m.MealDate, m.MealSlot, m.PreparedCount, m.DiscardedCount)
	return err
}

func (r *FinanceRepo) GetMealPrep(ctx context.Context, propertyID uuid.UUID, date time.Time) ([]domain.MealPrepActual, error) {
	rows, err := r.db.Query(ctx, `
		SELECT property_id, meal_date, meal_slot, prepared_count, discarded_count
		FROM meal_prep_actuals WHERE property_id=$1 AND meal_date=$2`, propertyID, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MealPrepActual
	for rows.Next() {
		var m domain.MealPrepActual
		if err := rows.Scan(&m.PropertyID, &m.MealDate, &m.MealSlot, &m.PreparedCount, &m.DiscardedCount); err != nil {
			return nil, err
		}
		out = append(out, m)
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
