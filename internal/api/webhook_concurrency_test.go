package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// setupWebhookTestDB bootstraps a live PostgreSQL connection and returns a cleanup func.
func setupWebhookTestDB(t *testing.T, propID uuid.UUID) (*pgxpool.Pool, *config.Config, func()) {
	_ = godotenv.Load("../../.env")
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("config load failed: %v", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("cannot connect to Postgres: %v", err))
	}

	cleanup := func() {
		_ = postgres.WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(context.Background(), "SET LOCAL app.ledger_maintenance = 'on';")
			_, _ = tx.Exec(context.Background(), "DELETE FROM financial_journal_entries WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM ledger_outbox_events WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM gateway_refunds WHERE payment_id IN (SELECT id FROM payments WHERE property_id = $1);", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payment_allocations WHERE payment_id IN (SELECT id FROM payments WHERE property_id = $1);", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM gateway_payments WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payments WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payment_intent_dues WHERE payment_intent_id IN (SELECT id FROM payment_intents WHERE due_id IN (SELECT id FROM dues WHERE property_id = $1));")
			_, _ = tx.Exec(context.Background(), "DELETE FROM payment_intents WHERE due_id IN (SELECT id FROM dues WHERE property_id = $1);")
			_, _ = tx.Exec(context.Background(), "DELETE FROM dues WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM tenants WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM users WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM properties WHERE id = $1;", propID)
			return nil
		})
		pool.Close()
	}

	return pool, cfg, cleanup
}

func seedTestFixture(ctx context.Context, pool *pgxpool.Pool, propID, ownerUserID, tenantID, dueID uuid.UUID, dueCode, orderID string, amountPaise int64) error {
	ownerPhone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	tenantPhone := fmt.Sprintf("+918%09d", time.Now().UnixNano()%1000000000)

	if _, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Gate 05 PG', '100 Webhook Way', $2, 'gate05@upi', 'Gate05 Owner', 'owner@gate05.test', $3)
	`, propID, ownerPhone, uuid.New().String()[:8]); err != nil {
		return fmt.Errorf("insert property: %w", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3)
	`, ownerUserID, ownerPhone, propID); err != nil {
		return fmt.Errorf("insert user: %w", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Gate05 Tenant', $3, '101', $4, 5, 'active')
	`, tenantID, propID, tenantPhone, amountPaise); err != nil {
		return fmt.Errorf("insert tenant: %w", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
		VALUES ($1, $2, $3, $4, 'rent', 'pending', $5, $5, CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month', CURRENT_DATE)
	`, dueID, propID, tenantID, dueCode, amountPaise); err != nil {
		return fmt.Errorf("insert due: %w", err)
	}

	exp := time.Now().Add(1 * time.Hour)
	intentRepo := postgres.NewPaymentIntentRepo(pool)
	return intentRepo.Create(ctx, &domain.PaymentIntent{
		ID:              uuid.New(),
		DueID:           dueID,
		Provider:        "cashfree",
		ProviderOrderID: orderID,
		AmountPaise:     amountPaise,
		Status:          domain.IntentCreated,
		ExpiresAt:       &exp,
	})
}

