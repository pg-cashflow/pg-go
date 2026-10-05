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

type FinanceRepo struct {
	pool *pgxpool.Pool
}

func NewFinanceRepo(pool *pgxpool.Pool) *FinanceRepo {
	return &FinanceRepo{
		pool: pool,
	}
}

func isUnique(err error) bool {
	var e *pgconn.PgError
	return errors.As(err, &e) && e.Code == "23505"
}

// mapLedgerPgErr translates the closed-period SQLSTATEs raised by the ledger triggers
// (migration 044) into domain.ErrPeriodClosed: LG001 = posting into a closed period,
// LG004 = closed period tie-out is frozen. Other errors pass through unchanged.
func mapLedgerPgErr(err error) error {
	var e *pgconn.PgError
	if errors.As(err, &e) && (e.Code == "LG001" || e.Code == "LG004") {
		return fmt.Errorf("%w: %s", domain.ErrPeriodClosed, e.Message)
	}
	return err
}

func (r *FinanceRepo) EnsureDefaults(ctx context.Context, propertyID uuid.UUID) error {
	if r.pool == nil {
		return fmt.Errorf("EnsureDefaults requires pool access, got nil pool")
	}
	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO property_finance_settings (property_id) VALUES ($1) ON CONFLICT DO NOTHING`, propertyID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO approval_policies (property_id) VALUES ($1) ON CONFLICT DO NOTHING`, propertyID)
		return err
	})
}

func (r *FinanceRepo) GetPolicy(ctx context.Context, propertyID uuid.UUID) (domain.ApprovalPolicy, error) {
	_ = r.EnsureDefaults(ctx, propertyID)
	var p domain.ApprovalPolicy
	p.PropertyID = propertyID
	err := r.pool.QueryRow(ctx, `
		SELECT manager_daily_limit_paise, single_expense_limit_paise, manager_monthly_limit_paise,
		       owner_approval_threshold_paise, reimbursement_threshold_paise, emergency_bypass_enabled
		FROM approval_policies WHERE property_id=$1`, propertyID).Scan(
		&p.ManagerDailyLimitPaise, &p.SingleExpenseLimitPaise, &p.ManagerMonthlyLimitPaise,
		&p.OwnerApprovalThresholdPaise, &p.ReimbursementThresholdPaise, &p.EmergencyBypassEnabled)
	return p, err
}

func (r *FinanceRepo) SavePolicy(ctx context.Context, p domain.ApprovalPolicy) error {
	_, err := r.pool.Exec(ctx, `
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
	err := r.pool.QueryRow(ctx, `
		SELECT fiscal_month_start_day, manager_can_view_capital, manager_can_view_roi, manager_can_view_leakage,
		       tdr_effective_bps, tdr_is_estimated
		FROM property_finance_settings WHERE property_id=$1`, propertyID).Scan(
		&s.FiscalMonthStartDay, &s.ManagerCanViewCapital, &s.ManagerCanViewROI, &s.ManagerCanViewLeakage,
		&s.TDREffectiveBPS, &s.TDRIsEstimated)
	return s, err
}

func (r *FinanceRepo) SaveSettings(ctx context.Context, s domain.PropertyFinanceSettings) error {
	_, err := r.pool.Exec(ctx, `
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
	if r.pool == nil {
		return fmt.Errorf("SaveUnifiedSettings requires transactional pool access, got nil pool")
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

	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		return exec(tx)
	})
}

