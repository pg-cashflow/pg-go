package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

func signPayoutWebhook(secret, timestamp string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(timestamp))
	h.Write(body)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func signLegacyPayoutBody(secret string, fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if strings.EqualFold(key, "signature") || strings.EqualFold(key, "event_signature") {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var postData strings.Builder
	for _, key := range keys {
		value := fmt.Sprint(fields[key])
		if value != "" && value != "<nil>" {
			postData.WriteString(value)
		}
	}
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(postData.String()))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestLivePayoutWebhookFlows(t *testing.T) {
	if testing.Short() {
		testutil.FailOnSkipIfDBRequired(t, "skipping live postgres test in short mode")
	}
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	_ = postgres.Migrate(ctx, pool, filepath.Join("..", "..", "migrations"))

	propID := uuid.New()
	inviteCode := fmt.Sprintf("WH%s", uuid.New().String()[:6])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Webhook Test PG', '789 Test Ave', '+919999955555', 'owner@upi', 'Owner', 'owner@test.com', $2)`,
		propID, inviteCode,
	)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	ownerID := uuid.New()
	_, _ = pool.Exec(ctx, `INSERT INTO users (id, phone, role, property_id) VALUES ($1, $2, 'owner', $3)`, ownerID, fmt.Sprintf("+91%s", uuid.New().String()[:10]), propID)

	financeRepo := postgres.NewFinanceRepo(pool)
	_ = financeRepo.EnsureDefaults(ctx, propID)
	finSvc := finance.NewService(financeRepo, nil)
	payoutRepo := postgres.NewPayoutRepo(pool, finSvc)

	webhookSecret := "test_payout_webhook_secret_32_bytes_long"
	outboxRepo := postgres.NewLedgerOutboxRepo(pool)
	h := &Handlers{
		Deps: Deps{
			Pool:                        pool,
			LedgerOutboxRepo:            outboxRepo,
			PayoutRepo:                  payoutRepo,
			CashfreePayoutWebhookSecret: webhookSecret,
			Finance:                     finSvc,
			WebhookToleranceSec:         300,
		},
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

	tenantID := uuid.New()
	_, _ = pool.Exec(ctx, `INSERT INTO tenants (id, property_id, name, rent_amount, due_day, status) VALUES ($1, $2, 'Departing Tenant', 1000000, 5, 'active')`, tenantID, propID)
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	}()

	// Create a test payee and items
	payee := &domain.PayoutPayee{
		PropertyID:        propID,
		TenantID:          &tenantID,
		PayeeType:         domain.PayeeTypeTenantDeposit,
		Name:              "Departing Tenant",
		AccountNumberHash: domain.ComputeAccountHash([]byte("salt"), "tenant@upi"),
		UPIVPA:            ptr("tenant@upi"),
	}
	if err := payoutRepo.CreatePayee(ctx, payee); err != nil {
		t.Fatalf("create payee: %v", err)
	}
	if _, err := payoutRepo.VerifyPayee(ctx, payee.ID, propID, ownerID); err != nil {
		t.Fatalf("verify payee: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payout_payees SET created_at = NOW() - INTERVAL '25 hours', verified_at = NOW() - INTERVAL '25 hours' WHERE id = $1`, payee.ID); err != nil {
		t.Fatalf("age payee: %v", err)
	}

	itemA := &domain.PayoutItem{
		PayeeID:         payee.ID,
		ReferenceNumber: "DEP-A-001",
		AmountPaise:     800000, // Rs 8000.00
		Purpose:         "Deposit Refund",
		Status:          domain.PayoutPending,
	}
	if err := payoutRepo.CreatePayoutItem(ctx, itemA); err != nil {
		t.Fatalf("create item A: %v", err)
	}

	itemB := &domain.PayoutItem{
		PayeeID:         payee.ID,
		ReferenceNumber: "DEP-B-002",
		AmountPaise:     400000, // Rs 4000.00
		Purpose:         "Deposit Refund",
		Status:          domain.PayoutPending,
	}
	if err := payoutRepo.CreatePayoutItem(ctx, itemB); err != nil {
		t.Fatalf("create item B: %v", err)
	}

	batchNum := fmt.Sprintf("BATCH-WH-%s", uuid.New().String()[:8])
	batch, _, err := payoutRepo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, batchNum, []byte("secret"), nil)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	_, _ = payoutRepo.ApproveBatch(ctx, batch.ID, ownerID)
	_, _, _ = payoutRepo.InitiateBatchTransferTx(ctx, batch.ID)

	cfTr1 := fmt.Sprintf("cf_tr_1_%s", uuid.New().String()[:8])
	cfTr2 := fmt.Sprintf("cf_tr_2_%s", uuid.New().String()[:8])
	cfTr3 := fmt.Sprintf("cf_tr_3_%s", uuid.New().String()[:8])
	cfTr4 := fmt.Sprintf("cf_tr_4_%s", uuid.New().String()[:8])
	cfTr5 := fmt.Sprintf("cf_tr_5_%s", uuid.New().String()[:8])

	t.Run("Invalid_Signature_Rejected_401", func(t *testing.T) {
		body := []byte(`{"event":"TRANSFER_SUCCESS"}`)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(body))
		req.Header.Set("x-webhook-signature", "invalid_sig")
		req.Header.Set("x-webhook-timestamp", ts)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized on bad sig, got %d", w.Code)
		}
	})

	t.Run("Transfer_Success_And_Idempotent_Replay", func(t *testing.T) {
		payload := PayoutWebhookEnvelope{
			Event:     "TRANSFER_ACKNOWLEDGED",
			EventTime: time.Now().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:     fmt.Sprintf("pgo_%s", itemA.ID),
			CFTransferID:   cfTr1,
			Status:         "SUCCESS",
			StatusCode:     "COMPLETED",
			TransferAmount: json.Number("8000.00"),
			UTR:            ptr("UTR88888888"),
		}
		payload.Data, _ = json.Marshal(data)
		rawBody, _ := json.Marshal(payload)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signPayoutWebhook(webhookSecret, ts, rawBody)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(rawBody))
		req.Header.Set("x-webhook-signature", sig)
		req.Header.Set("x-webhook-timestamp", ts)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}

		// Verify Item A is succeeded with UTR
		updatedItemA, err := payoutRepo.GetPayoutItemByID(ctx, itemA.ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if updatedItemA.Status != domain.PayoutSucceeded || updatedItemA.UTR == nil || *updatedItemA.UTR != "UTR88888888" {
			t.Fatalf("unexpected item state: %+v", updatedItemA)
		}

		// Replay delivery: must succeed 200 without duplicate ledger entries
		reqReplay := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(rawBody))
		reqReplay.Header.Set("x-webhook-signature", sig)
		reqReplay.Header.Set("x-webhook-timestamp", ts)
		wReplay := httptest.NewRecorder()
		router.ServeHTTP(wReplay, reqReplay)
		if wReplay.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on replay, got %d", wReplay.Code)
		}

		var eventCount, processedCount int
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM webhook_events
			WHERE provider = 'cashfree' AND signature = $1
		`, sig).Scan(&eventCount); err != nil {
			t.Fatalf("count webhook_events: %v", err)
		}
		if eventCount != 2 {
			t.Fatalf("expected 2 webhook_events rows for delivery plus replay, got %d", eventCount)
		}
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM processed_webhook_events
			WHERE provider = 'cashfree' AND event_type = 'TRANSFER_ACKNOWLEDGED' AND provider_reference_id = $1
		`, cfTr1).Scan(&processedCount); err != nil {
			t.Fatalf("count processed_webhook_events: %v", err)
		}
		if processedCount != 1 {
			t.Fatalf("expected 1 processed payout event, got %d", processedCount)
		}
	})

	t.Run("Mixed_Outcomes_Partially_Failed_Batch", func(t *testing.T) {
		// Send FAILED for Item B
		payload := PayoutWebhookEnvelope{
			Event:     "TRANSFER_FAILED",
			EventTime: time.Now().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:        fmt.Sprintf("pgo_%s", itemB.ID),
			CFTransferID:      cfTr2,
			Status:            "FAILED",
			StatusCode:        "BENE_BANK_DECLINED",
			StatusDescription: "Beneficiary bank declined transfer",
		}
		payload.Data, _ = json.Marshal(data)
		rawBody, _ := json.Marshal(payload)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signPayoutWebhook(webhookSecret, ts, rawBody)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(rawBody))
		req.Header.Set("x-webhook-signature", sig)
		req.Header.Set("x-webhook-timestamp", ts)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}

		// Verify Item B is failed
		updatedItemB, err := payoutRepo.GetPayoutItemByID(ctx, itemB.ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if updatedItemB.Status != domain.PayoutFailed {
			t.Fatalf("expected item B status failed, got %s", updatedItemB.Status)
		}

		// Verify Batch rollup: Item A succeeded + Item B failed -> partially_failed!
		reloadedBatch, err := payoutRepo.GetBatchByID(ctx, batch.ID)
		if err != nil {
			t.Fatalf("get batch: %v", err)
		}
		if reloadedBatch.Status != domain.BatchPartiallyFailed {
			t.Fatalf("expected batch status 'partially_failed', got '%s'", reloadedBatch.Status)
		}
	})

	t.Run("Transfer_Reversed_With_Ledger_Reversal", func(t *testing.T) {
		payload := PayoutWebhookEnvelope{
			Event:     "TRANSFER_REVERSED",
			EventTime: time.Now().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:        fmt.Sprintf("pgo_%s", itemA.ID),
			CFTransferID:      cfTr1,
			Status:            "REVERSED",
			StatusDescription: "Bank returned funds due to closed account",
		}
		payload.Data, _ = json.Marshal(data)
		rawBody, _ := json.Marshal(payload)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signPayoutWebhook(webhookSecret, ts, rawBody)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(rawBody))
		req.Header.Set("x-webhook-signature", sig)
		req.Header.Set("x-webhook-timestamp", ts)

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}

		updatedItemA, err := payoutRepo.GetPayoutItemByID(ctx, itemA.ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if updatedItemA.Status != domain.PayoutReversed {
			t.Fatalf("expected item A status reversed, got %s", updatedItemA.Status)
		}
	})

	t.Run("Transfer_Reversed_Ledger_Failure_Enqueues_Outbox", func(t *testing.T) {
		itemC := &domain.PayoutItem{
			PayeeID:         payee.ID,
			ReferenceNumber: "DEP-C-003",
			AmountPaise:     500000,
			Purpose:         "Deposit Refund",
			Status:          domain.PayoutProcessing,
		}
		if err := payoutRepo.CreatePayoutItem(ctx, itemC); err != nil {
			t.Fatalf("create item C: %v", err)
		}

		// Inject failing finance store to force MirrorPayoutReversed failure
		failingStore := &failingJournalStore{Store: financeRepo}
		failingFinSvc := finance.NewService(failingStore, nil)
		failingH := &Handlers{
			Deps: Deps{
				Pool:                        pool,
				LedgerOutboxRepo:            outboxRepo,
				PayoutRepo:                  payoutRepo,
				CashfreePayoutWebhookSecret: webhookSecret,
				Finance:                     failingFinSvc,
				WebhookToleranceSec:         300,
			},
		}
		failingRouter := gin.New()
		failingRouter.POST("/webhooks/cashfree/payouts", failingH.CashfreePayoutWebhook)

		payload := PayoutWebhookEnvelope{
			Event:     "TRANSFER_REVERSED",
			EventTime: time.Now().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:        fmt.Sprintf("pgo_%s", itemC.ID),
			CFTransferID:      cfTr3,
			Status:            "REVERSED",
			StatusDescription: "Bank reversal forcing ledger outbox fallback",
		}
		payload.Data, _ = json.Marshal(data)
		rawBody, _ := json.Marshal(payload)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signPayoutWebhook(webhookSecret, ts, rawBody)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(rawBody))
		req.Header.Set("x-webhook-signature", sig)
		req.Header.Set("x-webhook-timestamp", ts)

		w := httptest.NewRecorder()
		failingRouter.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}

		// Verify outbox recorded both the settlement prerequisite and the reversal event
		var settlementCount int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM ledger_outbox_events 
			WHERE event_type = $1 AND source_id = $2`,
			domain.LedgerOutboxPayoutSettlement, itemC.ID,
		).Scan(&settlementCount)
		if err != nil {
			t.Fatalf("query outbox settlement: %v", err)
		}
		if settlementCount != 1 {
			t.Fatalf("expected 1 outbox event for settled payout prerequisite, got %d", settlementCount)
		}

		var outboxCount int
		err = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM ledger_outbox_events 
			WHERE event_type = $1 AND source_id = $2`,
			domain.LedgerOutboxPayoutReversal, itemC.ID,
		).Scan(&outboxCount)
		if err != nil {
			t.Fatalf("query outbox: %v", err)
		}
		if outboxCount != 1 {
			t.Fatalf("expected 1 outbox event for reversed payout, got %d", outboxCount)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM ledger_outbox_events WHERE source_id = $1`, itemC.ID)
	})

	t.Run("Transfer_Reversed_Outbox_Failure_Returns_500", func(t *testing.T) {
		itemD := &domain.PayoutItem{
			PayeeID:         payee.ID,
			ReferenceNumber: "DEP-D-004",
			AmountPaise:     400000,
			Purpose:         "Deposit Refund",
			Status:          domain.PayoutProcessing,
		}
		if err := payoutRepo.CreatePayoutItem(ctx, itemD); err != nil {
			t.Fatalf("create item D: %v", err)
		}

		// Inject failing finance store to force MirrorPayoutReversed failure AND nil outbox repo to force outbox fallback failure
		failingStore := &failingJournalStore{Store: financeRepo}
		failingFinSvc := finance.NewService(failingStore, nil)
		failingH := &Handlers{
			Deps: Deps{
				Pool:                        pool,
				LedgerOutboxRepo:            nil, // forces outbox fallback failure
				PayoutRepo:                  payoutRepo,
				CashfreePayoutWebhookSecret: webhookSecret,
				Finance:                     failingFinSvc,
				WebhookToleranceSec:         300,
			},
		}
		failingRouter := gin.New()
		failingRouter.POST("/webhooks/cashfree/payouts", failingH.CashfreePayoutWebhook)

		payload := PayoutWebhookEnvelope{
			Event:     "TRANSFER_REVERSED",
			EventTime: time.Now().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:        fmt.Sprintf("pgo_%s", itemD.ID),
			CFTransferID:      cfTr4,
			Status:            "REVERSED",
			StatusDescription: "Bank reversal with failing outbox",
		}
		payload.Data, _ = json.Marshal(data)
		rawBody, _ := json.Marshal(payload)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signPayoutWebhook(webhookSecret, ts, rawBody)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(rawBody))
		req.Header.Set("x-webhook-signature", sig)
		req.Header.Set("x-webhook-timestamp", ts)

		w := httptest.NewRecorder()
		failingRouter.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 Internal Server Error when outbox fallback fails, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("Transfer_Success_Ledger_Failure_Enqueues_Outbox_And_Worker_Settles", func(t *testing.T) {
		itemE := &domain.PayoutItem{
			PayeeID:         payee.ID,
			ReferenceNumber: "DEP-E-005",
			AmountPaise:     650000,
			Purpose:         "Deposit Refund",
			Status:          domain.PayoutProcessing,
		}
		if err := payoutRepo.CreatePayoutItem(ctx, itemE); err != nil {
			t.Fatalf("create item E: %v", err)
		}

		// Inject failing finance store to simulate immediate journal write failure
		failingStore := &failingJournalStore{Store: financeRepo}
		failingFinSvc := finance.NewService(failingStore, nil)
		failingH := &Handlers{
			Deps: Deps{
				Pool:                        pool,
				LedgerOutboxRepo:            outboxRepo,
				PayoutRepo:                  payoutRepo,
				CashfreePayoutWebhookSecret: webhookSecret,
				Finance:                     failingFinSvc,
				WebhookToleranceSec:         300,
			},
		}
		failingRouter := gin.New()
		failingRouter.POST("/webhooks/cashfree/payouts", failingH.CashfreePayoutWebhook)

		payload := PayoutWebhookEnvelope{
			Event:     "TRANSFER_ACKNOWLEDGED",
			EventTime: time.Now().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:        fmt.Sprintf("pgo_%s", itemE.ID),
			CFTransferID:      cfTr5,
			Status:            "SUCCESS",
			StatusCode:        "COMPLETED",
			TransferAmount:    json.Number("6500.00"),
			UTR:               ptr("UTR99999999"),
			StatusDescription: "Transfer successful via outbox fallback",
		}
		payload.Data, _ = json.Marshal(data)
		rawBody, _ := json.Marshal(payload)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signPayoutWebhook(webhookSecret, ts, rawBody)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(rawBody))
		req.Header.Set("x-webhook-signature", sig)
		req.Header.Set("x-webhook-timestamp", ts)

		w := httptest.NewRecorder()
		failingRouter.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK via outbox fallback, got %d: %s", w.Code, w.Body.String())
		}

		// 1. Verify item marked succeeded
		updatedItemE, err := payoutRepo.GetPayoutItemByID(ctx, itemE.ID)
		if err != nil {
			t.Fatalf("get item: %v", err)
		}
		if updatedItemE.Status != domain.PayoutSucceeded || updatedItemE.UTR == nil || *updatedItemE.UTR != "UTR99999999" {
			t.Fatalf("unexpected item status: %+v", updatedItemE)
		}

		// 2. Verify outbox event was enqueued
		var outboxID int64
		err = pool.QueryRow(ctx, `
			SELECT id FROM ledger_outbox_events
			WHERE event_type = $1 AND source_id = $2 AND processed_at IS NULL AND failed_at IS NULL`,
			domain.LedgerOutboxPayoutSettlement, itemE.ID,
		).Scan(&outboxID)
		if err != nil {
			t.Fatalf("query outbox: %v", err)
		}

		// 3. Process the outbox event using real LedgerOutboxWorker with healthy finSvc
		worker := finance.NewLedgerOutboxWorker(pool, outboxRepo, finSvc)
		if err := worker.ProcessSingleEvent(ctx, outboxID); err != nil {
			t.Fatalf("worker process single event: %v", err)
		}

		// 4. Verify double-entry journal entries created
		var journalCount int
		err = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM financial_journal_entries
			WHERE source_type = 'payout_settlement' AND source_id = $1`,
			itemE.ID,
		).Scan(&journalCount)
		if err != nil {
			t.Fatalf("query journal: %v", err)
		}
		if journalCount != 2 {
			t.Fatalf("expected 2 journal lines (Dr refund_payable, Cr bank), got %d", journalCount)
		}

		// 5. Replay delivery: must succeed 200 without creating duplicate journal entries
		reqReplay := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(rawBody))
		reqReplay.Header.Set("x-webhook-signature", sig)
		reqReplay.Header.Set("x-webhook-timestamp", ts)
		wReplay := httptest.NewRecorder()
		router.ServeHTTP(wReplay, reqReplay)
		if wReplay.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on replay, got %d", wReplay.Code)
		}

		var replayJournalCount int
		err = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM financial_journal_entries
			WHERE source_type = 'payout_settlement' AND source_id = $1`,
			itemE.ID,
		).Scan(&replayJournalCount)
		if err != nil {
			t.Fatalf("query journal after replay: %v", err)
		}
		if replayJournalCount != 2 {
			t.Fatalf("expected still 2 journal lines after replay, got %d", replayJournalCount)
		}
	})
}

type failingJournalStore struct {
	finance.Store
}

func (s *failingJournalStore) InsertJournal(ctx context.Context, lines []domain.JournalLine) error {
	return errors.New("simulated database journal failure for outbox fallback test")
}

func ptr(s string) *string {
	return &s
}

func TestCashfreePayoutWebhookFailsClosedWithoutSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{Deps: Deps{CashfreePayoutWebhookSecret: ""}}
	r := gin.New()
	r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewBufferString(`{"transfer_id":"tr_1"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d", w.Code)
	}
}