// TestGate05_SequentialDuplicateReplay verifies that receiving the same
// PAYMENT_SUCCESS_WEBHOOK three times sequentially returns HTTP 200 OK on each request
// while producing exactly one payment mutation in PostgreSQL.
func TestGate05_SequentialDuplicateReplay(t *testing.T) {
	propID := uuid.New()
	tenantID := uuid.New()
	ownerUserID := uuid.New()
	dueID := uuid.New()
	cfOrderID := fmt.Sprintf("cf_seq_%s", uuid.New().String()[:12])
	whSecret := "whsec_gate05_sequential_test_32b!"

	pool, _, cleanup := setupWebhookTestDB(t, propID)
	defer cleanup()

	ctx := context.Background()
	dueCode, err := domain.GenerateDueCode()
	if err != nil {
		t.Fatalf("generate due code failed: %v", err)
	}
	if err := seedTestFixture(ctx, pool, propID, ownerUserID, tenantID, dueID, dueCode, cfOrderID, 1200000); err != nil {
		t.Fatalf("seed fixture failed: %v", err)
	}

	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	summaryRepo := payment.NewSQLSummaryRepository(pool)
	eventsPub := &events.NoopPublisher{}
	paySvc := payment.NewService(dueRepo, paymentRepo, tenantRepo, summaryRepo, eventsPub)

	gin.SetMode(gin.TestMode)
	deps := Deps{
		JWTSecret:          "money-lifecycle-secret-32-bytes!!",
		CashfreeSecret:     whSecret,
		DueStore:           dueRepo,
		PaymentStore:       paymentRepo,
		GatewayPaymentRepo: paymentRepo,
		IntentStore:        postgres.NewPaymentIntentRepo(pool),
		Payments:           paySvc,
		Pool:               pool,
	}
	router := NewRouter(deps)

	cfPaymentID := int(time.Now().UnixNano()%899999999 + 100000000)
	payloadMap := map[string]any{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": map[string]any{
			"order": map[string]any{
				"order_id": cfOrderID,
			},
			"payment": map[string]any{
				"cf_payment_id":  cfPaymentID,
				"payment_amount": 12000.0,
				"bank_reference": fmt.Sprintf("UTR_SEQ_%d", cfPaymentID),
			},
		},
	}
	rawPayload, err := json.Marshal(payloadMap)
	if err != nil {
		t.Fatalf("marshal payload failed: %v", err)
	}

	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signWebhook(whSecret, ts, string(rawPayload))

	// Replay 3 times sequentially
	for replay := 1; replay <= 3; replay++ {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewReader(rawPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-webhook-timestamp", ts)
		req.Header.Set("x-webhook-signature", sig)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("replay %d expected HTTP 200, got %d (body: %s)", replay, rec.Code, rec.Body.String())
		}
	}

	// Assert exactly 1 payment record in PostgreSQL
	var payCount int
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM payments WHERE due_id = $1", dueID).Scan(&payCount)
	if err != nil {
		t.Fatalf("query payments count failed: %v", err)
	}
	if payCount != 1 {
		t.Fatalf("expected exactly 1 payment record for due %s, got %d", dueID, payCount)
	}

	// Assert due status is paid
	var dueStatus string
	err = pool.QueryRow(ctx, "SELECT status FROM dues WHERE id = $1", dueID).Scan(&dueStatus)
	if err != nil {
		t.Fatalf("query due status failed: %v", err)
	}
	if dueStatus != string(domain.DueStatusPaid) {
		t.Fatalf("expected due status 'paid', got %q", dueStatus)
	}
}