func (r *FinanceRepo) InsertCapital(ctx context.Context, tx *domain.CapitalTransaction) error {
	err := r.pool.QueryRow(ctx, `
		INSERT INTO capital_transactions (id, property_id, owner_user_id, kind, amount_paise, purpose, reference, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`,
		tx.ID, tx.PropertyID, tx.OwnerUserID, tx.Kind, tx.AmountPaise, tx.Purpose, tx.Reference, tx.IdempotencyKey, tx.OccurredAt,
	).Scan(&tx.CreatedAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) InsertCapitalAtomic(ctx context.Context, txRecord *domain.CapitalTransaction, lines []domain.JournalLine) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `
		INSERT INTO capital_transactions (id, property_id, owner_user_id, kind, amount_paise, purpose, reference, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`,
		txRecord.ID, txRecord.PropertyID, txRecord.OwnerUserID, txRecord.Kind, txRecord.AmountPaise, txRecord.Purpose, txRecord.Reference, txRecord.IdempotencyKey, txRecord.OccurredAt,
	).Scan(&txRecord.CreatedAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	if err != nil {
		return fmt.Errorf("insert capital: %w", err)
	}

	for _, l := range lines {
		_, err = tx.Exec(ctx, `
			INSERT INTO financial_journal_entries (id, property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			l.ID, l.PropertyID, l.AccountCode, l.DebitPaise, l.CreditPaise, l.SourceType, l.SourceID, l.LineKind, l.OccurredAt)
		if isUnique(err) {
			return domain.ErrDuplicateIdempotency
		}
		if err != nil {
			return fmt.Errorf("insert journal line: %w", err)
		}
	}

	return tx.Commit(ctx)
}

func (r *FinanceRepo) ListCapital(ctx context.Context, propertyID uuid.UUID) ([]domain.CapitalTransaction, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, owner_user_id, kind, amount_paise, COALESCE(purpose,''), reference, occurred_at, created_at
		FROM capital_transactions WHERE property_id=$1 ORDER BY occurred_at DESC LIMIT 200`, propertyID)
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
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM capital_transactions WHERE property_id=$1`, propertyID).Scan(&n)
	return n, err
}

func (r *FinanceRepo) GetPropertyOwnerUserID(ctx context.Context, propertyID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT id FROM users
		WHERE property_id=$1 AND role='owner'
		ORDER BY created_at ASC
		LIMIT 1`, propertyID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, domain.ErrNotFound
		}
		return uuid.Nil, err
	}
	return id, nil
}

