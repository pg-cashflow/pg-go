package postgres

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLivePostgres_VoidExpenseAtomic_ConcurrentRace(t *testing.T) {
	pool, cfg := setupLiveTestPool(t, 60*time.Second)
	if pool == nil || cfg == nil {
		return
	}
	ctx := context.Background()

	// Ensure migrations applied
	if err := Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	propID := uuid.New()
	ownerID := uuid.New()
	inviteCode := fmt.Sprintf("VOID%s", uuid.New().String()[:8])

	// Create property
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Void Race Test PG', '123 Void Way', '+919999977777', 'owner@upi', 'Owner', 'void@test.com', $2)`,
		propID, inviteCode,
	)
	if err != nil {
		t.Fatalf("failed to insert property: %v", err)
	}

	phone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+77)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3)`, ownerID, phone, propID)
	if err != nil {
		t.Fatalf("failed to insert owner: %v", err)
	}

	defer func() {
		// Stop deleting ledger rows: financial_journal_entries is append-only by migrations 043/044/057.
		if _, err := pool.Exec(ctx, `DELETE FROM expenses WHERE property_id = $1`, propID); err != nil {
			t.Errorf("cleanup expenses failed: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE property_id = $1`, propID); err != nil {
			t.Errorf("cleanup users failed: %v", err)
		}
	}()

	repo := NewFinanceRepo(pool)

	expID := uuid.New()
	now := time.Now().UTC()
	exp := &domain.Expense{
		ID:             expID,
		PropertyID:     propID,
		CategoryCode:   "vendor",
		AmountPaise:    150000,
		Status:         domain.ExpenseApproved,
		CreatedBy:      ownerID,
		CreatedByRole:  string(domain.RoleOwner),
		IdempotencyKey: fmt.Sprintf("exp-%s", expID),
		OccurredAt:     now,
	}

	lines := []domain.JournalLine{
		{
			ID:          uuid.New(),
			PropertyID:  propID,
			AccountCode: domain.AcctOperatingExpense,
			DebitPaise:  150000,
			CreditPaise: 0,
			SourceType:  "expense",
			SourceID:    expID,
			LineKind:    "expense_accrual",
			OccurredAt:  now,
		},
		{
			ID:          uuid.New(),
			PropertyID:  propID,
			AccountCode: domain.AcctAccountsPayable,
			DebitPaise:  0,
			CreditPaise: 150000,
			SourceType:  "expense",
			SourceID:    expID,
			LineKind:    "payable_accrual",
			OccurredAt:  now,
		},
	}

	if err := repo.InsertExpenseAtomic(ctx, exp, lines, nil); err != nil {
		t.Fatalf("insert expense atomic: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	stateChangedCount := 0
	otherErrors := 0

	voidLines := []domain.JournalLine{
		{
			ID:          uuid.New(),
			PropertyID:  propID,
			AccountCode: domain.AcctAccountsPayable,
			DebitPaise:  150000,
			CreditPaise: 0,
			SourceType:  "expense_void",
			SourceID:    expID,
			LineKind:    "void_payable_dr",
			OccurredAt:  now,
		},
		{
			ID:          uuid.New(),
			PropertyID:  propID,
			AccountCode: domain.AcctOperatingExpense,
			DebitPaise:  0,
			CreditPaise: 150000,
			SourceType:  "expense_void",
			SourceID:    expID,
			LineKind:    "void_expense_cr",
			OccurredAt:  now,
		},
	}

	v := domain.ExpenseVoid{
		Reason:   "concurrent duplicate bill void",
		VoidedBy: ownerID,
		VoidedAt: now,
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(workerIdx int) {
			defer wg.Done()
			// Generate distinct journal line IDs for each attempt to avoid unique constraint collisions
			attemptLines := []domain.JournalLine{
				{
					ID:          uuid.New(),
					PropertyID:  propID,
					AccountCode: voidLines[0].AccountCode,
					DebitPaise:  voidLines[0].DebitPaise,
					CreditPaise: voidLines[0].CreditPaise,
					SourceType:  voidLines[0].SourceType,
					SourceID:    voidLines[0].SourceID,
					LineKind:    voidLines[0].LineKind,
					OccurredAt:  voidLines[0].OccurredAt,
				},
				{
					ID:          uuid.New(),
					PropertyID:  propID,
					AccountCode: voidLines[1].AccountCode,
					DebitPaise:  voidLines[1].DebitPaise,
					CreditPaise: voidLines[1].CreditPaise,
					SourceType:  voidLines[1].SourceType,
					SourceID:    voidLines[1].SourceID,
					LineKind:    voidLines[1].LineKind,
					OccurredAt:  voidLines[1].OccurredAt,
				},
			}
			err := repo.VoidExpenseAtomic(ctx, expID, domain.ExpenseApproved, attemptLines, v)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, domain.ErrExpenseStateChanged):
				stateChangedCount++
			default:
				otherErrors++
				t.Logf("worker %d returned unexpected error: %v", workerIdx, err)
			}
		}(i)
	}
	wg.Wait()

	if okCount != 1 {
		t.Fatalf("expected exactly 1 successful void, got %d", okCount)
	}
	if stateChangedCount != workers-1 {
		t.Fatalf("expected %d ErrExpenseStateChanged errors, got %d", workers-1, stateChangedCount)
	}
	if otherErrors != 0 {
		t.Fatalf("expected 0 other errors, got %d", otherErrors)
	}

	// Verify database row state
	var status, voidReason string
	var voidedBy uuid.UUID
	err = pool.QueryRow(ctx, `SELECT status, void_reason, voided_by FROM expenses WHERE id = $1`, expID).
		Scan(&status, &voidReason, &voidedBy)
	if err != nil {
		t.Fatalf("query final expense state: %v", err)
	}
	if status != string(domain.ExpenseCancelled) {
		t.Errorf("expected expense status %s, got %s", domain.ExpenseCancelled, status)
	}
	if voidReason != v.Reason {
		t.Errorf("expected void_reason %q, got %q", v.Reason, voidReason)
	}
	if voidedBy != v.VoidedBy {
		t.Errorf("expected voided_by %s, got %s", v.VoidedBy, voidedBy)
	}
}