// TestGate05_ConcurrentWebhookStampede launches 10 concurrent goroutines
// sending the exact same webhook payload simultaneously under -race.
// Asserts zero double-credits, zero race warnings, and all requests return HTTP 200 OK.
func TestGate05_ConcurrentWebhookStampede(t *testing.T) {
	propID := uuid.New()
	tenantID := uuid.New()
	ownerUserID := uuid.New()
	dueID := uuid.New()
	cfOrderID := fmt.Sprintf("cf_conc_%s", uuid.New().String()[:12])
	whSecret := "whsec_gate05_concurrent_test_32b!"

	pool, _, cleanup := setupWebhookTestDB(t, propID)
	defer cleanup()

	ctx := context.Background()
	dueCode, err := domain.GenerateDueCode()
	if err != nil {
		t.Fatalf("generate due code failed: %v", err)
	}
	if err := seedTestFixture(ctx, pool, propID, ownerUserID, tenantID, dueID, dueCode, cfOrderID, 1800000); err != nil {
		t.Fatalf("seed fixture failed: %v", err)
	}

	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	summaryRepo := payment.NewSQLSummaryRepository(pool)
	eventsPub := &events.NoopPublisher{}
	paySvc := payment.NewService(dueRepo, paymentRepo, tenantRepo, summaryRepo, eventsPub)

	gin.SetMode(gin.TestMode)
	deps := Deps{
		JWTSecret:          "money-lifecycle-secret-32-bytes!!",
		CashfreeSecret:     whSecret,
		DueStore:           dueRepo,
		PaymentStore:       paymentRepo,
		GatewayPaymentRepo: paymentRepo,
		IntentStore:        postgres.NewPaymentIntentRepo(pool),
		Payments:           paySvc,
		Pool:               pool,
	}
	router := NewRouter(deps)

	cfPaymentID := int(time.Now().UnixNano()%899999999 + 100000000)
	payloadMap := map[string]any{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": map[string]any{
			"order": map[string]any{
				"order_id": cfOrderID,
			},
			"payment": map[string]any{
				"cf_payment_id":  cfPaymentID,
				"payment_amount": 18000.0,
				"bank_reference": fmt.Sprintf("UTR_STAMPEDE_%d", cfPaymentID),
			},
		},
	}
	rawPayload, err := json.Marshal(payloadMap)
	if err != nil {
		t.Fatalf("marshal payload failed: %v", err)
	}

	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signWebhook(whSecret, ts, string(rawPayload))

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)
	statusCodes := make([]int, concurrency)
	startBarrier := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			<-startBarrier

			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewReader(rawPayload))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-webhook-timestamp", ts)
			req.Header.Set("x-webhook-signature", sig)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			statusCodes[idx] = rec.Code
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	for idx, code := range statusCodes {
		if code != http.StatusOK {
			t.Fatalf("goroutine %d got HTTP %d, expected 200 OK", idx, code)
		}
	}

	// Invariant: Exactly one payment created
	var payCount int
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM payments WHERE due_id = $1", dueID).Scan(&payCount)
	if err != nil {
		t.Fatalf("query payments count failed: %v", err)
	}
	if payCount != 1 {
		t.Fatalf("expected exactly 1 payment record under stampede, got %d", payCount)
	}

	// Invariant: Due marked paid
	var dueStatus string
	err = pool.QueryRow(ctx, "SELECT status FROM dues WHERE id = $1", dueID).Scan(&dueStatus)
	if err != nil {
		t.Fatalf("query due status failed: %v", err)
	}
	if dueStatus != string(domain.DueStatusPaid) {
		t.Fatalf("expected due status 'paid', got %q", dueStatus)
	}
}