type mockPayoutWebhookStore struct {
	getItemFunc                 func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error)
	getBatchFunc                func(ctx context.Context, id uuid.UUID) (*domain.PayoutBatch, error)
	getPayeeFunc                func(ctx context.Context, id uuid.UUID) (*domain.PayoutPayee, error)
	updateStatusFunc            func(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error
	updateBatchStatusFunc       func(ctx context.Context, tx pgx.Tx, batchID uuid.UUID) (*domain.PayoutBatch, error)
	setBatchDispatchUnknownFunc func(ctx context.Context, batchID uuid.UUID, reason string) error
	setBatchFailedFunc          func(ctx context.Context, batchID uuid.UUID, reason string) error
}

func (m *mockPayoutWebhookStore) GetPayoutItemByID(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
	if m.getItemFunc != nil {
		return m.getItemFunc(ctx, id)
	}
	return nil, nil
}

func (m *mockPayoutWebhookStore) GetBatchByID(ctx context.Context, id uuid.UUID) (*domain.PayoutBatch, error) {
	if m.getBatchFunc != nil {
		return m.getBatchFunc(ctx, id)
	}
	return nil, nil
}

func (m *mockPayoutWebhookStore) GetPayeeByID(ctx context.Context, id uuid.UUID) (*domain.PayoutPayee, error) {
	if m.getPayeeFunc != nil {
		return m.getPayeeFunc(ctx, id)
	}
	return nil, nil
}