func (r *FinanceRepo) InsertExpense(ctx context.Context, e *domain.Expense) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO expenses (id, property_id, category_code, vendor_name, description, amount_paise, status, emergency, room_id, created_by, created_by_role, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		e.ID, e.PropertyID, e.CategoryCode, nullIfEmpty(e.VendorName), nullIfEmpty(e.Description), e.AmountPaise, e.Status, e.Emergency, e.RoomID, e.CreatedBy, e.CreatedByRole, e.IdempotencyKey, e.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) InsertExpenseAtomic(ctx context.Context, e *domain.Expense, lines []domain.JournalLine, approval *domain.ApprovalRequest) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if e.CreatedByRole == string(domain.RoleManager) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(format('manager-spend:%s:%s', $1::text, $2::text)))`, e.PropertyID, e.CreatedBy); err != nil {
			return fmt.Errorf("lock manager spend: %w", err)
		}
	}

	if e.RoomID != nil {
		var roomPropID uuid.UUID
		err = tx.QueryRow(ctx, `SELECT property_id FROM rooms WHERE id=$1`, *e.RoomID).Scan(&roomPropID)
		if errors.Is(err, pgx.ErrNoRows) || roomPropID != e.PropertyID {
			return fmt.Errorf("room does not belong to property: %w", domain.ErrForbidden)
		}
		if err != nil {
			return err
		}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO expenses (id, property_id, category_code, vendor_name, description, amount_paise, status, emergency, room_id, created_by, created_by_role, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		e.ID, e.PropertyID, e.CategoryCode, nullIfEmpty(e.VendorName), nullIfEmpty(e.Description), e.AmountPaise, e.Status, e.Emergency, e.RoomID, e.CreatedBy, e.CreatedByRole, e.IdempotencyKey, e.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	if err != nil {
		return fmt.Errorf("insert expense: %w", err)
	}

	if approval != nil {
		_, err = tx.Exec(ctx, `
			INSERT INTO approval_requests (id, property_id, kind, subject_id, amount_paise, requested_by, status, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			approval.ID, approval.PropertyID, approval.Kind, approval.SubjectID, approval.AmountPaise, approval.RequestedBy, approval.Status, approval.CreatedAt)
		if isUnique(err) {
			return domain.ErrDuplicateIdempotency
		}
		if err != nil {
			return fmt.Errorf("insert approval: %w", err)
		}
	}

	for _, l := range lines {
		_, err = tx.Exec(ctx, `
			INSERT INTO financial_journal_entries (id, property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			l.ID, l.PropertyID, l.AccountCode, l.DebitPaise, l.CreditPaise, l.SourceType, l.SourceID, l.LineKind, l.OccurredAt)
		if isUnique(err) {
			return domain.ErrDuplicateIdempotency
		}
		if err != nil {
			return fmt.Errorf("insert journal line: %w", err)
		}
	}

	return tx.Commit(ctx)
}

func (r *FinanceRepo) RecordExpensePaymentAtomic(ctx context.Context, p *domain.ExpensePayment, lines []domain.JournalLine, adv *domain.ManagerAdvance) (*domain.Expense, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if p.PayerRole == domain.PayerManager {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(format('manager-spend:%s:%s', $1::text, $2::text)))`, p.PropertyID, p.PayerUserID); err != nil {
			return nil, fmt.Errorf("lock manager spend: %w", err)
		}
	}

	var e domain.Expense
	var vendor, desc *string
	err = tx.QueryRow(ctx, `
		SELECT id, property_id, category_code, vendor_name, description, amount_paise, status, emergency, room_id, created_by, created_by_role, occurred_at, created_at
		FROM expenses WHERE id=$1 FOR UPDATE`, p.ExpenseID).Scan(
		&e.ID, &e.PropertyID, &e.CategoryCode, &vendor, &desc, &e.AmountPaise, &e.Status, &e.Emergency, &e.RoomID, &e.CreatedBy, &e.CreatedByRole, &e.OccurredAt, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if vendor != nil {
		e.VendorName = *vendor
	}
	if desc != nil {
		e.Description = *desc
	}
	if e.PropertyID != p.PropertyID {
		return nil, domain.ErrForbidden
	}
	if e.Status != domain.ExpenseApproved && e.Status != domain.ExpensePaid {
		return nil, domain.ErrExpenseNotPayable
	}

	var paid int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise), 0) FROM expense_payments WHERE expense_id=$1`, p.ExpenseID).Scan(&paid)
	if err != nil {
		return nil, fmt.Errorf("sum expense payments: %w", err)
	}
	if paid+p.AmountPaise > e.AmountPaise {
		return nil, domain.ErrOverpay
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO expense_payments (id, expense_id, property_id, amount_paise, payer_role, payer_user_id, method, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		p.ID, p.ExpenseID, p.PropertyID, p.AmountPaise, p.PayerRole, p.PayerUserID, p.Method, p.IdempotencyKey, p.OccurredAt)
	if isUnique(err) {
		return nil, domain.ErrDuplicateIdempotency
	}
	if err != nil {
		return nil, fmt.Errorf("insert expense payment: %w", err)
	}

	if adv != nil {
		_, err = tx.Exec(ctx, `
			INSERT INTO manager_advances (id, property_id, manager_user_id, expense_payment_id, amount_paise, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			adv.ID, adv.PropertyID, adv.ManagerUserID, adv.ExpensePaymentID, adv.AmountPaise, adv.OccurredAt)
		if isUnique(err) {
			return nil, domain.ErrDuplicateIdempotency
		}
		if err != nil {
			return nil, fmt.Errorf("insert advance: %w", err)
		}
	}

	for _, l := range lines {
		_, err = tx.Exec(ctx, `
			INSERT INTO financial_journal_entries (id, property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			l.ID, l.PropertyID, l.AccountCode, l.DebitPaise, l.CreditPaise, l.SourceType, l.SourceID, l.LineKind, l.OccurredAt)
		if isUnique(err) {
			return nil, domain.ErrDuplicateIdempotency
		}
		if err != nil {
			return nil, fmt.Errorf("insert journal line: %w", err)
		}
	}

	if paid+p.AmountPaise == e.AmountPaise {
		e.Status = domain.ExpensePaid
		_, err = tx.Exec(ctx, `UPDATE expenses SET status=$2 WHERE id=$1`, e.ID, e.Status)
		if err != nil {
			return nil, fmt.Errorf("update expense status: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &e, nil
}

func (r *FinanceRepo) GetExpense(ctx context.Context, id uuid.UUID) (*domain.Expense, error) {
	e := &domain.Expense{}
	var vendor, desc *string
	err := r.pool.QueryRow(ctx, `
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
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, category_code, COALESCE(vendor_name,''), COALESCE(description,''), amount_paise, status, emergency, room_id, created_by, created_by_role, occurred_at, created_at
		FROM expenses WHERE property_id=$1 ORDER BY occurred_at DESC, id DESC LIMIT 50`, propertyID)
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
	_, err := r.pool.Exec(ctx, `UPDATE expenses SET status=$2 WHERE id=$1`, id, status)
	return err
}

func (r *FinanceRepo) InsertExpensePayment(ctx context.Context, p *domain.ExpensePayment) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO expense_payments (id, expense_id, property_id, amount_paise, payer_role, payer_user_id, method, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		p.ID, p.ExpenseID, p.PropertyID, p.AmountPaise, p.PayerRole, p.PayerUserID, p.Method, p.IdempotencyKey, p.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) ListExpensePayments(ctx context.Context, expenseID uuid.UUID) ([]domain.ExpensePayment, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, expense_id, property_id, amount_paise, payer_role, payer_user_id, method, occurred_at
		FROM expense_payments WHERE expense_id=$1 ORDER BY occurred_at ASC LIMIT 100`, expenseID)
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
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM expense_payments WHERE expense_id=$1`, expenseID).Scan(&s)
	return s, err
}

func (r *FinanceRepo) SumManagerSpend(ctx context.Context, propertyID, managerID uuid.UUID, from, to time.Time) (int64, error) {
	var s int64
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_paise),0) FROM expense_payments
		WHERE property_id=$1 AND payer_user_id=$2 AND payer_role='manager' AND occurred_at >= $3 AND occurred_at < $4`,
		propertyID, managerID, from, to).Scan(&s)
	return s, err
}