// TestGate05_OutOfOrderRefundBeforeSuccess verifies that receiving an auto-refund webhook
// before the payment success webhook is handled gracefully without corrupting due state,
// and subsequent payment success webhook completes properly.
func TestGate05_OutOfOrderRefundBeforeSuccess(t *testing.T) {
	propID := uuid.New()
	tenantID := uuid.New()
	ownerUserID := uuid.New()
	dueID := uuid.New()
	cfOrderID := fmt.Sprintf("cf_ooo_%s", uuid.New().String()[:12])
	whSecret := "whsec_gate05_ooo_test_32bytes!!"

	pool, _, cleanup := setupWebhookTestDB(t, propID)
	defer cleanup()

	ctx := context.Background()
	dueCode, err := domain.GenerateDueCode()
	if err != nil {
		t.Fatalf("generate due code failed: %v", err)
	}
	if err := seedTestFixture(ctx, pool, propID, ownerUserID, tenantID, dueID, dueCode, cfOrderID, 900000); err != nil {
		t.Fatalf("seed fixture failed: %v", err)
	}

	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	summaryRepo := payment.NewSQLSummaryRepository(pool)
	eventsPub := &events.NoopPublisher{}
	paySvc := payment.NewService(dueRepo, paymentRepo, tenantRepo, summaryRepo, eventsPub)

	gin.SetMode(gin.TestMode)
	deps := Deps{
		JWTSecret:          "money-lifecycle-secret-32-bytes!!",
		CashfreeSecret:     whSecret,
		DueStore:           dueRepo,
		PaymentStore:       paymentRepo,
		GatewayPaymentRepo: paymentRepo,
		IntentStore:        postgres.NewPaymentIntentRepo(pool),
		Payments:           paySvc,
		Pool:               pool,
	}
	router := NewRouter(deps)

	cfPaymentID := int(time.Now().UnixNano()%899999999 + 100000000)

	// Step 1: Send AUTO_REFUND_STATUS_WEBHOOK first (out of order before payment success)
	refundPayload := fmt.Sprintf(`{
		"type": "AUTO_REFUND_STATUS_WEBHOOK",
		"data": {
			"auto_refund": {
				"cf_refund_id": 998877,
				"refund_id": "auto_ref_ooo_1",
				"order_id": "%s",
				"cf_payment_id": %d,
				"refund_status": "SUCCESS",
				"refund_amount": 9000.00,
				"refund_type": "PAYMENT_AUTO_REFUND",
				"refund_reason": "Out of order arrival simulation"
			}
		}
	}`, cfOrderID, cfPaymentID)

	ts1 := fmt.Sprintf("%d", time.Now().Unix())
	sig1 := signWebhook(whSecret, ts1, refundPayload)

	req1 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(refundPayload))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("x-webhook-timestamp", ts1)
	req1.Header.Set("x-webhook-signature", sig1)

	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 on out-of-order refund webhook, got %d (body: %s)", rec1.Code, rec1.Body.String())
	}

	// Verify due is still in pending state (not corrupted)
	var dueStatus string
	err = pool.QueryRow(ctx, "SELECT status FROM dues WHERE id = $1", dueID).Scan(&dueStatus)
	if err != nil {
		t.Fatalf("query due status failed: %v", err)
	}
	if dueStatus != string(domain.DueStatusPending) {
		t.Fatalf("expected due to remain pending after out-of-order refund, got %q", dueStatus)
	}

	// Step 2: Now send PAYMENT_SUCCESS_WEBHOOK
	successPayloadMap := map[string]any{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": map[string]any{
			"order": map[string]any{
				"order_id": cfOrderID,
			},
			"payment": map[string]any{
				"cf_payment_id":  cfPaymentID,
				"payment_amount": 9000.0,
				"bank_reference": fmt.Sprintf("UTR_OOO_%d", cfPaymentID),
			},
		},
	}
	rawSuccess, err := json.Marshal(successPayloadMap)
	if err != nil {
		t.Fatalf("marshal success payload failed: %v", err)
	}

	ts2 := fmt.Sprintf("%d", time.Now().Unix())
	sig2 := signWebhook(whSecret, ts2, string(rawSuccess))

	req2 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewReader(rawSuccess))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-webhook-timestamp", ts2)
	req2.Header.Set("x-webhook-signature", sig2)

	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 on payment success webhook, got %d (body: %s)", rec2.Code, rec2.Body.String())
	}

	// Assert due is now marked paid
	err = pool.QueryRow(ctx, "SELECT status FROM dues WHERE id = $1", dueID).Scan(&dueStatus)
	if err != nil {
		t.Fatalf("query due status failed: %v", err)
	}
	if dueStatus != string(domain.DueStatusPaid) {
		t.Fatalf("expected due to be marked paid, got %q", dueStatus)
	}
}