func (m *mockPayoutWebhookStore) UpdatePayoutItemStatusTx(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
	if m.updateStatusFunc != nil {
		return m.updateStatusFunc(ctx, tx, itemID, status, cfTransferID, utr, settledAt, failureReason)
	}
	return nil
}

func (m *mockPayoutWebhookStore) UpdateBatchStatusFromItemsTx(ctx context.Context, tx pgx.Tx, batchID uuid.UUID) (*domain.PayoutBatch, error) {
	if m.updateBatchStatusFunc != nil {
		return m.updateBatchStatusFunc(ctx, tx, batchID)
	}
	return nil, nil
}

func (m *mockPayoutWebhookStore) SetBatchDispatchUnknown(ctx context.Context, batchID uuid.UUID, reason string) error {
	if m.setBatchDispatchUnknownFunc != nil {
		return m.setBatchDispatchUnknownFunc(ctx, batchID, reason)
	}
	return nil
}

func (m *mockPayoutWebhookStore) SetBatchFailed(ctx context.Context, batchID uuid.UUID, reason string) error {
	if m.setBatchFailedFunc != nil {
		return m.setBatchFailedFunc(ctx, batchID, reason)
	}
	return nil
}

func createTestSignedWebhookReq(secret string, payload PayoutWebhookEnvelope) *http.Request {
	rawBody, _ := json.Marshal(payload)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signPayoutWebhook(secret, ts, rawBody)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(rawBody))
	req.Header.Set("x-webhook-signature", sig)
	req.Header.Set("x-webhook-timestamp", ts)
	return req
}

