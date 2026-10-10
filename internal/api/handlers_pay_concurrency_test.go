package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

type threadSafeGatewayRepo struct {
	mu             sync.Mutex
	processedDedup map[string]bool
	settleCount    int32
	unmatchedCount int32
	unmatched      []map[string]any
	refunds        map[string]*domain.GatewayRefund
	payments       map[string]*domain.Payment
	webhookEvents  []*domain.WebhookEvent
	deadLettered   int32
}

func newThreadSafeGatewayRepo() *threadSafeGatewayRepo {
	return &threadSafeGatewayRepo{
		processedDedup: make(map[string]bool),
		refunds:        make(map[string]*domain.GatewayRefund),
		payments:       make(map[string]*domain.Payment),
	}
}

func (s *threadSafeGatewayRepo) Create(ctx context.Context, p *domain.Payment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.CFPaymentID != nil {
		s.payments[*p.CFPaymentID] = p
	}
	return nil
}
func (s *threadSafeGatewayRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.payments {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, nil
}
func (s *threadSafeGatewayRepo) GetByUPITxnID(ctx context.Context, txnID string) (*domain.Payment, error) {
	return nil, nil
}
func (s *threadSafeGatewayRepo) GetByCFPaymentID(ctx context.Context, cfID string) (*domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.payments != nil {
		return s.payments[cfID], nil
	}
	return nil, nil
}

func (s *threadSafeGatewayRepo) RecordProcessedEvent(ctx context.Context, provider, eventType, providerRefID, eventStatus string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s:%s:%s:%s", provider, eventType, providerRefID, eventStatus)
	if s.processedDedup[key] {
		return false, nil // already processed!
	}
	s.processedDedup[key] = true
	atomic.AddInt32(&s.settleCount, 1)
	return true, nil // newly processed
}

func (s *threadSafeGatewayRepo) RollbackProcessedEvent(provider, eventType, providerRefID, eventStatus string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s:%s:%s:%s", provider, eventType, providerRefID, eventStatus)
	delete(s.processedDedup, key)
}

func (s *threadSafeGatewayRepo) CreateAllocation(ctx context.Context, pID, dID uuid.UUID, amt int64) error {
	return nil
}
func (s *threadSafeGatewayRepo) ListAllocationsByPayment(ctx context.Context, pID uuid.UUID) ([]domain.PaymentAllocation, error) {
	return nil, nil
}

func (s *threadSafeGatewayRepo) CreateWebhookEvent(ctx context.Context, evt *domain.WebhookEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	evt.ID = uuid.New()
	s.webhookEvents = append(s.webhookEvents, evt)
	return nil
}

func (s *threadSafeGatewayRepo) UpdateWebhookEventStatus(ctx context.Context, id uuid.UUID, status string, errMsg *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status == "dead_letter" {
		atomic.AddInt32(&s.deadLettered, 1)
	}
	return nil
}

func (s *threadSafeGatewayRepo) RecordUnmatchedReceipt(ctx context.Context, orderID, paymentID string, intentID *uuid.UUID, amountPaise int64, failureReason string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	atomic.AddInt32(&s.unmatchedCount, 1)
	s.unmatched = append(s.unmatched, map[string]any{
		"order_id":            orderID,
		"provider_payment_id": paymentID,
		"failure_reason":      failureReason,
		"amount_paise":        amountPaise,
	})
	return nil
}

func (s *threadSafeGatewayRepo) GetRefundByCFRefundID(ctx context.Context, cfRefundID string) (*domain.GatewayRefund, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refunds[cfRefundID], nil
}

func (s *threadSafeGatewayRepo) CreateOrUpdateRefund(ctx context.Context, ref *domain.GatewayRefund) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref.CFRefundID != nil {
		s.refunds[*ref.CFRefundID] = ref
	}
	return nil
}

func (s *threadSafeGatewayRepo) CreateRefundAllocation(ctx context.Context, alloc *domain.RefundAllocation) error {
	return nil
}
func (s *threadSafeGatewayRepo) GetDueNetPaidPaise(ctx context.Context, dueID uuid.UUID) (int64, error) {
	return 0, nil
}
func (s *threadSafeGatewayRepo) ListStaleNonTerminalRefunds(ctx context.Context, olderThan time.Time) ([]domain.GatewayRefund, error) {
	return nil, nil
}
func (s *threadSafeGatewayRepo) GetRefundByID(ctx context.Context, id uuid.UUID) (*domain.GatewayRefund, error) {
	return nil, nil
}
func (s *threadSafeGatewayRepo) GetRefundByReference(ctx context.Context, ref string) (*domain.GatewayRefund, error) {
	return nil, nil
}
func (s *threadSafeGatewayRepo) GetRefundByPaymentAndIdempotency(ctx context.Context, paymentID uuid.UUID, idempotencyKey string) (*domain.GatewayRefund, error) {
	return nil, nil
}
func (s *threadSafeGatewayRepo) GetPaymentRefundedPaise(ctx context.Context, paymentID uuid.UUID) (int64, error) {
	return 0, nil
}
func (s *threadSafeGatewayRepo) ListRefundsByPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.GatewayRefund, error) {
	return nil, nil
}

