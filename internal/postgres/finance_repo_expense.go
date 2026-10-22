package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

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

		var p domain.ApprovalPolicy
		p.PropertyID = e.PropertyID
		err := tx.QueryRow(ctx, `
			SELECT manager_daily_limit_paise, single_expense_limit_paise, manager_monthly_limit_paise,
			       owner_approval_threshold_paise, reimbursement_threshold_paise, emergency_bypass_enabled
			FROM approval_policies WHERE property_id=$1`, e.PropertyID).Scan(
			&p.ManagerDailyLimitPaise, &p.SingleExpenseLimitPaise, &p.ManagerMonthlyLimitPaise,
			&p.OwnerApprovalThresholdPaise, &p.ReimbursementThresholdPaise, &p.EmergencyBypassEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			p = domain.DefaultPolicy(e.PropertyID)
		} else if err != nil {
			return fmt.Errorf("read approval policy: %w", err)
		}

		dayFrom := domain.DayStart(e.OccurredAt)
		dayTo := dayFrom.AddDate(0, 0, 1)
		monthFrom, monthTo, err := domain.PeriodBounds(e.OccurredAt.Format("2006-01"))
		if err != nil {
			return fmt.Errorf("calculate period bounds: %w", err)
		}

		spendQuery := `
			SELECT (
				COALESCE((SELECT SUM(amount_paise) FROM expense_payments
				          WHERE property_id=$1 AND payer_user_id=$2 AND payer_role='manager'
				            AND occurred_at >= $3 AND occurred_at < $4), 0)
				+
				COALESCE((SELECT SUM(GREATEST(0, ex.amount_paise - COALESCE((SELECT SUM(ep.amount_paise) FROM expense_payments ep WHERE ep.expense_id = ex.id AND ep.payer_user_id = $2), 0)))
				          FROM expenses ex
				          WHERE ex.property_id=$1 AND ex.created_by=$2 AND ex.created_by_role='manager'
				            AND ex.status IN ('approved', 'paid')
				            AND ex.occurred_at >= $3 AND ex.occurred_at < $4), 0)
			)`

		var daily, monthly int64
		if err := tx.QueryRow(ctx, spendQuery, e.PropertyID, e.CreatedBy, dayFrom, dayTo).Scan(&daily); err != nil {
			return fmt.Errorf("query daily spend: %w", err)
		}
		if err := tx.QueryRow(ctx, spendQuery, e.PropertyID, e.CreatedBy, monthFrom, monthTo).Scan(&monthly); err != nil {
			return fmt.Errorf("query monthly spend: %w", err)
		}

		chk := domain.EvaluateManagerSpend(p, e.AmountPaise, daily, monthly, e.Emergency)
		if chk.Reject != nil {
			return chk.Reject
		}
		if chk.NeedsApproval && !(e.Emergency && p.EmergencyBypassEnabled) {
			e.Status = domain.ExpensePendingApproval
		} else {
			e.Status = domain.ExpenseApproved
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

	if e.Status == domain.ExpensePendingApproval && approval != nil {
		approval.SubjectID = e.ID
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

	if e.Status == domain.ExpenseApproved {
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
	}

	return tx.Commit(ctx)
}

func (r *FinanceRepo) VoidExpenseAtomic(ctx context.Context, expenseID uuid.UUID, expected domain.ExpenseStatus, lines []domain.JournalLine, v domain.ExpenseVoid) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock order: pending approval rows first, then the expense row.
	// DecideApprovalAtomic uses the same order, so the two paths cannot deadlock.
	if _, err := tx.Exec(ctx, `
		SELECT id FROM approval_requests
		WHERE kind = 'expense' AND subject_id = $1 AND status = 'pending'
		ORDER BY id FOR UPDATE`, expenseID); err != nil {
		return fmt.Errorf("lock approvals: %w", err)
	}

	var cur string
	err = tx.QueryRow(ctx, `SELECT status FROM expenses WHERE id = $1 FOR UPDATE`, expenseID).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock expense: %w", err)
	}
	if domain.ExpenseStatus(cur) != expected {
		return domain.ErrExpenseStateChanged
	}

	if _, err := tx.Exec(ctx, `
		UPDATE expenses
		SET status = 'cancelled', void_reason = $2, voided_by = $3, voided_at = $4
		WHERE id = $1`, expenseID, v.Reason, v.VoidedBy, v.VoidedAt); err != nil {
		return fmt.Errorf("update expense status: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE approval_requests
		SET status = 'cancelled', decided_by = $2, decided_at = $3, note = $4
		WHERE kind = 'expense' AND subject_id = $1 AND status = 'pending'`,
		expenseID, v.VoidedBy, v.VoidedAt, v.Reason); err != nil {
		return fmt.Errorf("cancel pending approval: %w", err)
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
			return fmt.Errorf("insert journal line: %w", mapLedgerPgErr(err))
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

		var pol domain.ApprovalPolicy
		pol.PropertyID = p.PropertyID
		err := tx.QueryRow(ctx, `
			SELECT manager_daily_limit_paise, single_expense_limit_paise, manager_monthly_limit_paise,
			       owner_approval_threshold_paise, reimbursement_threshold_paise, emergency_bypass_enabled
			FROM approval_policies WHERE property_id=$1`, p.PropertyID).Scan(
			&pol.ManagerDailyLimitPaise, &pol.SingleExpenseLimitPaise, &pol.ManagerMonthlyLimitPaise,
			&pol.OwnerApprovalThresholdPaise, &pol.ReimbursementThresholdPaise, &pol.EmergencyBypassEnabled)
		if errors.Is(err, pgx.ErrNoRows) {
			pol = domain.DefaultPolicy(p.PropertyID)
		} else if err != nil {
			return nil, fmt.Errorf("read approval policy: %w", err)
		}

		dayFrom := domain.DayStart(p.OccurredAt)
		dayTo := dayFrom.AddDate(0, 0, 1)
		monthFrom, monthTo, err := domain.PeriodBounds(p.OccurredAt.Format("2006-01"))
		if err != nil {
			return nil, fmt.Errorf("calculate period bounds: %w", err)
		}

		spendQuery := `
			SELECT (
				COALESCE((SELECT SUM(amount_paise) FROM expense_payments
				          WHERE property_id=$1 AND payer_user_id=$2 AND payer_role='manager'
				            AND occurred_at >= $3 AND occurred_at < $4), 0)
				+
				COALESCE((SELECT SUM(GREATEST(0, ex.amount_paise - COALESCE((SELECT SUM(ep.amount_paise) FROM expense_payments ep WHERE ep.expense_id = ex.id AND ep.payer_user_id = $2), 0)))
				          FROM expenses ex
				          WHERE ex.property_id=$1 AND ex.created_by=$2 AND ex.created_by_role='manager'
				            AND ex.status IN ('approved', 'paid')
				            AND ex.occurred_at >= $3 AND ex.occurred_at < $4), 0)
			)`

		var daily, monthly int64
		if err := tx.QueryRow(ctx, spendQuery, p.PropertyID, p.PayerUserID, dayFrom, dayTo).Scan(&daily); err != nil {
			return nil, fmt.Errorf("query daily spend: %w", err)
		}
		if err := tx.QueryRow(ctx, spendQuery, p.PropertyID, p.PayerUserID, monthFrom, monthTo).Scan(&monthly); err != nil {
			return nil, fmt.Errorf("query monthly spend: %w", err)
		}

		chk := domain.EvaluateManagerSpend(pol, p.AmountPaise, daily, monthly, false)
		if chk.Reject != nil {
			return nil, chk.Reject
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

func (r *FinanceRepo) ListExpensesPaginated(ctx context.Context, propertyID uuid.UUID, limit int, beforeOccurredAt *time.Time, beforeID *uuid.UUID) ([]domain.Expense, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `
		SELECT id, property_id, category_code, COALESCE(vendor_name,''), COALESCE(description,''), amount_paise, status, emergency, room_id, created_by, created_by_role, occurred_at, created_at
		FROM expenses WHERE property_id=$1`
	args := []any{propertyID}
	argIdx := 2
	if beforeOccurredAt != nil && !beforeOccurredAt.IsZero() && beforeID != nil {
		q += fmt.Sprintf(" AND (occurred_at, id) < ($%d, $%d)", argIdx, argIdx+1)
		args = append(args, *beforeOccurredAt, *beforeID)
		argIdx += 2
	} else if beforeOccurredAt != nil && !beforeOccurredAt.IsZero() {
		q += fmt.Sprintf(" AND occurred_at < $%d", argIdx)
		args = append(args, *beforeOccurredAt)
		argIdx++
	}
	q += fmt.Sprintf(" ORDER BY occurred_at DESC, id DESC LIMIT $%d", argIdx)
	args = append(args, limit)

	rows, err := r.db.Query(ctx, q, args...)
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

func (r *FinanceRepo) ListExpenses(ctx context.Context, propertyID uuid.UUID) ([]domain.Expense, error) {
	return r.ListExpensesPaginated(ctx, propertyID, 50, nil, nil)
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
	err := r.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM expense_payments WHERE expense_id=$1`, expenseID).Scan(&s)
	return s, err
}

