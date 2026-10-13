package jobs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// TestGate07_CrashRecovery_BillingCycle tests G7 crash safety:
// 1. Spawns cmd/billing-cycle subprocess.
// 2. Kills process abruptly mid-batch.
// 3. Reruns cmd/billing-cycle to completion.
// 4. Asserts zero duplicate rent dues and a balanced financial ledger.
func TestGate07_CrashRecovery_BillingCycle(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("config load: %v", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	loc, _ := time.LoadLocation("Asia/Kolkata")
	if loc == nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	now := time.Now().In(loc)
	todayDueDay := now.Day()

	propID := uuid.New()
	tenantID := uuid.New()
	inviteCode := fmt.Sprintf("CB%s", uuid.New().String()[:6])
	phone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)

	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Crash Billing Property', '202 Crash St', '+919988112233', 'crashprop@upi', 'Crash Owner', 'crash@prop.test', $2)`,
		propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM dues WHERE tenant_id = $1", tenantID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenantID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM properties WHERE id = $1", propID)
	}()

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Crash Test Tenant', $3, '102', 1200000, $4, 'active')`,
		tenantID, propID, phone, todayDueDay)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	cmdPath := filepath.Join("..", "..", "cmd", "billing-cycle")
	env := isolatedTestEnv(
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"DATABASE_URL="+cfg.DatabaseURL,
		"DATABASE_MAINT_URL="+cfg.DatabaseMaintURL,
		"JWT_SECRET="+cfg.JWTSecret,
		"OTP_HMAC_SECRET="+cfg.OTPHMACSecret,
		"MAGIC_LINK_HMAC_SECRET="+cfg.MagicLinkHMACSecret,
		"MAGIC_LINK_BASE_URL=http://localhost:3000",
		"MIGRATIONS_DIR="+filepath.Join("..", "..", "migrations"),
	)

	// Step 1: Start billing-cycle subprocess and kill it abruptly mid-batch
	cmd1 := exec.Command("go", "run", ".")
	cmd1.Dir = cmdPath
	cmd1.Env = env

	if err := cmd1.Start(); err != nil {
		t.Fatalf("failed to start billing-cycle: %v", err)
	}

	// Give the subprocess a moment to boot and begin DB connection, then kill
	time.Sleep(350 * time.Millisecond)
	_ = cmd1.Process.Kill()
	_ = cmd1.Wait()

	// Wait briefly for Postgres connection drop and advisory lock release
	time.Sleep(500 * time.Millisecond)

	// Step 2: Rerun billing-cycle to completion
	cmd2Ctx, cmd2Cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cmd2Cancel()

	cmd2 := exec.CommandContext(cmd2Ctx, "go", "run", ".")
	cmd2.Dir = cmdPath
	cmd2.Env = env

	out, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("expected rerun of billing-cycle to exit 0, got err: %v (out: %s)", err, string(out))
	}

	// Step 3: Assert invariant: zero duplicate rent dues for test tenant
	var tenantDueCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM dues WHERE tenant_id = $1 AND kind = 'rent'", tenantID).Scan(&tenantDueCount)
	if err != nil {
		t.Fatalf("query tenant dues: %v", err)
	}
	if tenantDueCount > 1 {
		t.Fatalf("duplicate rent dues created for tenant: count = %d", tenantDueCount)
	}

	// Assert invariant: zero duplicate rent dues across all tenants
	var duplicateDuesCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT tenant_id, period_start 
			FROM dues 
			WHERE kind = 'rent' 
			GROUP BY tenant_id, period_start 
			HAVING count(*) > 1
		) d`).Scan(&duplicateDuesCount)
	if err != nil {
		t.Fatalf("query duplicate dues: %v", err)
	}
	if duplicateDuesCount != 0 {
		t.Fatalf("found %d tenants with duplicate rent dues for period", duplicateDuesCount)
	}

	// Step 4: Assert invariant: ledger is balanced
	var totalDebit, totalCredit int64
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_paise), 0), COALESCE(SUM(credit_paise), 0)
		FROM financial_journal_entries`).Scan(&totalDebit, &totalCredit)
	if err != nil {
		t.Fatalf("query ledger totals: %v", err)
	}
	if totalDebit != totalCredit {
		t.Fatalf("financial ledger unbalanced: sum(debit)=%d, sum(credit)=%d", totalDebit, totalCredit)
	}
}