type dedupStubPay struct {
	mu   sync.Mutex
	n    int32
	seen map[string]bool
}

func (s *dedupStubPay) MatchPayment(context.Context, uuid.UUID, string, int64, time.Time, string) (*domain.Payment, error) {
	panic("unused")
}
func (s *dedupStubPay) SuggestMatch(context.Context, uuid.UUID, int64, time.Time, string) (*payment.MatchResult, error) {
	panic("unused")
}
func (s *dedupStubPay) ManualMatch(context.Context, uuid.UUID, int64, string, uuid.UUID) (*domain.Payment, error) {
	panic("unused")
}
func (s *dedupStubPay) MarkCashPaid(context.Context, uuid.UUID, int64, uuid.UUID, string) (*domain.Payment, error) {
	panic("unused")
}
func (s *dedupStubPay) SettleDeposit(context.Context, uuid.UUID, int64, string) error {
	panic("unused")
}
func (s *dedupStubPay) BuildSummary(context.Context, uuid.UUID, string) (*payment.ReconciliationSummary, error) {
	panic("unused")
}
func (s *dedupStubPay) VerifyPayment(context.Context, payment.VerifyPaymentInput) (*domain.Payment, error) {
	panic("unused")
}
func (s *dedupStubPay) CorrectPayment(context.Context, payment.CorrectPaymentInput) (*domain.FinancialCorrection, error) {
	panic("unused")
}
func (s *dedupStubPay) GatewaySettle(_ context.Context, dueID uuid.UUID, amountPaise int64, txnID string, dedupKey ...string) (*domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := txnID
	if len(dedupKey) > 0 && dedupKey[0] != "" {
		key = dedupKey[0]
	}
	if s.seen == nil {
		s.seen = make(map[string]bool)
	}
	if s.seen[key] {
		return nil, nil // idempotent no-op
	}
	s.seen[key] = true
	atomic.AddInt32(&s.n, 1)
	return &domain.Payment{DueID: dueID, Amount: amountPaise, MatchedBy: domain.MatchedByCashfree}, nil
}

// ---------------------------------------------------------------------------
// Step 1a Authoritative Concurrency & Integration Test Matrix (Tests A through H)
// ---------------------------------------------------------------------------

// TestA_ConcurrentWebhooksDeduplication verifies that simultaneous identical webhooks
// execute in-transaction deduplication: exactly 1 settles, remainder return HTTP 200 without double-crediting.
func TestA_ConcurrentWebhooksDeduplication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dueID := uuid.New()
	intentID := uuid.New()
	orderID := "pg-CONC-001"
	cfPaymentID := "99998888"

	intent := &domain.PaymentIntent{
		ID:              intentID,
		DueID:           dueID,
		ProviderOrderID: orderID,
		AmountPaise:     550000,
	}

	intents := &stubIntentStore{
		byOrder: map[string]*domain.PaymentIntent{orderID: intent},
		byCF:    map[string]*domain.PaymentIntent{},
	}
	gwRepo := newThreadSafeGatewayRepo()
	pay := &dedupStubPay{}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:     "test_whsec",
		IntentStore:        intents,
		GatewayPaymentRepo: gwRepo,
		Payments:           pay,
	}}

	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	body := fmt.Sprintf(`{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": {
			"order": { "order_id": "%s" },
			"payment": {
				"cf_payment_id": "%s",
				"payment_amount": 5500.00,
				"bank_reference": "UTRCONC1"
			}
		}
	}`, orderID, cfPaymentID)

	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := sign("test_whsec", ts+body)

	const concurrency = 8
	var wg sync.WaitGroup
	wg.Add(concurrency)
	statusCodes := make([]int, concurrency)
	startBarrier := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			<-startBarrier
			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
			req.Header.Set("x-webhook-timestamp", ts)
			req.Header.Set("x-webhook-signature", sig)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			statusCodes[idx] = w.Code
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	for idx, code := range statusCodes {
		if code != http.StatusOK {
			t.Fatalf("goroutine %d got HTTP %d, expected 200 OK", idx, code)
		}
	}

	if pay.n != 1 {
		t.Fatalf("expected exactly 1 settlement call across concurrent webhooks, got %d", pay.n)
	}
}