func TestPayoutWebhook_UnitScenarios(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "unit_test_webhook_secret_32_bytes_long"
	itemID := uuid.New()
	propID := uuid.New()
	batchID := uuid.New()
	payeeID := uuid.New()

	makeEnv := func(evt string, itemID uuid.UUID) PayoutWebhookEnvelope {
		ack := 1
		data := PayoutWebhookData{
			TransferID:     fmt.Sprintf("pgo_%s", itemID),
			CFTransferID:   "cf_tr_unit_1",
			Status:         "SUCCESS",
			StatusCode:     "COMPLETED",
			TransferAmount: json.Number("10.00"),
			Acknowledged:   &ack,
		}
		dataBytes, _ := json.Marshal(data)
		return PayoutWebhookEnvelope{
			Event:     evt,
			EventTime: time.Now().UTC().Format(time.RFC3339),
			Data:      dataBytes,
		}
	}

	t.Run("nil_repository_returns_503", func(t *testing.T) {
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           nil,
				PayoutRepo:                  nil,
				Pool:                        nil,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_SUCCESS", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable, got %d", w.Code)
		}
	})

	t.Run("item_lookup_db_error_returns_500", func(t *testing.T) {
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return nil, errors.New("db query error")
			},
		}
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_SUCCESS", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 Internal Server Error, got %d", w.Code)
		}
	})

	t.Run("item_not_found_returns_200", func(t *testing.T) {
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return nil, pgx.ErrNoRows
			},
		}
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_SUCCESS", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for not found item, got %d", w.Code)
		}
	})

	t.Run("batch_lookup_db_error_returns_500", func(t *testing.T) {
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: itemID, BatchID: &batchID, AmountPaise: 1000}, nil
			},
			getBatchFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutBatch, error) {
				return nil, errors.New("batch lookup failed")
			},
		}
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_SUCCESS", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 Internal Server Error, got %d", w.Code)
		}
	})

	t.Run("payee_lookup_db_error_returns_500", func(t *testing.T) {
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: itemID, PayeeID: payeeID, AmountPaise: 1000}, nil
			},
			getPayeeFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutPayee, error) {
				return nil, errors.New("payee lookup failed")
			},
		}
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_SUCCESS", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 Internal Server Error, got %d", w.Code)
		}
	})

	t.Run("settlement_unresolved_property_returns_500", func(t *testing.T) {
		updateCalled := false
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: itemID, AmountPaise: 1000, Status: domain.PayoutProcessing}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}
		finSvc := finance.NewService(&failingJournalStore{}, nil)
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
				Finance:                     finSvc,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_SUCCESS", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 for unresolved property on settlement, got %d", w.Code)
		}
		if updateCalled {
			t.Fatal("expected status update NOT to be called when property resolution fails")
		}
	})

	t.Run("reversal_unresolved_property_returns_500", func(t *testing.T) {
		updateCalled := false
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: itemID, AmountPaise: 1000, Status: domain.PayoutSucceeded}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}
		finSvc := finance.NewService(&failingJournalStore{}, nil)
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
				Finance:                     finSvc,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_REVERSED", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 for unresolved property on reversal, got %d", w.Code)
		}
		if updateCalled {
			t.Fatal("expected status update NOT to be called when property resolution fails")
		}
	})

	t.Run("settlement_idempotency_already_succeeded_returns_200", func(t *testing.T) {
		updateCalled := false
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: itemID, AmountPaise: 1000, Status: domain.PayoutSucceeded}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}
		finSvc := finance.NewService(&failingJournalStore{}, nil)
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
				Finance:                     finSvc,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_SUCCESS", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for already succeeded item, got %d", w.Code)
		}
		if updateCalled {
			t.Fatal("expected update status NOT to be called on idempotent replay")
		}
	})

	t.Run("reversal_idempotency_already_reversed_returns_200", func(t *testing.T) {
		updateCalled := false
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: itemID, AmountPaise: 1000, Status: domain.PayoutReversed}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}
		finSvc := finance.NewService(&failingJournalStore{}, nil)
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
				Finance:                     finSvc,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_REVERSED", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for already reversed item, got %d", w.Code)
		}
		if updateCalled {
			t.Fatal("expected update status NOT to be called on idempotent replay")
		}
	})

	t.Run("settlement_ledger_mirror_fail_outbox_fail_returns_500", func(t *testing.T) {
		updateCalled := false
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: itemID, AmountPaise: 1000, Status: domain.PayoutProcessing, PayeeID: payeeID}, nil
			},
			getPayeeFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutPayee, error) {
				return &domain.PayoutPayee{ID: payeeID, PropertyID: propID}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}
		finSvc := finance.NewService(&failingJournalStore{}, nil)
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
				Finance:                     finSvc,
				LedgerOutboxRepo:            nil, // forces outbox failure
				Pool:                        nil,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		req := createTestSignedWebhookReq(secret, makeEnv("TRANSFER_SUCCESS", itemID))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 when outbox fallback fails, got %d", w.Code)
		}
		if updateCalled {
			t.Fatal("expected status update NOT to be called when outbox fallback fails")
		}
	})

	t.Run("transfer_success_without_completed_status_code_is_ignored", func(t *testing.T) {
		updateCalled := false
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: itemID, AmountPaise: 1000, Status: domain.PayoutProcessing}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
				WebhookToleranceSec:         300,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		unconfirmedEnv := PayoutWebhookEnvelope{
			Event:     "TRANSFER_SUCCESS",
			EventTime: time.Now().UTC().Format(time.RFC3339),
		}
		unconfirmedData := PayoutWebhookData{
			TransferID:   fmt.Sprintf("pgo_%s", itemID),
			CFTransferID: "cf_tr_unconfirmed",
			Status:       "SUCCESS",
			StatusCode:   "WAIT_TIME_EXCEEDED",
		}
		unconfirmedEnv.Data, _ = json.Marshal(unconfirmedData)

		req := createTestSignedWebhookReq(secret, unconfirmedEnv)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		if updateCalled {
			t.Fatal("expected status update NOT to be called when status_code is not COMPLETED")
		}
	})

	t.Run("transfer_success_with_acknowledged_zero_is_ignored", func(t *testing.T) {
		itemID := uuid.New()
		updateCalled := false
		zero := 0

		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{
					ID:          id,
					Status:      domain.PayoutProcessing,
					BatchID:     nil,
					AmountPaise: 50000,
				}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
				WebhookToleranceSec:         300,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		env := PayoutWebhookEnvelope{
			Event:     "TRANSFER_SUCCESS",
			EventTime: time.Now().UTC().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:   fmt.Sprintf("pgo_%s", itemID),
			CFTransferID: "cf_tr_unack",
			Status:       "SUCCESS",
			StatusCode:   "COMPLETED",
			Acknowledged: &zero,
		}
		env.Data, _ = json.Marshal(data)

		req := createTestSignedWebhookReq(secret, env)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		if updateCalled {
			t.Fatal("expected status update NOT to be called when acknowledged is 0")
		}
	})

	t.Run("transfer_success_without_acknowledged_flag_is_ignored", func(t *testing.T) {
		itemID := uuid.New()
		updateCalled := false

		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{
					ID:          id,
					Status:      domain.PayoutProcessing,
					BatchID:     nil,
					AmountPaise: 50000,
				}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
				WebhookToleranceSec:         300,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		env := PayoutWebhookEnvelope{
			Event:     "TRANSFER_SUCCESS",
			EventTime: time.Now().UTC().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:     fmt.Sprintf("pgo_%s", itemID),
			CFTransferID:   "cf_tr_no_ack",
			Status:         "SUCCESS",
			StatusCode:     "COMPLETED",
			TransferAmount: json.Number("500.00"),
			Acknowledged:   nil, // Missing acknowledged flag
		}
		env.Data, _ = json.Marshal(data)

		req := createTestSignedWebhookReq(secret, env)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		if updateCalled {
			t.Fatal("expected status update NOT to be called when acknowledged flag is missing on TRANSFER_SUCCESS")
		}
	})

	t.Run("transfer_acknowledged_v2_without_acknowledged_flag_reaches_settlement", func(t *testing.T) {
		itemID := uuid.New()
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{
					ID:          id,
					Status:      domain.PayoutProcessing,
					PayeeID:     payeeID,
					AmountPaise: 1000,
				}, nil
			},
			getPayeeFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutPayee, error) {
				return &domain.PayoutPayee{ID: payeeID, PropertyID: propID}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				return nil
			},
		}
		finSvc := finance.NewService(&failingJournalStore{}, nil)
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
				Finance:                     finSvc,
				LedgerOutboxRepo:            nil, // forces outbox failure after reaching settlement
				WebhookToleranceSec:         300,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		env := PayoutWebhookEnvelope{
			Event:     "TRANSFER_ACKNOWLEDGED",
			EventTime: time.Now().UTC().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:     fmt.Sprintf("pgo_%s", itemID),
			CFTransferID:   "cf_tr_v2_ack",
			Status:         "SUCCESS",
			StatusCode:     "COMPLETED",
			TransferAmount: json.Number("10.00"),
			Acknowledged:   nil, // V2 ACK event does not require legacy acknowledged flag
		}
		env.Data, _ = json.Marshal(data)

		req := createTestSignedWebhookReq(secret, env)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		// Reaching settlement without outbox repo returns 500 (proves it didn't exit early on ignored_unconfirmed)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 when attempting settlement without outbox repo, got %d (body: %s)", w.Code, w.Body.String())
		}
	})

	t.Run("bulk_transfer_rejected_sets_batch_failed", func(t *testing.T) {
		batchID := uuid.New()
		var failedBatchID uuid.UUID
		var failedReason string
		mock := &mockPayoutWebhookStore{
			setBatchFailedFunc: func(ctx context.Context, bID uuid.UUID, reason string) error {
				failedBatchID = bID
				failedReason = reason
				return nil
			},
		}
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		env := PayoutWebhookEnvelope{
			Type: "BULK_TRANSFER_REJECTED",
		}
		data := PayoutWebhookData{
			BatchTransferID:   "pgo_" + batchID.String(),
			StatusCode:        "REJECTED",
			StatusDescription: "insufficient float balance",
		}
		env.Data, _ = json.Marshal(data)

		req := createTestSignedWebhookReq(secret, env)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		if failedBatchID != batchID {
			t.Fatalf("expected batch %s to be marked failed, got %s", batchID, failedBatchID)
		}
		if !strings.Contains(failedReason, "insufficient float balance") {
			t.Fatalf("expected reason to contain failure message, got %q", failedReason)
		}
	})

	t.Run("transfer_acknowledged_ignores_terminal_item", func(t *testing.T) {
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{
					ID:      id,
					Status:  domain.PayoutFailed,
					BatchID: nil,
				}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, itemID uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				t.Fatalf("unexpected call to UpdatePayoutItemStatusTx on terminal item")
				return nil
			},
		}
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           mock,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		env := makeEnv("TRANSFER_ACKNOWLEDGED", itemID)
		req := createTestSignedWebhookReq(secret, env)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("transfer_acknowledged_settles_processing_item", func(t *testing.T) {
		var gotStatus domain.PayoutItemStatus
		var gotUTR string
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: id, AmountPaise: 1000, Status: domain.PayoutProcessing}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				gotStatus = status
				if utr != nil {
					gotUTR = *utr
				}
				return nil
			},
		}
		h := &Handlers{Deps: Deps{
			CashfreePayoutWebhookSecret: secret,
			GatewayPaymentRepo:          &stubGatewayRepo{},
			PayoutWebhookRepo:           mock,
		}}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		env := makeEnv("TRANSFER_ACKNOWLEDGED", itemID)
		var data PayoutWebhookData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		data.TransferUTR = ptr("UTR-ACK-001")
		env.Data, _ = json.Marshal(data)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, createTestSignedWebhookReq(secret, env))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if gotStatus != domain.PayoutSucceeded || gotUTR != "UTR-ACK-001" {
			t.Fatalf("ACK event did not settle payout: status=%q utr=%q", gotStatus, gotUTR)
		}
	})

	t.Run("transfer_acknowledged_with_acknowledged_status_code_settles_processing_item", func(t *testing.T) {
		var gotStatus domain.PayoutItemStatus
		var gotUTR string
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: id, AmountPaise: 1000, Status: domain.PayoutProcessing}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				gotStatus = status
				if utr != nil {
					gotUTR = *utr
				}
				return nil
			},
		}
		h := &Handlers{Deps: Deps{
			CashfreePayoutWebhookSecret: secret,
			GatewayPaymentRepo:          &stubGatewayRepo{},
			PayoutWebhookRepo:           mock,
		}}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		env := makeEnv("TRANSFER_ACKNOWLEDGED", itemID)
		var data PayoutWebhookData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		data.StatusCode = "ACKNOWLEDGED"
		data.TransferUTR = ptr("UTR-ACK-002")
		env.Data, _ = json.Marshal(data)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, createTestSignedWebhookReq(secret, env))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if gotStatus != domain.PayoutSucceeded || gotUTR != "UTR-ACK-002" {
			t.Fatalf("ACK event with ACKNOWLEDGED status_code did not settle payout: status=%q utr=%q", gotStatus, gotUTR)
		}
	})

	t.Run("transfer_settlement_amount_mismatch_is_not_applied", func(t *testing.T) {
		updateCalled := false
		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{ID: id, AmountPaise: 1000, Status: domain.PayoutProcessing}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}
		h := &Handlers{Deps: Deps{
			CashfreePayoutWebhookSecret: secret,
			GatewayPaymentRepo:          &stubGatewayRepo{},
			PayoutWebhookRepo:           mock,
		}}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		env := makeEnv("TRANSFER_ACKNOWLEDGED", itemID)
		var data PayoutWebhookData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		data.TransferAmount = json.Number("10.01")
		env.Data, _ = json.Marshal(data)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, createTestSignedWebhookReq(secret, env))
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 acknowledgement for quarantined event, got %d: %s", w.Code, w.Body.String())
		}
		if updateCalled {
			t.Fatal("amount mismatch must not mutate payout status")
		}
	})

	t.Run("low_balance_alert_returns_200", func(t *testing.T) {
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           &mockPayoutWebhookStore{},
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		env := PayoutWebhookEnvelope{
			Type: "LOW_BALANCE_ALERT",
		}
		data := PayoutWebhookData{
			StatusDescription: "Float balance dropped below threshold",
		}
		env.Data, _ = json.Marshal(data)

		req := createTestSignedWebhookReq(secret, env)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("low_balance_alert_dashboard_ping_without_signature_is_rejected", func(t *testing.T) {
		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          &stubGatewayRepo{},
				PayoutWebhookRepo:           &mockPayoutWebhookStore{},
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		env := PayoutWebhookEnvelope{
			Type: "LOW_BALANCE_ALERT",
		}
		data := PayoutWebhookData{
			StatusDescription: "Dashboard Test Ping",
		}
		env.Data, _ = json.Marshal(data)
		raw, _ := json.Marshal(env)

		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unsigned dashboard test ping, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("low_balance_alert_dashboard_test_with_body_signature_is_accepted", func(t *testing.T) {
		h := &Handlers{Deps: Deps{
			CashfreePayoutWebhookSecret: secret,
			GatewayPaymentRepo:          &stubGatewayRepo{},
			PayoutWebhookRepo:           &mockPayoutWebhookStore{},
		}}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		fields := map[string]any{"type": "LOW_BALANCE_ALERT", "timestamp": ts, "currentBalance": "8.53"}
		fields["signature"] = signLegacyPayoutBody(secret, fields)
		raw, _ := json.Marshal(fields)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for valid signed dashboard test event, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("transfer_reversed_on_terminal_failed_item_is_rejected_as_conflict", func(t *testing.T) {
		itemID := uuid.New()
		updateCalled := false
		stubAudit := &stubGatewayRepo{}

		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{
					ID:          id,
					Status:      domain.PayoutFailed,
					AmountPaise: 50000,
				}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}

		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          stubAudit,
				PayoutWebhookRepo:           mock,
				WebhookToleranceSec:         300,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		env := PayoutWebhookEnvelope{
			Event:     "TRANSFER_REVERSED",
			EventTime: time.Now().UTC().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:   fmt.Sprintf("pgo_%s", itemID),
			CFTransferID: "cf_tr_rev_conflict",
			Status:       "REVERSED",
		}
		env.Data, _ = json.Marshal(data)

		req := createTestSignedWebhookReq(secret, env)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		if updateCalled {
			t.Fatal("expected status update NOT to be called when reversal arrives on terminal failed item")
		}
		if stubAudit.lastStatus != "terminal_status_conflict" {
			t.Fatalf("expected audit status 'terminal_status_conflict', got %q", stubAudit.lastStatus)
		}
	})

	t.Run("transfer_success_on_terminal_failed_item_is_rejected_as_conflict", func(t *testing.T) {
		itemID := uuid.New()
		updateCalled := false
		stubAudit := &stubGatewayRepo{}

		mock := &mockPayoutWebhookStore{
			getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
				return &domain.PayoutItem{
					ID:          id,
					Status:      domain.PayoutFailed,
					AmountPaise: 50000,
				}, nil
			},
			updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
				updateCalled = true
				return nil
			},
		}

		h := &Handlers{
			Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				GatewayPaymentRepo:          stubAudit,
				PayoutWebhookRepo:           mock,
				WebhookToleranceSec:         300,
			},
		}
		r := gin.New()
		r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

		env := PayoutWebhookEnvelope{
			Event:     "TRANSFER_ACKNOWLEDGED",
			EventTime: time.Now().UTC().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:     fmt.Sprintf("pgo_%s", itemID),
			CFTransferID:   "cf_tr_ack_conflict",
			Status:         "SUCCESS",
			StatusCode:     "COMPLETED",
			TransferAmount: json.Number("500.00"),
		}
		env.Data, _ = json.Marshal(data)

		req := createTestSignedWebhookReq(secret, env)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		if updateCalled {
			t.Fatal("expected status update NOT to be called when success arrives on terminal failed item")
		}
		if stubAudit.lastStatus != "terminal_status_conflict" {
			t.Fatalf("expected audit status 'terminal_status_conflict', got %q", stubAudit.lastStatus)
		}
	})
}

type seqPayoutAudit struct {
	*stubGatewayRepo
	seen   map[string]int
	events int
}

func (s *seqPayoutAudit) CreateWebhookEvent(ctx context.Context, evt *domain.WebhookEvent) error {
	s.events++
	return s.stubGatewayRepo.CreateWebhookEvent(ctx, evt)
}

func (s *seqPayoutAudit) RecordProcessedEvent(_ context.Context, _, eventType, providerRefID, _ string) (bool, error) {
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	key := eventType + "|" + providerRefID
	s.seen[key]++
	return s.seen[key] == 1, nil
}

func TestPayoutWebhook_AuditAndDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "unit_test_webhook_secret_32_bytes_long"
	itemID := uuid.New()
	updates := 0
	mock := &mockPayoutWebhookStore{
		getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
			return &domain.PayoutItem{ID: itemID, AmountPaise: 1000, Status: domain.PayoutProcessing}, nil
		},
		updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfTransferID, utr *string, settledAt *time.Time, failureReason *string) error {
			updates++
			return nil
		},
	}
	audit := &seqPayoutAudit{stubGatewayRepo: &stubGatewayRepo{}}
	h := &Handlers{Deps: Deps{
		CashfreePayoutWebhookSecret: secret,
		GatewayPaymentRepo:          audit,
		PayoutWebhookRepo:           mock,
		WebhookToleranceSec:         300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
	env := PayoutWebhookEnvelope{Event: "TRANSFER_FAILED", EventTime: time.Now().UTC().Format(time.RFC3339)}
	env.Data, _ = json.Marshal(PayoutWebhookData{TransferID: "pgo_" + itemID.String(), CFTransferID: "cf_dup_1", Status: "FAILED", StatusDescription: "no"})
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, createTestSignedWebhookReq(secret, env))
		if w.Code != http.StatusOK {
			t.Fatalf("delivery %d: expected 200, got %d %s", i+1, w.Code, w.Body.String())
		}
	}
	if audit.events != 2 {
		t.Fatalf("expected 2 audit rows, got %d", audit.events)
	}
	if updates != 1 {
		t.Fatalf("expected 1 status update, got %d", updates)
	}
}