func (r *FinanceRepo) SumManagerSpend(ctx context.Context, propertyID, managerID uuid.UUID, from, to time.Time) (int64, error) {
	var s int64
	err := r.db.QueryRow(ctx, `
		SELECT (
			COALESCE((SELECT SUM(amount_paise) FROM expense_payments
			          WHERE property_id=$1 AND payer_user_id=$2 AND payer_role='manager'
			            AND occurred_at >= $3 AND occurred_at < $4), 0)
			+
			COALESCE((SELECT SUM(ex.amount_paise) FROM expenses ex
			          WHERE ex.property_id=$1 AND ex.created_by=$2 AND ex.created_by_role='manager'
			            AND ex.status IN ('approved', 'paid')
			            AND ex.occurred_at >= $3 AND ex.occurred_at < $4
			            AND NOT EXISTS (SELECT 1 FROM expense_payments ep WHERE ep.expense_id = ex.id AND ep.payer_user_id = $2)), 0)
		)`,
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

func (r *FinanceRepo) InsertReimbursementAtomic(ctx context.Context, rm *domain.ManagerReimbursement, lines []domain.JournalLine) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(format('manager-reimburse:%s:%s', $1::text, $2::text)))`, rm.PropertyID, rm.ManagerUserID); err != nil {
		return fmt.Errorf("lock manager reimbursement: %w", err)
	}

	var adv, re int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_advances WHERE property_id=$1 AND manager_user_id=$2`, rm.PropertyID, rm.ManagerUserID).Scan(&adv); err != nil {
		return fmt.Errorf("query manager advances: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_reimbursements WHERE property_id=$1 AND manager_user_id=$2`, rm.PropertyID, rm.ManagerUserID).Scan(&re); err != nil {
		return fmt.Errorf("query manager reimbursements: %w", err)
	}
	outstanding := adv - re
	if rm.AmountPaise > outstanding {
		return domain.ErrOverpay
	}

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
	if err := r.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_advances WHERE property_id=$1`, propertyID).Scan(&adv); err != nil {
		return 0, err
	}
	if err := r.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_reimbursements WHERE property_id=$1`, propertyID).Scan(&re); err != nil {
		return 0, err
	}
	return adv - re, nil
}

func (r *FinanceRepo) ManagerAdvanceOutstanding(ctx context.Context, propertyID, managerID uuid.UUID) (int64, error) {
	var adv, re int64
	if err := r.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_advances WHERE property_id=$1 AND manager_user_id=$2`, propertyID, managerID).Scan(&adv); err != nil {
		return 0, err
	}
	if err := r.db.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise),0) FROM manager_reimbursements WHERE property_id=$1 AND manager_user_id=$2`, propertyID, managerID).Scan(&re); err != nil {
		return 0, err
	}
	return adv - re, nil
}

func (r *FinanceRepo) ListAdvances(ctx context.Context, propertyID uuid.UUID) ([]domain.ManagerAdvance, error) {
	rows, err := r.db.Query(ctx, `
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