// TestGate05_ZeroDeadlocksUnderConcurrentExecution verifies that concurrent payment operations
// across multiple dues do not increase the database deadlock counter in pg_stat_database.
func TestGate05_ZeroDeadlocksUnderConcurrentExecution(t *testing.T) {
	propID := uuid.New()
	pool, _, cleanup := setupWebhookTestDB(t, propID)
	defer cleanup()

	ctx := context.Background()

	// Sample initial deadlocks from pg_stat_database
	var initialDeadlocks int64
	err := pool.QueryRow(ctx, "SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()").Scan(&initialDeadlocks)
	if err != nil {
		t.Fatalf("query deadlocks failed: %v", err)
	}

	// Create 5 dues and intents
	const numDues = 5
	dueIDs := make([]uuid.UUID, numDues)
	orderIDs := make([]string, numDues)
	whSecret := "whsec_gate05_deadlocks_test_32b!"

	ownerPhone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Gate 05 DL Property', '503 Deadlock Free Way', $2, 'dl@upi', 'DL Owner', 'owner@dl.test', $3)
	`, propID, ownerPhone, uuid.New().String()[:8])
	if err != nil {
		t.Fatalf("seed property failed: %v", err)
	}

	intentRepo := postgres.NewPaymentIntentRepo(pool)

	for i := 0; i < numDues; i++ {
		tenantID := uuid.New()
		dueIDs[i] = uuid.New()
		orderIDs[i] = fmt.Sprintf("cf_dl_%d_%s", i, uuid.New().String()[:8])
		tPhone := fmt.Sprintf("+917%09d", (time.Now().UnixNano()+int64(i*1000))%1000000000)

		_, err := pool.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
			VALUES ($1, $2, $3, $4, '501', 500000, 5, 'active')
		`, tenantID, propID, fmt.Sprintf("Tenant %d", i), tPhone)
		if err != nil {
			t.Fatalf("seed tenant %d failed: %v", i, err)
		}

		dueCode, err := domain.GenerateDueCode()
		if err != nil {
			t.Fatalf("generate due code failed: %v", err)
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
			VALUES ($1, $2, $3, $4, 'rent', 'pending', 500000, 500000, CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month', CURRENT_DATE)
		`, dueIDs[i], propID, tenantID, dueCode)
		if err != nil {
			t.Fatalf("seed due %d failed: %v", i, err)
		}

		exp := time.Now().Add(1 * time.Hour)
		intent := &domain.PaymentIntent{
			ID:              uuid.New(),
			DueID:           dueIDs[i],
			Provider:        "cashfree",
			ProviderOrderID: orderIDs[i],
			AmountPaise:     500000,
			Status:          domain.IntentCreated,
			ExpiresAt:       &exp,
		}
		if err := intentRepo.Create(ctx, intent); err != nil {
			t.Fatalf("create intent %d failed: %v", i, err)
		}
	}

	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	summaryRepo := payment.NewSQLSummaryRepository(pool)
	eventsPub := &events.NoopPublisher{}
	paySvc := payment.NewService(dueRepo, paymentRepo, tenantRepo, summaryRepo, eventsPub)

	gin.SetMode(gin.TestMode)
	deps := Deps{
		JWTSecret:          "money-lifecycle-secret-32-bytes!!",
		CashfreeSecret:     whSecret,
		DueStore:           dueRepo,
		PaymentStore:       paymentRepo,
		GatewayPaymentRepo: paymentRepo,
		IntentStore:        intentRepo,
		Payments:           paySvc,
		Pool:               pool,
	}
	router := NewRouter(deps)

	// Launch concurrent requests across all 5 dues simultaneously
	const totalWorkers = 15
	var wg sync.WaitGroup
	wg.Add(totalWorkers)
	var failureCount int32
	startBarrier := make(chan struct{})

	for i := 0; i < totalWorkers; i++ {
		go func(workerIdx int) {
			defer wg.Done()
			<-startBarrier

			dueIdx := workerIdx % numDues
			ordID := orderIDs[dueIdx]
			cfPID := int(time.Now().UnixNano()%899999999 + 100000000)

			payloadMap := map[string]any{
				"type": "PAYMENT_SUCCESS_WEBHOOK",
				"data": map[string]any{
					"order": map[string]any{"order_id": ordID},
					"payment": map[string]any{
						"cf_payment_id":  cfPID,
						"payment_amount": 5000.0,
						"bank_reference": fmt.Sprintf("UTR_DL_%d", cfPID),
					},
				},
			}
			raw, _ := json.Marshal(payloadMap)
			ts := fmt.Sprintf("%d", time.Now().Unix())
			sig := signWebhook(whSecret, ts, string(raw))

			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-webhook-timestamp", ts)
			req.Header.Set("x-webhook-signature", sig)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				atomic.AddInt32(&failureCount, 1)
			}
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	if failureCount > 0 {
		t.Fatalf("%d webhook requests failed under concurrency", failureCount)
	}

	// Sample final deadlocks from pg_stat_database
	var finalDeadlocks int64
	err = pool.QueryRow(ctx, "SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()").Scan(&finalDeadlocks)
	if err != nil {
		t.Fatalf("query final deadlocks failed: %v", err)
	}

	deadlockDelta := finalDeadlocks - initialDeadlocks
	if deadlockDelta != 0 {
		t.Fatalf("deadlocks incremented during concurrent execution: delta=%d", deadlockDelta)
	}
}