func TestPayoutWebhook_AuditDataMinimization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test_secret_32_bytes_audit_min_123"
	itemID := uuid.New()
	stubAudit := &stubGatewayRepo{}
	mockStore := &mockPayoutWebhookStore{
		getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
			return &domain.PayoutItem{
				ID:          itemID,
				Status:      domain.PayoutProcessing,
				AmountPaise: 50000,
			}, nil
		},
		updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfID, utr *string, settledAt *time.Time, failureReason *string) error {
			return nil
		},
	}

	h := &Handlers{Deps: Deps{
		CashfreePayoutWebhookSecret: secret,
		GatewayPaymentRepo:          stubAudit,
		PayoutWebhookRepo:           mockStore,
		WebhookToleranceSec:         300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

	// Construct webhook with sensitive beneficiary bank details
	rawJSON := fmt.Sprintf(`{
		"event": "TRANSFER_SUCCESS",
		"event_time": "%s",
		"data": {
			"transfer_id": "pgo_%s",
			"cf_transfer_id": "cf_tr_12345",
			"status": "SUCCESS",
			"status_code": "COMPLETED",
			"transfer_amount": "500.00",
			"utr": "UTR12345678",
			"beneficiary_details": {
				"bank_account_number": "987654321098",
				"ifsc": "HDFC0001234",
				"phone": "9999999999",
				"email": "beneficiary@example.com"
			}
		}
	}`, time.Now().UTC().Format(time.RFC3339), itemID.String())

	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signPayoutWebhook(secret, ts, []byte(rawJSON))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader([]byte(rawJSON)))
	req.Header.Set("x-webhook-signature", sig)
	req.Header.Set("x-webhook-timestamp", ts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	if len(stubAudit.webhookEvents) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(stubAudit.webhookEvents))
	}
	evt := stubAudit.webhookEvents[0]
	if evt.EventType != "TRANSFER_SUCCESS" {
		t.Errorf("expected EventType TRANSFER_SUCCESS, got %s", evt.EventType)
	}

	storedPayload := string(evt.RawPayload)
	// Verify sensitive fields are stripped
	for _, sensitive := range []string{"987654321098", "HDFC0001234", "beneficiary_details", "9999999999"} {
		if strings.Contains(storedPayload, sensitive) {
			t.Errorf("sensitive field %q must not be retained in audit raw_payload: %s", sensitive, storedPayload)
		}
	}
	// Verify operational fields are retained
	for _, operational := range []string{"pgo_" + itemID.String(), "cf_tr_12345", "SUCCESS", "COMPLETED", "UTR12345678"} {
		if !strings.Contains(storedPayload, operational) {
			t.Errorf("operational field %q must be retained in audit raw_payload: %s", operational, storedPayload)
		}
	}
}