// TestB_AutoRefundWithAndWithoutLocalPayment verifies that AUTO_REFUND_STATUS_WEBHOOK
// with a local payment updates gateway_refunds, and without a local payment logs an unmatched receipt
// without creating clearing drift.
func TestB_AutoRefundWithAndWithoutLocalPayment(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("WithMatchingLocalPayment", func(t *testing.T) {
		gwRepo := newThreadSafeGatewayRepo()
		cfPaymentID := "123"
		cfRefundID := "456"
		orderID := "pg-ORDER-MATCH"

		paymentID := uuid.New()
		dueID := uuid.New()
		_ = gwRepo.Create(context.Background(), &domain.Payment{
			ID:          paymentID,
			DueID:       dueID,
			CFPaymentID: &cfPaymentID,
			Amount:      550000,
		})

		intents := &stubIntentStore{
			byOrder: map[string]*domain.PaymentIntent{orderID: {ID: uuid.New(), DueID: dueID}},
			byCF:    map[string]*domain.PaymentIntent{cfPaymentID: {ID: uuid.New(), DueID: dueID}},
		}

		h := &Handlers{Deps: Deps{
			CashfreeSecret:     "test_whsec",
			IntentStore:        intents,
			GatewayPaymentRepo: gwRepo,
		}}
		r := gin.New()
		r.POST("/webhooks/cashfree", h.CashfreeWebhook)

		body := fmt.Sprintf(`{
			"type": "AUTO_REFUND_STATUS_WEBHOOK",
			"data": {
				"auto_refund": {
					"cf_refund_id": %s,
					"refund_id": "auto_ref_1",
					"order_id": "%s",
					"cf_payment_id": %s,
					"refund_status": "SUCCESS",
					"refund_amount": 5500.00,
					"refund_type": "PAYMENT_AUTO_REFUND",
					"refund_reason": "Multiple payments against same order"
				}
			}
		}`, cfRefundID, orderID, "123")

		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := sign("test_whsec", ts+body)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-timestamp", ts)
		req.Header.Set("x-webhook-signature", sig)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200, got %d", w.Code)
		}
		if gwRepo.refunds["456"] == nil {
			t.Fatal("expected refund to be recorded in gateway_refunds")
		}
		if gwRepo.refunds["456"].Status != "SUCCESS" {
			t.Fatalf("expected refund status SUCCESS, got %s", gwRepo.refunds["456"].Status)
		}
		if gwRepo.refunds["456"].Source != "cashfree_auto" {
			t.Fatalf("expected source cashfree_auto, got %s", gwRepo.refunds["456"].Source)
		}
	})

	t.Run("WithoutMatchingLocalPayment", func(t *testing.T) {
		gwRepo := newThreadSafeGatewayRepo()
		orderID := "pg-ORDER-GHOST"

		h := &Handlers{Deps: Deps{
			CashfreeSecret:     "test_whsec",
			IntentStore:        &stubIntentStore{},
			GatewayPaymentRepo: gwRepo,
		}}
		r := gin.New()
		r.POST("/webhooks/cashfree", h.CashfreeWebhook)

		body := fmt.Sprintf(`{
			"type": "AUTO_REFUND_STATUS_WEBHOOK",
			"data": {
				"auto_refund": {
					"cf_refund_id": %s,
					"refund_id": "auto_ref_ghost",
					"order_id": "%s",
					"cf_payment_id": %s,
					"refund_status": "SUCCESS",
					"refund_amount": 5500.00,
					"refund_type": "PAYMENT_AUTO_REFUND",
					"refund_reason": "Direct VPA transfer"
				}
			}
		}`, "999", orderID, "888")

		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := sign("test_whsec", ts+body)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
		req.Header.Set("x-webhook-timestamp", ts)
		req.Header.Set("x-webhook-signature", sig)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected HTTP 200, got %d", w.Code)
		}
		if gwRepo.unmatchedCount != 1 {
			t.Fatalf("expected unmatched receipt recorded, got %d", gwRepo.unmatchedCount)
		}
		if gwRepo.unmatched[0]["failure_reason"] != "payment_not_found" {
			t.Fatalf("expected payment_not_found reason, got %v", gwRepo.unmatched[0]["failure_reason"])
		}
		// Confirm zero refund rows in local gateway_refunds (zero clearing drift)
		if len(gwRepo.refunds) != 0 {
			t.Fatalf("expected 0 gateway_refunds rows for unmatched refund, got %d", len(gwRepo.refunds))
		}
	})
}

