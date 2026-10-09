package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

type mockMirrorer struct {
	calls          []domain.DepartureSettlementMirrorPayload
	paymentCalls   []domain.PaymentMirrorPayload
	unappliedCalls []domain.PaymentMirrorPayload
	fail           bool
}

type mockAlerter struct {
	calledWith []*domain.LedgerOutboxEvent
	lastError  string
}

func (a *mockAlerter) NotifyDeadLetter(ctx context.Context, evt *domain.LedgerOutboxEvent, failureErr string) error {
	a.calledWith = append(a.calledWith, evt)
	a.lastError = failureErr
	return nil
}

func (m *mockMirrorer) MirrorDepartureSettlement(
	ctx context.Context,
	propertyID, departureID uuid.UUID,
	depositPaise, unusedRentReversal, damagesPaise, netRefundPaise, outstandingDuesNettedPaise, receivableBalancePaise int64,
	at time.Time,
) error {
	if m.fail {
		return errors.New("simulated mirror ledger failure")
	}
	m.calls = append(m.calls, domain.DepartureSettlementMirrorPayload{
		PropertyID:                 propertyID,
		DepartureID:                departureID,
		DepositAmountPaise:         depositPaise,
		UnusedRentRefundPaise:      unusedRentReversal,
		TotalDeductions:            damagesPaise,
		NetRefundPaise:             netRefundPaise,
		OutstandingDuesNettedPaise: outstandingDuesNettedPaise,
		ReceivableBalancePaise:     receivableBalancePaise,
		OccurredAt:                 at,
	})
	return nil
}

func (m *mockMirrorer) MirrorPaymentAllocations(
	ctx context.Context,
	propertyID uuid.UUID,
	p *domain.Payment,
	allocations []PaymentAllocationItem,
	unappliedPaise int64,
) error {
	if m.fail {
		return errors.New("simulated mirror payment failure")
	}
	var allocPayloads []domain.PaymentAllocationItemPayload
	for _, a := range allocations {
		allocPayloads = append(allocPayloads, domain.PaymentAllocationItemPayload{
			AmountPaise: a.AmountPaise,
			DueKind:     string(a.DueKind),
		})
	}
	m.paymentCalls = append(m.paymentCalls, domain.PaymentMirrorPayload{
		PropertyID:     propertyID,
		PaymentID:      p.ID,
		Allocations:    allocPayloads,
		UnappliedPaise: unappliedPaise,
		IsUnapplied:    false,
		MatchedAt:      p.MatchedAt,
		MatchedBy:      p.MatchedBy,
		AmountPaise:    int64(p.Amount),
	})
	return nil
}

func (m *mockMirrorer) MirrorUnappliedPayment(
	ctx context.Context,
	propertyID, paymentID uuid.UUID,
	amountPaise int64,
	at time.Time,
) error {
	if m.fail {
		return errors.New("simulated mirror unapplied failure")
	}
	m.unappliedCalls = append(m.unappliedCalls, domain.PaymentMirrorPayload{
		PropertyID:  propertyID,
		PaymentID:   paymentID,
		AmountPaise: amountPaise,
		IsUnapplied: true,
		MatchedAt:   at,
	})
	return nil
}

func (m *mockMirrorer) MirrorDepositSettlement(
	ctx context.Context,
	propertyID, settlementID, tenantID, dueID uuid.UUID,
	originalDepositPaise, refundedPaise, deductionsPaise int64,
	at time.Time,
) error {
	if m.fail {
		return errors.New("simulated mirror deposit settlement failure")
	}
	return nil
}

func (m *mockMirrorer) MirrorRefund(
	ctx context.Context,
	propertyID, refundID uuid.UUID,
	amountPaise int64,
	isUnapplied bool,
	dueKind domain.DueKind,
	at time.Time,
) error {
	if m.fail {
		return errors.New("simulated mirror refund failure")
	}
	return nil
}