func TestPayoutWebhook_SecretRotation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secretOld := "old_secret_key_32_bytes_long_1"
	secretNew := "new_secret_key_32_bytes_long_2"
	itemID := uuid.New()
	stubAudit := &stubGatewayRepo{}
	mockStore := &mockPayoutWebhookStore{
		getItemFunc: func(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
			return &domain.PayoutItem{
				ID:          itemID,
				Status:      domain.PayoutProcessing,
				AmountPaise: 1000,
			}, nil
		},
		updateStatusFunc: func(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.PayoutItemStatus, cfID, utr *string, settledAt *time.Time, failureReason *string) error {
			return nil
		},
	}

	h := &Handlers{Deps: Deps{
		CashfreePayoutWebhookSecrets: map[string]string{
			"v1": secretOld,
			"v2": secretNew,
		},
		GatewayPaymentRepo:  stubAudit,
		PayoutWebhookRepo:   mockStore,
		WebhookToleranceSec: 300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

	makeReq := func(sec string) *http.Request {
		raw := fmt.Sprintf(`{"event":"TRANSFER_FAILED","data":{"transfer_id":"pgo_%s","status":"FAILED"}}`, itemID)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := signPayoutWebhook(sec, ts, []byte(raw))
		req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewReader([]byte(raw)))
		req.Header.Set("x-webhook-signature", sig)
		req.Header.Set("x-webhook-timestamp", ts)
		return req
	}

	// 1. Signed with old active secret succeeds
	wOld := httptest.NewRecorder()
	r.ServeHTTP(wOld, makeReq(secretOld))
	if wOld.Code != http.StatusOK {
		t.Errorf("expected 200 OK for old secret during rotation, got %d: %s", wOld.Code, wOld.Body.String())
	}

	// 2. Signed with new secret succeeds
	wNew := httptest.NewRecorder()
	r.ServeHTTP(wNew, makeReq(secretNew))
	if wNew.Code != http.StatusOK {
		t.Errorf("expected 200 OK for new secret during rotation, got %d: %s", wNew.Code, wNew.Body.String())
	}

	// 3. Signed with untrusted secret fails 401
	wBad := httptest.NewRecorder()
	r.ServeHTTP(wBad, makeReq("wrong_untrusted_secret_key_12345"))
	if wBad.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for untrusted secret, got %d", wBad.Code)
	}
}