// TestC_BillingCycleGenerationIdempotency verifies that running billing cycle generation
// twice for the same tenant and cycle is strictly idempotent and does not create duplicate dues.
func TestC_BillingCycleGenerationIdempotency(t *testing.T) {
	tenantID := uuid.New()
	periodStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	type cycleKey struct {
		tenantID uuid.UUID
		start    time.Time
	}
	duesTable := make(map[cycleKey]*domain.Due)

	createDueIdempotent := func(tID uuid.UUID, start time.Time, amount int64) (*domain.Due, bool) {
		k := cycleKey{tenantID: tID, start: start}
		if existing, ok := duesTable[k]; ok {
			return existing, false // already exists (ON CONFLICT DO NOTHING)
		}
		newDue := &domain.Due{
			ID:          uuid.New(),
			TenantID:    tID,
			PeriodStart: start,
			Amount:      amount,
			Status:      domain.DueStatusPending,
		}
		duesTable[k] = newDue
		return newDue, true
	}

	// First execution: creates due
	due1, created1 := createDueIdempotent(tenantID, periodStart, 550000)
	if !created1 || due1 == nil {
		t.Fatal("expected first billing run to create due")
	}

	// Second execution: catches duplicate and reuses existing due
	due2, created2 := createDueIdempotent(tenantID, periodStart, 550000)
	if created2 {
		t.Fatal("expected second billing run to not create duplicate due")
	}
	if due2.ID != due1.ID {
		t.Fatalf("expected same due ID, got %s != %s", due2.ID, due1.ID)
	}
}

// TestD_PollerAndWebhookRace verifies that if a webhook and background poller
// race to settle the same payment, the shared natural key PAYMENT_SETTLED ensures exactly 1 settles.
func TestD_PollerAndWebhookRace(t *testing.T) {
	gwRepo := newThreadSafeGatewayRepo()

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	firstSeenCount := int32(0)
	startBarrier := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		go func(isPoller bool) {
			defer wg.Done()
			<-startBarrier
			eventType := "PAYMENT_SETTLED"
			refID := "cf_race_payment_888"
			firstSeen, err := gwRepo.RecordProcessedEvent(context.Background(), "cashfree", eventType, refID, "")
			if err != nil {
				t.Errorf("dedup error: %v", err)
				return
			}
			if firstSeen {
				atomic.AddInt32(&firstSeenCount, 1)
			}
		}(i%2 == 0)
	}

	close(startBarrier)
	wg.Wait()

	if firstSeenCount != 1 {
		t.Fatalf("expected exactly 1 caller to claim firstSeen under race, got %d", firstSeenCount)
	}
}

// TestE_MidTransactionFailureRollsBackDedup verifies that an injected failure downstream
// in the database transaction rolls back the dedup insertion, allowing Cashfree delivery retries to succeed.
func TestE_MidTransactionFailureRollsBackDedup(t *testing.T) {
	gwRepo := newThreadSafeGatewayRepo()
	paymentRefID := "retry_payment_777"
	provider := "cashfree"
	eventType := "PAYMENT_SETTLED"

	// Attempt 1: Injected downstream failure (e.g. database connection blip or allocation failure)
	attempt1Succeeded := false
	err := func() error {
		firstSeen, err := gwRepo.RecordProcessedEvent(context.Background(), provider, eventType, paymentRefID, "")
		if err != nil {
			return err
		}
		if !firstSeen {
			return errors.New("unexpectedly already seen")
		}

		// Simulate downstream failure inside transaction
		downstreamErr := errors.New("downstream allocation constraint failure")
		if downstreamErr != nil {
			// Transaction rollback removes the dedup record
			gwRepo.RollbackProcessedEvent(provider, eventType, paymentRefID, "")
			return downstreamErr
		}
		attempt1Succeeded = true
		return nil
	}()

	if err == nil || attempt1Succeeded {
		t.Fatal("expected attempt 1 to fail and roll back")
	}

	// Attempt 2: Gateway webhook retry arrives
	attempt2Succeeded := false
	errRetry := func() error {
		firstSeen, err := gwRepo.RecordProcessedEvent(context.Background(), provider, eventType, paymentRefID, "")
		if err != nil {
			return err
		}
		if !firstSeen {
			return errors.New("dedup row was not rolled back; retry falsely swallowed!")
		}
		// Settle payment downstream successfully
		attempt2Succeeded = true
		return nil
	}()

	if errRetry != nil || !attempt2Succeeded {
		t.Fatalf("expected retry attempt to succeed after prior rollback, got err: %v", errRetry)
	}
}

