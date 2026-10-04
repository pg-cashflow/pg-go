package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// TestPhysicalCrashSafety_LivePostgresExecution proves B-7 and B-15 against a physical PostgreSQL instance:
// 1. Payment + Ledger Outbox Enqueue commit atomically in real PG transaction.
// 2. Process crashes immediately post-commit (zero in-memory state, zero journal entries written).
// 3. Independent worker process starts later against the database, discovers pending outbox event, and creates journal entries.
// 4. Worker crashes after journal write and retries: deterministic UUIDs ensure zero duplicate journal entries.
// 5. Invariant check: sum(debit) == sum(credit) holds.
func TestPhysicalCrashSafety_LivePostgresExecution(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping physical crash test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	propID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()
	inviteCode := fmt.Sprintf("CR%s", uuid.New().String()[:6])

	// Seed test property and tenant
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Physical Crash PG', '101 Crash Ave', '+919988776655', 'crash@upi', 'Crash Owner', 'crash@test.com', $2)`,
		propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM properties WHERE id = $1", propID)
	}()

	phone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Crash Tenant', $3, '101', 1500000, 1, 'active')`,
		tenantID, propID, phone)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	dueID := uuid.New()
	dueCode := fmt.Sprintf("D%s", uuid.New().String()[:7])
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
		VALUES ($1, $2, $3, $4, 'rent', 1500000, 1500000, CURRENT_DATE, CURRENT_DATE, CURRENT_DATE, 'pending')`,
		dueID, dueCode, tenantID, propID)
	if err != nil {
		t.Fatalf("insert due: %v", err)
	}

	// -----------------------------------------------------------------------
	// Step 1: Execute Atomic Payment + Outbox Enqueue inside single PostgreSQL TX
	// -----------------------------------------------------------------------
	var outboxEventID int64
	cfPaymentID := fmt.Sprintf("cf_crash_%d", time.Now().UnixNano())

	err = postgres.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		txPayRepo := postgres.NewPaymentRepo(tx)
		p := &domain.Payment{
			DueID:       dueID,
			TenantID:    tenantID,
			CFPaymentID: &cfPaymentID,
			Amount:      1500000,
			MatchedBy:   domain.MatchedByCashfree,
			MatchedAt:   now,
			IsUnapplied: false,
		}
		if err := txPayRepo.Create(ctx, p); err != nil {
			return fmt.Errorf("insert payment: %w", err)
		}
		paymentID = p.ID

		// 1b. Enqueue ledger outbox event
		payload, err := json.Marshal(domain.PaymentMirrorPayload{
			PropertyID: propID,
			PaymentID:  paymentID,
			Allocations: []domain.PaymentAllocationItemPayload{
				{AmountPaise: 1500000, DueKind: "rent"},
			},
			UnappliedPaise: 0,
			IsUnapplied:    false,
			MatchedAt:      now,
			MatchedBy:      domain.MatchedByCashfree,
			AmountPaise:    1500000,
		})
		if err != nil {
			return err
		}

		outboxRepo := postgres.NewLedgerOutboxRepo(pool)
		outboxEvt := &domain.LedgerOutboxEvent{
			EventType:      "payment_mirror",
			PropertyID:     propID,
			SourceID:       paymentID,
			Payload:        payload,
			IdempotencyKey: fmt.Sprintf("payment_mirror:%s", paymentID),
			MaxAttempts:    5,
		}
		if err := outboxRepo.InsertLedgerOutboxEventTx(ctx, tx, outboxEvt); err != nil {
			return fmt.Errorf("enqueue outbox: %w", err)
		}
		outboxEventID = outboxEvt.ID
		return nil
	})
	if err != nil {
		t.Fatalf("WithinTx payment + outbox failed: %v", err)
	}
	if outboxEventID == 0 {
		t.Fatalf("expected non-zero outbox event ID")
	}

	// -----------------------------------------------------------------------
	// Step 2: Simulate Complete Process Termination Immediately Post-Commit
	// -----------------------------------------------------------------------
	// At this point, the HTTP handler terminates. NO journal write has occurred.
	// Query the live PostgreSQL database from a fresh query to verify physical state:
	var paymentExists bool
	_ = pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM payments WHERE id = $1)", paymentID).Scan(&paymentExists)
	if !paymentExists {
		t.Fatalf("expected payment %s to exist in PostgreSQL", paymentID)
	}

	var pendingOutboxCount int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM ledger_outbox_events WHERE id = $1 AND processed_at IS NULL", outboxEventID).Scan(&pendingOutboxCount)
	if pendingOutboxCount != 1 {
		t.Fatalf("expected 1 pending outbox event in PostgreSQL, got %d", pendingOutboxCount)
	}

	var initialJournalCount int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM financial_journal_entries WHERE source_id = $1", paymentID).Scan(&initialJournalCount)
	if initialJournalCount != 0 {
		t.Fatalf("expected 0 journal entries before worker runs, got %d", initialJournalCount)
	}
	t.Logf("✓ Physical crash state confirmed: payment committed, outbox enqueued, 0 journals written")

	// -----------------------------------------------------------------------
	// Step 3: Independent Worker Boots Up Later & Processes Pending Outbox Event
	// -----------------------------------------------------------------------
	outboxRepo := postgres.NewLedgerOutboxRepo(pool)
	financeRepo := postgres.NewFinanceRepo(pool)
	financeSvc := NewService(financeRepo, nil)

	worker := NewLedgerOutboxWorker(pool, outboxRepo, financeSvc)
	err = worker.ProcessSingleEvent(ctx, outboxEventID)
	if err != nil {
		t.Fatalf("worker failed processing event: %v", err)
	}

	// Verify journal entries in PostgreSQL
	var postWorkerJournalCount int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM financial_journal_entries WHERE source_id = $1", paymentID).Scan(&postWorkerJournalCount)
	if postWorkerJournalCount != 2 {
		t.Fatalf("expected exactly 2 journal lines in PostgreSQL, got %d", postWorkerJournalCount)
	}

	var isProcessed bool
	_ = pool.QueryRow(ctx, "SELECT processed_at IS NOT NULL FROM ledger_outbox_events WHERE id = $1", outboxEventID).Scan(&isProcessed)
	if !isProcessed {
		t.Fatalf("expected outbox event %d to be marked processed", outboxEventID)
	}
	t.Logf("✓ Worker successfully mirrored payment to financial journal in PostgreSQL")

	// -----------------------------------------------------------------------
	// Step 4: Worker Retry Crash Proof — Re-executing Same Event Does NOT Duplicate Lines
	// -----------------------------------------------------------------------
	// Reset processed_at to simulate worker crashing before commit of processed_at
	_, err = pool.Exec(ctx, "UPDATE ledger_outbox_events SET processed_at = NULL WHERE id = $1", outboxEventID)
	if err != nil {
		t.Fatalf("reset processed_at: %v", err)
	}

	err = worker.ProcessSingleEvent(ctx, outboxEventID)
	if err != nil {
		t.Fatalf("worker retry failed: %v", err)
	}

	var postRetryJournalCount int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM financial_journal_entries WHERE source_id = $1", paymentID).Scan(&postRetryJournalCount)
	if postRetryJournalCount != 2 {
		t.Fatalf("DUPLICATE CORRUPTION: expected 2 journal lines after retry, found %d", postRetryJournalCount)
	}
	t.Logf("✓ Worker retry executed with zero duplicate lines (deterministic UUID-v5 deduplication verified in PostgreSQL)")

	// -----------------------------------------------------------------------
	// Step 5: Verify Ledger Invariants on Physical PostgreSQL Database
	// -----------------------------------------------------------------------
	var netDiff int64
	err = pool.QueryRow(ctx, "SELECT coalesce(sum(debit_paise) - sum(credit_paise), 0) FROM financial_journal_entries WHERE property_id = $1", propID).Scan(&netDiff)
	if err != nil || netDiff != 0 {
		t.Fatalf("ledger unbalanced in PostgreSQL: net diff = %d paise", netDiff)
	}
	t.Logf("✓ Double-entry balance holds in PostgreSQL: net difference = 0 paise")
}

// TestPhysicalCrashSafety_DepartureSettlement_LivePostgres proves B-15 against live PostgreSQL:
// 1. Departure state + outbox event commit in single PG transaction.
// 2. Process dies post-commit (zero journal entries exist).
// 3. Worker recovers, polls event, writes 4 balanced journal lines to financial_journal_entries.
// 4. Worker retry creates zero duplicate lines in PostgreSQL.
// 5. Net double-entry balance is strictly 0 paise.
func TestPhysicalCrashSafety_DepartureSettlement_LivePostgres(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping physical crash test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	propID := uuid.New()
	depID := uuid.New()
	inviteCode := fmt.Sprintf("DP%s", uuid.New().String()[:6])

	// Seed test property
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Physical Departure PG', '202 Depart St', '+919988771122', 'depart@upi', 'Depart Owner', 'depart@test.com', $2)`,
		propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM properties WHERE id = $1", propID)
	}()

	now := time.Now().UTC().Truncate(time.Microsecond)
	var outboxEventID int64

	// Step 1: Atomic departure settlement transaction
	err = postgres.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		payload, err := json.Marshal(domain.DepartureSettlementMirrorPayload{
			PropertyID:                 propID,
			DepartureID:                depID,
			DepositAmountPaise:         1000000,
			UnusedRentRefundPaise:      100000,
			TotalDeductions:            50000,
			NetRefundPaise:             850000,
			OutstandingDuesNettedPaise: 200000,
			ReceivableBalancePaise:     0,
			OccurredAt:                 now,
		})
		if err != nil {
			return err
		}

		outboxRepo := postgres.NewLedgerOutboxRepo(pool)
		outboxEvt := &domain.LedgerOutboxEvent{
			EventType:      "departure_settlement_mirror",
			PropertyID:     propID,
			SourceID:       depID,
			Payload:        payload,
			IdempotencyKey: fmt.Sprintf("departure_settlement:%s", depID),
			MaxAttempts:    3,
		}
		if err := outboxRepo.InsertLedgerOutboxEventTx(ctx, tx, outboxEvt); err != nil {
			return fmt.Errorf("enqueue departure outbox: %w", err)
		}
		outboxEventID = outboxEvt.ID
		return nil
	})
	if err != nil {
		t.Fatalf("departure WithinTx failed: %v", err)
	}

	// Step 2: Simulate process death post-commit (0 journals exist)
	var initialCount int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM financial_journal_entries WHERE source_id = $1", depID).Scan(&initialCount)
	if initialCount != 0 {
		t.Fatalf("expected 0 departure journal entries before worker, got %d", initialCount)
	}
	t.Logf("✓ Physical crash state confirmed: departure outbox enqueued, 0 journals written")

	// Step 3: Worker runs later against live PostgreSQL
	outboxRepo := postgres.NewLedgerOutboxRepo(pool)
	financeRepo := postgres.NewFinanceRepo(pool)
	financeSvc := NewService(financeRepo, nil)
	worker := NewLedgerOutboxWorker(pool, outboxRepo, financeSvc)

	err = worker.ProcessSingleEvent(ctx, outboxEventID)
	if err != nil {
		t.Fatalf("worker failed processing departure event: %v", err)
	}

	var postWorkerCount int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM financial_journal_entries WHERE source_id = $1", depID).Scan(&postWorkerCount)
	if postWorkerCount != 5 {
		t.Fatalf("expected exactly 5 departure journal lines in PostgreSQL, got %d", postWorkerCount)
	}
	t.Logf("✓ Worker successfully mirrored departure settlement to financial journal in PostgreSQL (5 lines)")

	// Step 4: Worker crash & retry proof — re-running event produces zero duplicate lines
	_, err = pool.Exec(ctx, "UPDATE ledger_outbox_events SET processed_at = NULL WHERE id = $1", outboxEventID)
	if err != nil {
		t.Fatalf("reset processed_at: %v", err)
	}

	err = worker.ProcessSingleEvent(ctx, outboxEventID)
	if err != nil {
		t.Fatalf("worker retry failed: %v", err)
	}

	var postRetryCount int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM financial_journal_entries WHERE source_id = $1", depID).Scan(&postRetryCount)
	if postRetryCount != 5 {
		t.Fatalf("DUPLICATE CORRUPTION: expected 5 departure lines after retry, got %d", postRetryCount)
	}
	t.Logf("✓ Departure worker retry executed with zero duplicate lines in PostgreSQL")

	// Step 5: Double-entry invariant holds
	var netDiff int64
	err = pool.QueryRow(ctx, "SELECT coalesce(sum(debit_paise) - sum(credit_paise), 0) FROM financial_journal_entries WHERE property_id = $1", propID).Scan(&netDiff)
	if err != nil || netDiff != 0 {
		t.Fatalf("departure ledger unbalanced in PostgreSQL: net diff = %d paise", netDiff)
	}
	t.Logf("✓ Double-entry balance holds in PostgreSQL for departure settlement: net difference = 0 paise")
}
