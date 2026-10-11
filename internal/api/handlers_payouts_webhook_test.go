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
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
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

	// Create a test payee and items
	payee := &domain.PayoutPayee{
		PropertyID:        propID,
		PayeeType:         domain.PayeeTypeTenantDeposit,
		Name:              "Departing Tenant",
		AccountNumberHash: domain.ComputeAccountHash([]byte("salt"), "tenant@upi"),
		UPIVPA:            ptr("tenant@upi"),
	}
	if err := payoutRepo.CreatePayee(ctx, payee); err != nil {
		t.Fatalf("create payee: %v", err)
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
			Event:     "TRANSFER_SUCCESS",
			EventTime: time.Now().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:   fmt.Sprintf("pgo_%s", itemA.ID),
			CFTransferID: "cf_tr_111",
			Status:       "SUCCESS",
			UTR:          ptr("UTR88888888"),
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
	})

	t.Run("Mixed_Outcomes_Partially_Failed_Batch", func(t *testing.T) {
		// Send FAILED for Item B
		payload := PayoutWebhookEnvelope{
			Event:     "TRANSFER_FAILED",
			EventTime: time.Now().Format(time.RFC3339),
		}
		data := PayoutWebhookData{
			TransferID:        fmt.Sprintf("pgo_%s", itemB.ID),
			CFTransferID:      "cf_tr_222",
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
			CFTransferID:      "cf_tr_111",
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
			CFTransferID:      "cf_tr_333",
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

		// Verify outbox fallback recorded the reversal event
		var outboxCount int
		err := pool.QueryRow(ctx, `
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
			CFTransferID:      "cf_tr_444",
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