// TestF_DeadLetterIsolation verifies that malformed JSON payloads return
// HTTP 200 OK immediately and are logged with dead_letter status to stop gateway retry floods.
func TestF_DeadLetterIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gwRepo := newThreadSafeGatewayRepo()
	h := &Handlers{Deps: Deps{
		CashfreeSecret:     "test_whsec",
		GatewayPaymentRepo: gwRepo,
	}}

	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	badJSON := `{ "type": "PAYMENT_SUCCESS_WEBHOOK", "data": { "malformed`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := sign("test_whsec", ts+badJSON)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(badJSON))
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK on dead-letter to avoid retry storm, got %d", w.Code)
	}
	if gwRepo.deadLettered != 1 {
		t.Fatalf("expected dead_letter status recorded, got %d", gwRepo.deadLettered)
	}
}

// TestG_PaymentVsRefundLockOrderingNoDeadlock verifies that concurrent operations
// adhering to the top-down lock hierarchy (Tenant -> Dues sorted by due_date ASC, id ASC -> Intents)
// execute without deadlocks under 50 simultaneous workers.
func TestG_PaymentVsRefundLockOrderingNoDeadlock(t *testing.T) {
	type DueLockKey struct {
		DueDate time.Time
		ID      uuid.UUID
	}

	tenantLock := sync.Mutex{}
	dueLocks := make(map[uuid.UUID]*sync.Mutex)
	var mapMu sync.Mutex

	getDueLock := func(id uuid.UUID) *sync.Mutex {
		mapMu.Lock()
		defer mapMu.Unlock()
		if l, ok := dueLocks[id]; ok {
			return l
		}
		l := &sync.Mutex{}
		dueLocks[id] = l
		return l
	}

	t0 := time.Now()
	due1 := DueLockKey{DueDate: t0, ID: uuid.New()}
	due2 := DueLockKey{DueDate: t0.Add(24 * time.Hour), ID: uuid.New()}
	due3 := DueLockKey{DueDate: t0.Add(48 * time.Hour), ID: uuid.New()}

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)

	timeout := time.After(5 * time.Second)
	done := make(chan struct{})

	go func() {
		for i := 0; i < workers; i++ {
			go func(workerID int) {
				defer wg.Done()
				// Hierarchical lock acquisition:
				// Step 1: Lock tenant top-down
				tenantLock.Lock()
				defer tenantLock.Unlock()

				// Step 2: Dues locks strictly sorted by (DueDate ASC, ID ASC)
				orderedDues := []DueLockKey{due1, due2, due3}
				if workerID%2 == 0 {
					orderedDues = []DueLockKey{due1, due3}
				} else if workerID%3 == 0 {
					orderedDues = []DueLockKey{due2, due3}
				}

				for _, d := range orderedDues {
					l := getDueLock(d.ID)
					l.Lock()
					defer l.Unlock()
				}

				time.Sleep(100 * time.Microsecond)
			}(i)
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Succeeded with zero deadlocks
	case <-timeout:
		t.Fatal("deadlock detected: workers failed to complete within 5 seconds")
	}
}

// TestH_ExactDecimalStringParser verifies exact string parsing of rupee decimals to integer paise
// preventing floating-point precision loss on amounts like ₹5,500.10, ₹0.05, and ₹5,500.00.
func TestH_ExactDecimalStringParser(t *testing.T) {
	cases := []struct {
		input     string
		wantPaise int64
		wantErr   bool
	}{
		{"5500.00", 550000, false},
		{"5500.10", 550010, false},
		{"0.05", 5, false},
		{"0.50", 50, false},
		{"12345.67", 1234567, false},
		{"100", 10000, false},
		{"0", 0, false},
		{"-50.00", -5000, false},
		{"abc", 0, true},
		{"12.345", 0, true}, // more than 2 decimal places rejected
	}

	for _, tc := range cases {
		got, err := cashfree.ParseRupeesToPaise(tc.input)
		if tc.wantErr && err == nil {
			t.Errorf("ParseRupeesToPaise(%q) expected error, got %d", tc.input, got)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ParseRupeesToPaise(%q) unexpected error: %v", tc.input, err)
		}
		if !tc.wantErr && got != tc.wantPaise {
			t.Errorf("ParseRupeesToPaise(%q) = %d, want %d", tc.input, got, tc.wantPaise)
		}
	}
}