func (r *FinanceRepo) InsertAdvance(ctx context.Context, a *domain.ManagerAdvance) error {
	key := a.ExpensePaymentID.String()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO manager_advances (id, property_id, manager_user_id, expense_payment_id, amount_paise, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		a.ID, a.PropertyID, a.ManagerUserID, a.ExpensePaymentID, a.AmountPaise, key, a.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) InsertReimbursement(ctx context.Context, rm *domain.ManagerReimbursement) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO manager_reimbursements (id, property_id, manager_user_id, amount_paise, recorded_by, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		rm.ID, rm.PropertyID, rm.ManagerUserID, rm.AmountPaise, rm.RecordedBy, rm.IdempotencyKey, rm.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	return err
}

func (r *FinanceRepo) InsertReimbursementAtomic(ctx context.Context, rm *domain.ManagerReimbursement, lines []domain.JournalLine) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO manager_reimbursements (id, property_id, manager_user_id, amount_paise, recorded_by, idempotency_key, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		rm.ID, rm.PropertyID, rm.ManagerUserID, rm.AmountPaise, rm.RecordedBy, rm.IdempotencyKey, rm.OccurredAt)
	if isUnique(err) {
		return domain.ErrDuplicateIdempotency
	}
	if err != nil {
		return fmt.Errorf("insert reimbursement: %w", err)
	}

	for _, l := range lines {
		_, err = tx.Exec(ctx, `
			INSERT INTO financial_journal_entries (id, property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			l.ID, l.PropertyID, l.AccountCode, l.DebitPaise, l.CreditPaise, l.SourceType, l.SourceID, l.LineKind, l.OccurredAt)
		if isUnique(err) {
			return domain.ErrDuplicateIdempotency
		}
		if err != nil {
			return fmt.Errorf("insert journal line: %w", err)
		}
	}

	return tx.Commit(ctx)
}

func (r *FinanceRepo) AdvanceOutstanding(ctx context.Context, propertyID uuid.UUID) (int64, error) {
	var adv, re int64
	if err := r.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_advances WHERE property_id=$1`, propertyID).Scan(&adv); err != nil {
		return 0, err
	}
	if err := r.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_reimbursements WHERE property_id=$1`, propertyID).Scan(&re); err != nil {
		return 0, err
	}
	return adv - re, nil
}

func (r *FinanceRepo) ListAdvances(ctx context.Context, propertyID uuid.UUID) ([]domain.ManagerAdvance, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, manager_user_id, expense_payment_id, amount_paise, occurred_at
		FROM manager_advances WHERE property_id=$1 ORDER BY occurred_at DESC LIMIT 200`, propertyID)
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
	if r.pool == nil {
		return fmt.Errorf("InsertJournal requires transactional pool access, got nil pool")
	}

	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		firstLine := lines[0]
		existingRows, err := tx.Query(ctx, `
			SELECT id, line_kind, account_code, debit_paise, credit_paise 
			FROM financial_journal_entries 
			WHERE source_type=$1 AND source_id=$2`, firstLine.SourceType, firstLine.SourceID)
		if err != nil {
			return mapLedgerPgErr(err)
		}
		defer existingRows.Close()

		type lineSummary struct {
			acct   string
			debit  int64
			credit int64
		}
		existingMap := make(map[string]lineSummary)
		for existingRows.Next() {
			var id uuid.UUID
			var lKind, acct string
			var dr, cr int64
			if err := existingRows.Scan(&id, &lKind, &acct, &dr, &cr); err != nil {
				return err
			}
			existingMap[lKind] = lineSummary{acct: acct, debit: dr, credit: cr}
		}
		if err := existingRows.Err(); err != nil {
			return err
		}

		if len(existingMap) > 0 {
			if len(existingMap) != len(lines) {
				return fmt.Errorf("%w: journal source %s has %d existing lines but incoming has %d",
					domain.ErrIdempotencyConflict, firstLine.SourceID, len(existingMap), len(lines))
			}
			for _, l := range lines {
				ex, found := existingMap[l.LineKind]
				if !found || ex.acct != l.AccountCode || ex.debit != l.DebitPaise || ex.credit != l.CreditPaise {
					return fmt.Errorf("%w: journal line %s (%s) has conflicting values (existing %s dr:%d cr:%d vs incoming %s dr:%d cr:%d)",
						domain.ErrIdempotencyConflict, l.ID, l.LineKind, ex.acct, ex.debit, ex.credit, l.AccountCode, l.DebitPaise, l.CreditPaise)
				}
			}
			return domain.ErrDuplicateIdempotency
		}

		for _, l := range lines {
			_, err := tx.Exec(ctx, `
				INSERT INTO financial_journal_entries (id, property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				l.ID, l.PropertyID, l.AccountCode, l.DebitPaise, l.CreditPaise, l.SourceType, l.SourceID, l.LineKind, l.OccurredAt)
			if err != nil {
				if isUnique(err) {
					return domain.ErrIdempotencyConflict
				}
				return mapLedgerPgErr(err)
			}
		}
		return nil
	})
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
	q += ` ORDER BY occurred_at ASC LIMIT 1000`
	rows, err := r.pool.Query(ctx, q, args...)
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
	err := r.pool.QueryRow(ctx, `
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
	err := r.pool.QueryRow(ctx, `
		INSERT INTO budgets (id, property_id, category_code, period_month, amount_paise)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (property_id, category_code, period_month) DO UPDATE SET amount_paise=EXCLUDED.amount_paise, updated_at=NOW()
		RETURNING id`, b.ID, b.PropertyID, b.CategoryCode, b.PeriodMonth, b.AmountPaise).Scan(&b.ID)
	return err
}

func (r *FinanceRepo) GetBudget(ctx context.Context, propertyID uuid.UUID, category, period string) (*domain.Budget, error) {
	b := &domain.Budget{}
	err := r.pool.QueryRow(ctx, `
		SELECT id, property_id, category_code, period_month, amount_paise FROM budgets
		WHERE property_id=$1 AND category_code=$2 AND period_month=$3`, propertyID, category, period).
		Scan(&b.ID, &b.PropertyID, &b.CategoryCode, &b.PeriodMonth, &b.AmountPaise)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return b, err
}

func (r *FinanceRepo) ListBudgets(ctx context.Context, propertyID uuid.UUID, period string) ([]domain.Budget, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, property_id, category_code, period_month, amount_paise FROM budgets WHERE property_id=$1 AND ($2='' OR period_month=$2)`, propertyID, period)
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
	_, err := r.pool.Exec(ctx, `
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
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_paise),0) FROM reward_liability_transactions
		WHERE property_id=$1 AND ($2='' OR kind=$2) AND occurred_at >= $3 AND occurred_at < $4`,
		propertyID, kind, from, to).Scan(&s)
	return s, err
}

func (r *FinanceRepo) SumRewardPointsIssued(ctx context.Context, propertyID uuid.UUID, from, to time.Time) (int, error) {
	var s int
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(points),0) FROM reward_liability_transactions
		WHERE property_id=$1 AND kind='issued' AND occurred_at >= $2 AND occurred_at < $3`,
		propertyID, from, to).Scan(&s)
	return s, err
}