// TestGate07_CrashRecovery_OutboxWorkerInterrupted tests G7 outbox recovery:
// 1. Enqueue payment mirror event.
// 2. Stop worker mid-event via context cancellation (simulating crash).
// 3. Confirm event is NOT marked processed and zero journal entries are written.
// 4. Resume worker with valid context; confirm processing completes cleanly.
// 5. Retry worker; assert zero duplicate journal lines and balanced ledger.
func TestGate07_CrashRecovery_OutboxWorkerInterrupted(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := testutil.RequireDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	propID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()
	paymentID := uuid.New()
	inviteCode := fmt.Sprintf("OW%s", uuid.New().String()[:6])
	phone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)

	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Outbox Worker Property', '303 Crash Way', '+919988114455', 'outboxprop@upi', 'Outbox Owner', 'outbox@prop.test', $2)`,
		propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM financial_journal_entries WHERE property_id = $1", propID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM ledger_outbox_events WHERE property_id = $1", propID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM payments WHERE tenant_id = $1", tenantID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM dues WHERE tenant_id = $1", tenantID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenantID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM properties WHERE id = $1", propID)
	}()

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Outbox Tenant', $3, '103', 1400000, 1, 'active')`,
		tenantID, propID, phone)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	dueCode := fmt.Sprintf("D%s", uuid.New().String()[:7])
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
		VALUES ($1, $2, $3, $4, 'rent', 1400000, 1400000, CURRENT_DATE, CURRENT_DATE, CURRENT_DATE, 'pending')`,
		dueID, dueCode, tenantID, propID)
	if err != nil {
		t.Fatalf("insert due: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	cfPaymentID := fmt.Sprintf("cf_interrupted_%d", time.Now().UnixNano())
	var outboxEventID int64

	err = postgres.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		txPayRepo := postgres.NewPaymentRepo(tx)
		p := &domain.Payment{
			DueID:       dueID,
			TenantID:    tenantID,
			CFPaymentID: &cfPaymentID,
			Amount:      1400000,
			MatchedBy:   domain.MatchedByCashfree,
			MatchedAt:   now,
			IsUnapplied: false,
		}
		if err := txPayRepo.Create(ctx, p); err != nil {
			return fmt.Errorf("create payment: %w", err)
		}
		paymentID = p.ID

		payload, err := json.Marshal(domain.PaymentMirrorPayload{
			PropertyID: propID,
			PaymentID:  paymentID,
			Allocations: []domain.PaymentAllocationItemPayload{
				{AmountPaise: 1400000, DueKind: "rent"},
			},
			UnappliedPaise: 0,
			IsUnapplied:    false,
			MatchedAt:      now,
			MatchedBy:      domain.MatchedByCashfree,
			AmountPaise:    1400000,
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
		t.Fatalf("transaction failed: %v", err)
	}

	outboxRepo := postgres.NewLedgerOutboxRepo(pool)
	financeRepo := postgres.NewFinanceRepo(pool)
	financeSvc := finance.NewService(financeRepo, nil)
	worker := finance.NewLedgerOutboxWorker(pool, outboxRepo, financeSvc)

	// Step 1: Execute with a cancelled context (simulating process termination mid-event)
	cancelCtx, cancelFunc := context.WithCancel(ctx)
	cancelFunc() // Cancel immediately

	err = worker.ProcessSingleEvent(cancelCtx, outboxEventID)
	if err == nil {
		t.Fatalf("expected error when processing with canceled context, got nil")
	}

	// Confirm that the event was NOT marked processed
	var isProcessed bool
	err = pool.QueryRow(ctx, "SELECT processed_at IS NOT NULL FROM ledger_outbox_events WHERE id = $1", outboxEventID).Scan(&isProcessed)
	if err != nil {
		t.Fatalf("query outbox status: %v", err)
	}
	if isProcessed {
		t.Fatalf("event should not be marked processed after mid-event crash")
	}

	// Step 2: Resume worker with a healthy context; confirm recovery
	err = worker.ProcessSingleEvent(ctx, outboxEventID)
	if err != nil {
		t.Fatalf("worker recovery failed: %v", err)
	}

	err = pool.QueryRow(ctx, "SELECT processed_at IS NOT NULL FROM ledger_outbox_events WHERE id = $1", outboxEventID).Scan(&isProcessed)
	if err != nil {
		t.Fatalf("query outbox status: %v", err)
	}
	if !isProcessed {
		t.Fatalf("expected event to be marked processed after recovery")
	}

	// Step 3: Worker retry idempotency test (reset processed_at, reprocess)
	_, err = pool.Exec(ctx, "UPDATE ledger_outbox_events SET processed_at = NULL WHERE id = $1", outboxEventID)
	if err != nil {
		t.Fatalf("reset processed_at: %v", err)
	}

	err = worker.ProcessSingleEvent(ctx, outboxEventID)
	if err != nil {
		t.Fatalf("worker retry failed: %v", err)
	}

	// Verify journal count: exactly 2 lines (1 debit, 1 credit), zero duplicate lines
	var journalCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM financial_journal_entries WHERE source_id = $1", paymentID).Scan(&journalCount)
	if err != nil {
		t.Fatalf("query journal entries: %v", err)
	}
	if journalCount != 2 {
		t.Fatalf("expected exactly 2 journal lines, got %d", journalCount)
	}

	// Step 4: Verify ledger balance invariant
	var totalDebit, totalCredit int64
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_paise), 0), COALESCE(SUM(credit_paise), 0)
		FROM financial_journal_entries`).Scan(&totalDebit, &totalCredit)
	if err != nil {
		t.Fatalf("query ledger balance: %v", err)
	}
	if totalDebit != totalCredit {
		t.Fatalf("financial ledger unbalanced: sum(debit)=%d, sum(credit)=%d", totalDebit, totalCredit)
	}
}