func TestLivePostgres_VoidVsDecideApproval_ConcurrentRace(t *testing.T) {
	pool, cfg := setupLiveTestPool(t, 60*time.Second)
	if pool == nil || cfg == nil {
		return
	}
	ctx := context.Background()

	if err := Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	propID := uuid.New()
	ownerID := uuid.New()
	mgrID := uuid.New()
	inviteCode := fmt.Sprintf("APPR%s", uuid.New().String()[:8])

	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Approval Race Test PG', '456 Race Rd', '+919999966666', 'owner@upi', 'Owner', 'race@test.com', $2)`,
		propID, inviteCode,
	)
	if err != nil {
		t.Fatalf("failed to insert property: %v", err)
	}

	phone1 := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+88)%10000000000)
	phone2 := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+99)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $3, 'owner', $5), ($2, $4, 'manager', $5)`, ownerID, mgrID, phone1, phone2, propID)
	if err != nil {
		t.Fatalf("failed to insert users: %v", err)
	}

	defer func() {
		if _, err := pool.Exec(ctx, `DELETE FROM approval_requests WHERE property_id = $1`, propID); err != nil {
			t.Errorf("cleanup approval_requests failed: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM expenses WHERE property_id = $1`, propID); err != nil {
			t.Errorf("cleanup expenses failed: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM approval_policies WHERE property_id = $1`, propID); err != nil {
			t.Errorf("cleanup approval_policies failed: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE property_id = $1`, propID); err != nil {
			t.Errorf("cleanup users failed: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID); err != nil {
			t.Errorf("cleanup properties failed: %v", err)
		}
	}()

	repo := NewFinanceRepo(pool)

	// Save policy so expense > 1000 INR requires owner approval and enters pending_approval
	policy := &domain.ApprovalPolicy{
		PropertyID:                  propID,
		ManagerDailyLimitPaise:      10000000,
		ManagerMonthlyLimitPaise:    50000000,
		SingleExpenseLimitPaise:     10000000,
		OwnerApprovalThresholdPaise: 100000,
	}
	if err := repo.SaveUnifiedSettings(ctx, propID, nil, policy, nil); err != nil {
		t.Fatalf("save policy: %v", err)
	}

	expID := uuid.New()
	apprID := uuid.New()
	now := time.Now().UTC()

	exp := &domain.Expense{
		ID:             expID,
		PropertyID:     propID,
		CategoryCode:   "vendor",
		AmountPaise:    500000,
		Status:         domain.ExpensePendingApproval,
		CreatedBy:      mgrID,
		CreatedByRole:  string(domain.RoleManager),
		IdempotencyKey: fmt.Sprintf("exp-%s", expID),
		OccurredAt:     now,
	}

	appr := &domain.ApprovalRequest{
		ID:          apprID,
		PropertyID:  propID,
		Kind:        "expense",
		SubjectID:   expID,
		AmountPaise: 500000,
		RequestedBy: mgrID,
		Status:      "pending",
		CreatedAt:   now,
	}

	if err := repo.InsertExpenseAtomic(ctx, exp, nil, appr); err != nil {
		t.Fatalf("insert pending expense atomic: %v", err)
	}

	var wg sync.WaitGroup
	var voidErr, decideErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		v := domain.ExpenseVoid{
			Reason:   "void before approval decision",
			VoidedBy: ownerID,
			VoidedAt: time.Now().UTC(),
		}
		voidErr = repo.VoidExpenseAtomic(ctx, expID, domain.ExpensePendingApproval, nil, v)
	}()

	go func() {
		defer wg.Done()
		targetStatus := domain.ExpenseApproved
		decideAppr := &domain.ApprovalRequest{
			ID:         apprID,
			PropertyID: propID,
			SubjectID:  expID,
			Status:     "approved",
			DecidedBy:  &ownerID,
			DecidedAt:  &now,
			Note:       "approved by owner",
		}
		decideErr = repo.DecideApprovalAtomic(ctx, decideAppr, &targetStatus, nil)
	}()
	wg.Wait()

	// Exactly one of the two actions must win:
	// If void won: voidErr == nil, decideErr != nil (ErrExpenseStateChanged)
	// If decide won: decideErr == nil, voidErr != nil (ErrExpenseStateChanged)
	t.Logf("concurrency results: voidErr=%v, decideErr=%v", voidErr, decideErr)

	if voidErr == nil && decideErr == nil {
		t.Fatalf("illegal state: both void and decide approval succeeded concurrently")
	}

	// Verify database integrity: if void won, approval must not resurrect expense
	var finalExpStatus string
	err = pool.QueryRow(ctx, `SELECT status FROM expenses WHERE id = $1`, expID).Scan(&finalExpStatus)
	if err != nil {
		t.Fatalf("query expense status: %v", err)
	}

	var finalApprStatus string
	err = pool.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id = $1`, apprID).Scan(&finalApprStatus)
	if err != nil {
		t.Fatalf("query approval status: %v", err)
	}

	if voidErr == nil {
		if finalExpStatus != string(domain.ExpenseCancelled) {
			t.Errorf("void won but final expense status is %s, want cancelled", finalExpStatus)
		}
		if finalApprStatus != "cancelled" {
			t.Errorf("void won but final approval status is %s, want cancelled", finalApprStatus)
		}
	} else if decideErr == nil {
		if finalExpStatus != string(domain.ExpenseApproved) {
			t.Errorf("decide won but final expense status is %s, want approved", finalExpStatus)
		}
		if finalApprStatus != "approved" {
			t.Errorf("decide won but final approval status is %s, want approved", finalApprStatus)
		}
	}
}