func (m *mockMirrorer) MirrorRefundAllocations(
	ctx context.Context,
	propertyID, refundID uuid.UUID,
	allocations []RefundAllocationItem,
	at time.Time,
) error {
	if m.fail {
		return errors.New("simulated mirror refund allocations failure")
	}
	return nil
}

func (m *mockMirrorer) MirrorRewardRedeem(
	ctx context.Context,
	propertyID, tenantID, redemptionID uuid.UUID,
	points int,
	amountPaise int64,
) error {
	if m.fail {
		return errors.New("simulated mirror reward redeem failure")
	}
	return nil
}

func (m *mockMirrorer) MirrorPaymentCorrection(
	ctx context.Context,
	propertyID, correctionID, originalPaymentID uuid.UUID,
	amountPaise int64,
	at time.Time,
) error {
	if m.fail {
		return errors.New("simulated mirror payment correction failure")
	}
	return nil
}

func TestLiveLedgerOutboxWorkerAndReconciliation(t *testing.T) {
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	// Ensure ledger_outbox_events table exists
	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS ledger_outbox_events (
			id BIGSERIAL PRIMARY KEY,
			event_type TEXT NOT NULL,
			property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
			source_id UUID NOT NULL,
			payload JSONB NOT NULL DEFAULT '{}'::jsonb,
			idempotency_key TEXT NOT NULL UNIQUE,
			attempt_count INT NOT NULL DEFAULT 0,
			max_attempts INT NOT NULL DEFAULT 5,
			last_error TEXT,
			next_retry_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			processed_at TIMESTAMPTZ,
			failed_at TIMESTAMPTZ
		);
		CREATE INDEX IF NOT EXISTS idx_ledger_outbox_pending
			ON ledger_outbox_events (next_retry_at, id)
			WHERE processed_at IS NULL AND failed_at IS NULL;

		CREATE TABLE IF NOT EXISTS financial_journal_entries (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
			account_code VARCHAR(64) NOT NULL,
			debit_paise BIGINT NOT NULL DEFAULT 0,
			credit_paise BIGINT NOT NULL DEFAULT 0,
			source_type VARCHAR(40) NOT NULL,
			source_id UUID NOT NULL,
			line_kind VARCHAR(40) NOT NULL,
			occurred_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`)
	if err != nil {
		t.Fatalf("ensure tables: %v", err)
	}

	propID := uuid.New()
	inviteCode := fmt.Sprintf("E%s", uuid.New().String()[:7])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Ledger Outbox PG', '789 Ledger Way', '+919999988888', 'ledger@upi', 'Ledger Owner', 'ledger@test.com', $2)`,
		propID, inviteCode,
	)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	repo := postgres.NewLedgerOutboxRepo(pool)
	mirror := &mockMirrorer{}
	alerter := &mockAlerter{}
	worker := NewLedgerOutboxWorker(pool, repo, mirror, alerter)

	depID := uuid.New()
	now := time.Now().UTC()
	payload := domain.DepartureSettlementMirrorPayload{
		PropertyID:                 propID,
		DepartureID:                depID,
		DepositAmountPaise:         1000000,
		UnusedRentRefundPaise:      100000,
		TotalDeductions:            50000,
		NetRefundPaise:             850000,
		OutstandingDuesNettedPaise: 0,
		ReceivableBalancePaise:     0,
		OccurredAt:                 now,
	}
	payloadBytes, _ := json.Marshal(payload)

	evt := &domain.LedgerOutboxEvent{
		EventType:      "departure_settlement_mirror",
		PropertyID:     propID,
		SourceID:       depID,
		Payload:        payloadBytes,
		IdempotencyKey: fmt.Sprintf("departure_settlement:%s", depID),
		MaxAttempts:    3,
	}

	// 1. Test Transactional Enqueue
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := repo.InsertLedgerOutboxEventTx(ctx, tx, evt); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert ledger outbox event tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	if evt.ID == 0 {
		t.Fatalf("expected non-zero outbox event id")
	}

	// 2. Test Worker Processing (Success path)
	err = worker.ProcessSingleEvent(ctx, evt.ID)
	if err != nil {
		t.Fatalf("process single event: %v", err)
	}
	if len(mirror.calls) != 1 {
		t.Fatalf("expected 1 mirror call, got %d", len(mirror.calls))
	}
	if mirror.calls[0].DepartureID != depID {
		t.Errorf("expected departure id %s, got %s", depID, mirror.calls[0].DepartureID)
	}

	// Verify marked processed
	var processedAt *time.Time
	err = pool.QueryRow(ctx, `SELECT processed_at FROM ledger_outbox_events WHERE id = $1`, evt.ID).Scan(&processedAt)
	if err != nil {
		t.Fatalf("query processed_at: %v", err)
	}
	if processedAt == nil {
		t.Errorf("expected processed_at to be non-nil after successful processing")
	}

	// 3. Test Failure & Exponential Backoff & Max Attempts Escalation
	failEvtID := int64(0)
	failDepID := uuid.New()
	failEvt := &domain.LedgerOutboxEvent{
		EventType:      "departure_settlement_mirror",
		PropertyID:     propID,
		SourceID:       failDepID,
		Payload:        payloadBytes,
		IdempotencyKey: fmt.Sprintf("departure_settlement:%s", failDepID),
		MaxAttempts:    2,
	}
	txFail, _ := pool.Begin(ctx)
	_ = repo.InsertLedgerOutboxEventTx(ctx, txFail, failEvt)
	_ = txFail.Commit(ctx)
	failEvtID = failEvt.ID

	mirror.fail = true // Force failure

	// First attempt failure: increments attempt count and sets retry
	_ = worker.ProcessSingleEvent(ctx, failEvtID)
	var attempts int
	var failedAt *time.Time
	var nextRetry *time.Time
	_ = pool.QueryRow(ctx, `SELECT attempt_count, failed_at, next_retry_at FROM ledger_outbox_events WHERE id = $1`, failEvtID).Scan(&attempts, &failedAt, &nextRetry)
	if attempts != 1 {
		t.Errorf("expected attempt_count 1, got %d", attempts)
	}
	if failedAt != nil {
		t.Errorf("expected failed_at nil on first attempt, got %v", failedAt)
	}
	if nextRetry == nil {
		t.Errorf("expected next_retry_at to be populated")
	}
	if len(alerter.calledWith) != 0 {
		t.Errorf("expected 0 dead-letter alerts before reaching max attempts, got %d", len(alerter.calledWith))
	}

	// Reset next_retry_at to past so it can be re-locked
	_, _ = pool.Exec(ctx, `UPDATE ledger_outbox_events SET next_retry_at = now() - interval '1 second' WHERE id = $1`, failEvtID)

	// Second attempt failure (reaches max_attempts 2): must mark failed_at (escalation)
	_ = worker.ProcessSingleEvent(ctx, failEvtID)
	_ = pool.QueryRow(ctx, `SELECT attempt_count, failed_at FROM ledger_outbox_events WHERE id = $1`, failEvtID).Scan(&attempts, &failedAt)
	if attempts != 2 {
		t.Errorf("expected attempt_count 2, got %d", attempts)
	}
	if failedAt == nil {
		t.Errorf("expected failed_at to be set after reaching max attempts (dead letter escalation)")
	}
	if len(alerter.calledWith) != 1 {
		t.Errorf("expected exactly 1 dead-letter alert on max attempts, got %d", len(alerter.calledWith))
	} else if alerter.calledWith[0].ID != failEvtID {
		t.Errorf("expected alert for event %d, got %d", failEvtID, alerter.calledWith[0].ID)
	}

	// 4. Test Secondary Reconciliation Sweep
	// Insert an un-mirrored settled departure into tenant_departures
	tenantID := uuid.New()
	tPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+9)%10000000000)
	_, _ = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
		VALUES ($1, $2, 'Reconciliation Tenant', $3, 500000, 1, 'vacated')`,
		tenantID, propID, tPhone,
	)
	unreconciledDepID := uuid.New()
	_, _ = pool.Exec(ctx, `
		INSERT INTO tenant_departures (
			id, tenant_id, property_id, notice_given_at, planned_vacate_date, actual_vacate_date,
			deposit_amount_paise, net_refund_paise, status, updated_at
		) VALUES (
			$1, $2, $3, now() - interval '5 hours', CURRENT_DATE, CURRENT_DATE,
			500000, 500000, 'approved', now() - interval '3 hours'
		)`, unreconciledDepID, tenantID, propID,
	)

	// Run reconciliation with 1 hour threshold
	reports, err := ReconcileDepartureSettlements(ctx, pool, 1*time.Hour)
	if err != nil {
		t.Fatalf("reconcile departure settlements: %v", err)
	}
	found := false
	for _, rep := range reports {
		if rep.DepartureID == unreconciledDepID {
			found = true
			if rep.AgeHours < 2.0 {
				t.Errorf("expected age > 2h, got %.2f", rep.AgeHours)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected reconciliation to catch unreconciled departure %s", unreconciledDepID)
	}
}

func TestLedgerOutboxWorker_PayoutDispatcherUnconfiguredDoesNotDeadLetter(t *testing.T) {
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres: %v", err)
	}
	defer pool.Close()

	propID := uuid.New()
	inviteCode := fmt.Sprintf("E%s", uuid.New().String()[:7])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Payout Unconfigured Test PG', '123 Safe St', '+919999977777', 'payout@upi', 'Safe Owner', 'safe@test.com', $2)`,
		propID, inviteCode,
	)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	repo := postgres.NewLedgerOutboxRepo(pool)
	alerter := &mockAlerter{}
	// Worker initialized WITHOUT PayoutDispatcher
	worker := NewLedgerOutboxWorker(pool, repo, nil, alerter)

	batchID := uuid.New()
	payloadBytes, _ := json.Marshal(map[string]any{"batch_id": batchID})
	evt := &domain.LedgerOutboxEvent{
		EventType:      "payout_batch_transfer",
		PropertyID:     propID,
		SourceID:       batchID,
		Payload:        payloadBytes,
		IdempotencyKey: fmt.Sprintf("payout_batch_transfer:%s", batchID),
		MaxAttempts:    5,
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := repo.InsertLedgerOutboxEventTx(ctx, tx, evt); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// 1. FetchCandidateIDs without payout dispatcher should NOT fetch payout_batch_transfer
	ids, err := repo.FetchPendingCandidateIDs(ctx, 100, false)
	if err != nil {
		t.Fatalf("fetch candidates: %v", err)
	}
	for _, id := range ids {
		if id == evt.ID {
			t.Errorf("expected event %d to be excluded when includePayoutBatches is false", evt.ID)
		}
	}

	// 2. Direct ProcessSingleEvent on payout_batch_transfer when dispatcher is nil must return nil without burning attempts
	err = worker.ProcessSingleEvent(ctx, evt.ID)
	if err != nil {
		t.Fatalf("expected nil error on unconfigured dispatcher, got: %v", err)
	}

	var attempts int
	var failedAt *time.Time
	err = pool.QueryRow(ctx, `SELECT attempt_count, failed_at FROM ledger_outbox_events WHERE id = $1`, evt.ID).Scan(&attempts, &failedAt)
	if err != nil {
		t.Fatalf("query event: %v", err)
	}
	if attempts != 0 {
		t.Errorf("expected 0 attempts burned, got %d", attempts)
	}
	if failedAt != nil {
		t.Errorf("expected failed_at to remain nil, got %v", failedAt)
	}
	if len(alerter.calledWith) != 0 {
		t.Errorf("expected 0 dead letter alerts, got %d", len(alerter.calledWith))
	}
}