func (r *FinanceRepo) GetTieOut(ctx context.Context, propertyID uuid.UUID, period string) (*domain.PeriodTieOut, error) {
	t := &domain.PeriodTieOut{}
	var raw []byte
	err := r.pool.QueryRow(ctx, `
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
	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(format('ledger-period:%s:%s', $1::text, $2::text)))`, t.PropertyID, t.PeriodMonth); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
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
		return mapLedgerPgErr(err)
	})
}

func (r *FinanceRepo) ReopenTieOut(ctx context.Context, propertyID uuid.UUID, period string, actor string) error {
	if r.pool == nil {
		return fmt.Errorf("ReopenTieOut requires pool access, got nil pool")
	}
	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(format('ledger-period:%s:%s', $1::text, $2::text)))`, propertyID, period); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL app.actor = $1", actor); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL app.reopen_period = 'on'"); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE period_tie_outs
			SET status = 'open', closed_at = NULL, updated_at = NOW()
			WHERE property_id = $1 AND period_month = $2`, propertyID, period)
		if err != nil {
			return mapLedgerPgErr(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

func (r *FinanceRepo) ListTieOuts(ctx context.Context, propertyID uuid.UUID, limit int) ([]domain.PeriodTieOut, error) {
	if limit <= 0 {
		limit = 12
	}
	rows, err := r.pool.Query(ctx, `
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
	_, err := r.pool.Exec(ctx, `
		INSERT INTO approval_requests (id, property_id, kind, subject_id, amount_paise, requested_by, status, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		a.ID, a.PropertyID, a.Kind, a.SubjectID, a.AmountPaise, a.RequestedBy, a.Status, nullIfEmpty(a.Note))
	return err
}

func (r *FinanceRepo) GetApproval(ctx context.Context, id uuid.UUID) (*domain.ApprovalRequest, error) {
	a := &domain.ApprovalRequest{}
	var note *string
	err := r.pool.QueryRow(ctx, `
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
	rows, err := r.pool.Query(ctx, `
		SELECT id, property_id, kind, subject_id, amount_paise, requested_by, status, decided_by, decided_at, COALESCE(note,''), created_at
		FROM approval_requests WHERE property_id=$1 AND ($2='' OR status=$2) ORDER BY created_at DESC LIMIT 100`, propertyID, status)
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
	_, err := r.pool.Exec(ctx, `
		UPDATE approval_requests SET status=$2, decided_by=$3, decided_at=$4, note=$5 WHERE id=$1`,
		a.ID, a.Status, a.DecidedBy, a.DecidedAt, nullIfEmpty(a.Note))
	return err
}

func (r *FinanceRepo) DecideApprovalAtomic(ctx context.Context, a *domain.ApprovalRequest, expenseStatus *domain.ExpenseStatus, lines []domain.JournalLine) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if expenseStatus != nil {
		res, err := tx.Exec(ctx, `UPDATE expenses SET status=$2 WHERE id=$1`, a.SubjectID, *expenseStatus)
		if err != nil {
			return fmt.Errorf("update expense status: %w", err)
		}
		if res.RowsAffected() == 0 {
			return fmt.Errorf("expense not found: %w", domain.ErrNotFound)
		}
	}

	for _, l := range lines {
		_, err = tx.Exec(ctx, `
			INSERT INTO financial_journal_entries (id, property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			l.ID, l.PropertyID, l.AccountCode, l.DebitPaise, l.CreditPaise, l.SourceType, l.SourceID, l.LineKind, l.OccurredAt)
		if isUnique(err) {
			return domain.ErrDuplicateIdempotency
		}
		if err != nil {
			return fmt.Errorf("insert journal line: %w", err)
		}
	}

	res, err := tx.Exec(ctx, `
		UPDATE approval_requests SET status=$2, decided_by=$3, decided_at=$4, note=$5 WHERE id=$1`,
		a.ID, a.Status, a.DecidedBy, a.DecidedAt, nullIfEmpty(a.Note))
	if err != nil {
		return fmt.Errorf("update approval: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("approval not found: %w", domain.ErrNotFound)
	}

	return tx.Commit(ctx)
}

func (r *FinanceRepo) InsertKPI(ctx context.Context, s *domain.KPISnapshot) error {
	_, err := r.pool.Exec(ctx, `
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
	_, err := r.pool.Exec(ctx, `
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
	err := r.pool.QueryRow(ctx, `
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
	_, err := r.pool.Exec(ctx, `
		INSERT INTO leakage_events (id, property_id, category, estimated_paise, confidence_bps, evidence_json, severity, status, detected_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.ID, e.PropertyID, e.Category, e.EstimatedPaise, e.ConfidenceBPS, nullJSON(e.Evidence), e.Severity, e.Status, e.DetectedAt)
	return err
}

func (r *FinanceRepo) ListLeakage(ctx context.Context, propertyID uuid.UUID) ([]domain.LeakageEvent, error) {
	rows, err := r.pool.Query(ctx, `
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
	err := r.pool.QueryRow(ctx, `
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
	_, err := r.pool.Exec(ctx, `
		INSERT INTO recommendations (id, property_id, leakage_event_id, issue, suggested_action, expected_savings_paise,
			effort, risk, confidence_bps, safety_ok, quality_ok, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		rec.ID, rec.PropertyID, rec.LeakageEventID, rec.Issue, rec.SuggestedAction, rec.ExpectedSavingsPaise,
		rec.Effort, rec.Risk, rec.ConfidenceBPS, rec.SafetyOK, rec.QualityOK, rec.Status)
	return err
}

func (r *FinanceRepo) ListRecommendations(ctx context.Context, propertyID uuid.UUID) ([]domain.Recommendation, error) {
	rows, err := r.pool.Query(ctx, `
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
	err := r.pool.QueryRow(ctx, `
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
	_, err := r.pool.Exec(ctx, `
		UPDATE recommendations SET status=$2, realized_savings_paise=$3, accepted_at=$4, completed_at=$5 WHERE id=$1`,
		rec.ID, rec.Status, rec.RealizedSavingsPaise, rec.AcceptedAt, rec.CompletedAt)
	return err
}

func (r *FinanceRepo) InsertForecast(ctx context.Context, f *domain.ForecastSnapshot) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO forecast_snapshots (property_id, horizon_days, as_of, payload_json)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (property_id, horizon_days, as_of) DO UPDATE SET payload_json=EXCLUDED.payload_json`,
		f.PropertyID, f.HorizonDays, f.AsOf, nullJSON(f.Payload))
	return err
}

func (r *FinanceRepo) LatestForecast(ctx context.Context, propertyID uuid.UUID, horizon int) (*domain.ForecastSnapshot, error) {
	f := &domain.ForecastSnapshot{}
	err := r.pool.QueryRow(ctx, `
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
	_, err := r.pool.Exec(ctx, `
		INSERT INTO expense_import_suggestions (id, property_id, txn_id, amount_paise, txn_date, note, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (property_id, txn_id) DO UPDATE SET amount_paise=EXCLUDED.amount_paise, note=EXCLUDED.note`,
		s.ID, s.PropertyID, s.TxnID, s.AmountPaise, s.TxnDate, s.Note, s.Status)
	return err
}

func (r *FinanceRepo) ListImportSuggestions(ctx context.Context, propertyID uuid.UUID) ([]domain.ExpenseImportSuggestion, error) {
	rows, err := r.pool.Query(ctx, `
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
	_, err := r.pool.Exec(ctx, `
		INSERT INTO meal_prep_actuals (property_id, meal_date, meal_slot, prepared_count, discarded_count)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (property_id, meal_date, meal_slot) DO UPDATE SET prepared_count=EXCLUDED.prepared_count, discarded_count=EXCLUDED.discarded_count`,
		m.PropertyID, m.MealDate, m.MealSlot, m.PreparedCount, m.DiscardedCount)
	return err
}

func (r *FinanceRepo) GetMealPrep(ctx context.Context, propertyID uuid.UUID, date time.Time) ([]domain.MealPrepActual, error) {
	rows, err := r.pool.Query(ctx, `
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

func (r *FinanceRepo) GetTrialBalance(ctx context.Context, propertyID uuid.UUID, to time.Time) ([]domain.TrialBalanceLine, error) {
	rows, err := r.pool.Query(ctx, `
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
	rows, err := r.pool.Query(ctx, `
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
	rows, err := r.pool.Query(ctx, `
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
	rows, err := r.pool.Query(ctx, `
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
	rows, err := r.pool.Query(ctx, `
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
	_, err := r.pool.Exec(ctx, `
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
	rows, err := r.pool.Query(ctx, `
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
	_ = r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(p.amount), 0)
		FROM payments p
		LEFT JOIN dues d ON p.due_id = d.id
		WHERE (p.property_id = $1 OR d.property_id = $1)
		  AND p.matched_at >= $2 AND p.matched_at < $3`,
		propertyID, startUTC, endUTC).Scan(&collectedPaise)

	var duePaise int64
	_ = r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM dues
		WHERE property_id = $1
		  AND due_date >= $2 AND due_date < $3`,
		propertyID, startUTC, endUTC).Scan(&duePaise)

	var expensePaise int64
	_ = r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_paise), 0)
		FROM expenses
		WHERE property_id = $1
		  AND occurred_at >= $2 AND occurred_at < $3`,
		propertyID, startUTC, endUTC).Scan(&expensePaise)

	netCashFlow := collectedPaise - expensePaise

	var totalRooms int
	var capacityBeds int
	_ = r.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(capacity), 0)
		FROM rooms
		WHERE property_id = $1`,
		propertyID).Scan(&totalRooms, &capacityBeds)

	var occupiedBeds int
	var occupiedRooms int
	_ = r.pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT room_id)
		FROM tenants
		WHERE property_id = $1 AND status = 'active'`,
		propertyID).Scan(&occupiedBeds, &occupiedRooms)

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

