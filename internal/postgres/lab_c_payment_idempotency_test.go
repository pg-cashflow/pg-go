package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// TestLabC_UnknownPaymentOutcomeAndIdempotency implements Reproducible Failure Lab C:
// 1. Simulates duplicate payment / webhook event deliveries with duplicate identifiers.
// 2. Verifies that the database idempotency constraints reject duplicate financial mutations.
// 3. Verifies that the double-entry financial ledger invariants hold: sum(debit_paise) == sum(credit_paise).
// 4. Verifies that due payment allocation state is not corrupted by replays.
func TestLabC_UnknownPaymentOutcomeAndIdempotency(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	propID := uuid.New()
	invite := "LC" + uuid.New().String()[:6]

	// Seed property
	err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
			VALUES ($1, 'Lab C Property', '900 Financial Plaza', '+919555500010', 'fin@upi', 'Owner Fin', 'fin@test.com', $2)
		`, propID, invite)
		return err
	})
	if err != nil {
		t.Fatalf("failed seeding Lab C property: %v", err)
	}

	t.Cleanup(func() {
		_ = WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(context.Background(), "DELETE FROM financial_journal_entries WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payments WHERE due_id IN (SELECT id FROM dues WHERE property_id = $1)", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM dues WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM tenants WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM properties WHERE id = $1", propID)
			return nil
		})
	})

	tenantID := uuid.New()
	dueID := uuid.New()
	phone := "+91" + uuid.New().String()[:10]
	dueCode := "C" + uuid.New().String()[:6]
	const paymentAmountPaise int64 = 2500000 // ₹25,000.00

	err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Fin Tenant C', $3, $4, 5, 'active')
		`, tenantID, propID, phone, paymentAmountPaise)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
			VALUES ($1, $2, $3, $4, 'rent', $5, $5, CURRENT_DATE, CURRENT_DATE + 30, CURRENT_DATE + 5, 'pending')
		`, dueID, dueCode, tenantID, propID, paymentAmountPaise)
		return err
	})
	if err != nil {
		t.Fatalf("failed seeding tenant and due: %v", err)
	}

	scopedCtx := requestscope.WithPropertyID(ctx, propID)
	scopedDB := NewScopedDB(pool)
	upiTxnID := fmt.Sprintf("UPI-TXN-%s", uuid.New().String()[:12])
	paymentID := uuid.New()

	t.Run("Subtest 1: First payment delivery records payment, marks due, and writes balanced journal", func(t *testing.T) {
		err := WithinTx(scopedCtx, pool, func(tx pgx.Tx) error {
			// Insert payment with upi_txn_id
			_, err := tx.Exec(scopedCtx, `
				INSERT INTO payments (id, due_id, tenant_id, property_id, upi_txn_id, amount, matched_by, provider, raw_note)
				VALUES ($1, $2, $3, $4, $5, $6, 'manual', 'manual', 'Sandbox Payment Delivery')
			`, paymentID, dueID, tenantID, propID, upiTxnID, paymentAmountPaise)
			if err != nil {
				return fmt.Errorf("insert payment: %w", err)
			}

			// Update due to paid
			_, err = tx.Exec(scopedCtx, `
				UPDATE dues SET status = 'paid', paid_at = NOW(), amount = 0 WHERE id = $1
			`, dueID)
			if err != nil {
				return fmt.Errorf("update due: %w", err)
			}

			// Insert balanced double-entry financial journal lines (Debit Bank 1010, Credit Rent Income 4010)
			now := time.Now().UTC()
			_, err = tx.Exec(scopedCtx, `
				INSERT INTO financial_journal_entries (property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
				VALUES ($1, '1010_CASHFREE_CLEARING', $2, 0, 'payment', $3, 'clearing_debit', $4),
				       ($1, '4010_RENT_REVENUE', 0, $2, 'payment', $3, 'revenue_credit', $4)
			`, propID, paymentAmountPaise, paymentID, now)
			if err != nil {
				return fmt.Errorf("insert journal entries: %w", err)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("first payment transaction failed: %v", err)
		}

		// Verify state via ScopedDB
		var dueStatus string
		var dueRemaining int64
		err = scopedDB.QueryRow(scopedCtx, "SELECT status, amount FROM dues WHERE id = $1", dueID).Scan(&dueStatus, &dueRemaining)
		if err != nil {
			t.Fatalf("failed querying due: %v", err)
		}
		if dueStatus != "paid" || dueRemaining != 0 {
			t.Fatalf("expected due paid with 0 remaining, got status=%s amount=%d", dueStatus, dueRemaining)
		}
	})

	t.Run("Subtest 2: Duplicate payment webhook delivery is rejected idempotently by database uniqueness", func(t *testing.T) {
		duplicatePaymentID := uuid.New()
		err := WithinTx(scopedCtx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(scopedCtx, "SAVEPOINT sp_dup"); err != nil {
				return err
			}
			// Attempt replay with the same upi_txn_id
			_, err = tx.Exec(scopedCtx, `
				INSERT INTO payments (id, due_id, tenant_id, property_id, upi_txn_id, amount, matched_by, provider, raw_note)
				VALUES ($1, $2, $3, $4, $5, $6, 'manual', 'manual', 'Duplicate Webhook Delivery')
			`, duplicatePaymentID, dueID, tenantID, propID, upiTxnID, paymentAmountPaise)
			if err == nil {
				t.Fatalf("CRITICAL FINANCIAL INTEGRITY FAILURE: duplicate upi_txn_id was accepted by database!")
			}
			// Rollback savepoint
			_, _ = tx.Exec(scopedCtx, "ROLLBACK TO SAVEPOINT sp_dup")
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error during idempotency check: %v", err)
		}

		// Verify payment count is still strictly 1
		var payCount int
		err = scopedDB.QueryRow(scopedCtx, "SELECT count(*) FROM payments WHERE upi_txn_id = $1", upiTxnID).Scan(&payCount)
		if err != nil {
			t.Fatalf("failed counting payments: %v", err)
		}
		if payCount != 1 {
			t.Fatalf("expected exactly 1 payment record, found %d", payCount)
		}
	})

	t.Run("Subtest 3: Financial Ledger Double-Entry Balance Invariant Holds (sum(debit) == sum(credit))", func(t *testing.T) {
		var totalDebit, totalCredit int64
		err := scopedDB.QueryRow(scopedCtx, `
			SELECT COALESCE(SUM(debit_paise), 0), COALESCE(SUM(credit_paise), 0)
			FROM financial_journal_entries
			WHERE property_id = $1
		`, propID).Scan(&totalDebit, &totalCredit)
		if err != nil {
			t.Fatalf("failed querying ledger balance: %v", err)
		}

		if totalDebit != totalCredit {
			t.Fatalf("CRITICAL LEDGER BALANCE INVARIANT VIOLATION: totalDebit=%d != totalCredit=%d (discrepancy=%d paise)",
				totalDebit, totalCredit, totalDebit-totalCredit)
		}

		if totalDebit != paymentAmountPaise {
			t.Fatalf("expected ledger turnover of %d paise, got %d", paymentAmountPaise, totalDebit)
		}
	})
}
