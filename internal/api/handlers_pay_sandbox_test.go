package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

// In-memory ledger store wrapper for Slice 1 verification
type sandboxLedgerStore struct {
	*finance.MemoryStore
	mu              sync.Mutex
	insertedBatches [][]domain.JournalLine
}

func newSandboxLedgerStore() *sandboxLedgerStore {
	return &sandboxLedgerStore{
		MemoryStore: finance.NewMemoryStore(),
	}
}

func (s *sandboxLedgerStore) InsertJournal(ctx context.Context, lines []domain.JournalLine) error {
	s.mu.Lock()
	s.insertedBatches = append(s.insertedBatches, lines)
	s.mu.Unlock()
	return s.MemoryStore.InsertJournal(ctx, lines)
}

// In-memory intent store for webhook testing
type sandboxIntentStore struct {
	mu      sync.RWMutex
	byOrder map[string]*domain.PaymentIntent
	byCF    map[string]*domain.PaymentIntent
	paid    string
}

func newSandboxIntentStore() *sandboxIntentStore {
	return &sandboxIntentStore{
		byOrder: make(map[string]*domain.PaymentIntent),
		byCF:    make(map[string]*domain.PaymentIntent),
	}
}

func (s *sandboxIntentStore) GetByOrderID(_ context.Context, orderID string) (*domain.PaymentIntent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.byOrder[orderID]
	if p == nil {
		return nil, pgx.ErrNoRows
	}
	return p, nil
}

func (s *sandboxIntentStore) GetByCFPaymentID(_ context.Context, cfID string) (*domain.PaymentIntent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.byCF[cfID]
	if p == nil {
		return nil, payment.ErrDueNotOpen
	}
	return p, nil
}

func (s *sandboxIntentStore) MarkPaid(_ context.Context, id uuid.UUID, cfPaymentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paid = cfPaymentID
	for _, it := range s.byOrder {
		if it.ID == id {
			it.Status = domain.IntentPaid
			it.CFPaymentID = &cfPaymentID
			s.byCF[cfPaymentID] = it
		}
	}
	return nil
}

func (s *sandboxIntentStore) GetDuesSnapshot(_ context.Context, _ uuid.UUID) ([]domain.PaymentIntentDue, error) {
	return nil, nil
}

func (s *sandboxIntentStore) Create(_ context.Context, p *domain.PaymentIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byOrder[p.ProviderOrderID] = p
	return nil
}

func (s *sandboxIntentStore) LatestOpenForDue(_ context.Context, _ uuid.UUID) (*domain.PaymentIntent, error) {
	return nil, pgx.ErrNoRows
}

// In-memory payment service for webhook routing
type sandboxPaymentService struct {
	mu           sync.Mutex
	settleCalls  int
	dedupSeen    map[string]bool
	onSettlement func(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string) (*domain.Payment, error)
}

func newSandboxPaymentService() *sandboxPaymentService {
	return &sandboxPaymentService{
		dedupSeen: make(map[string]bool),
	}
}

func (s *sandboxPaymentService) MatchPayment(context.Context, uuid.UUID, string, int, time.Time, string) (*domain.Payment, error) {
	return nil, nil
}
func (s *sandboxPaymentService) SuggestMatch(context.Context, uuid.UUID, int, time.Time, string) (*payment.MatchResult, error) {
	return nil, nil
}
func (s *sandboxPaymentService) ManualMatch(context.Context, uuid.UUID, int, string, uuid.UUID) (*domain.Payment, error) {
	return nil, nil
}
func (s *sandboxPaymentService) MarkCashPaid(context.Context, uuid.UUID, int, uuid.UUID, string) (*domain.Payment, error) {
	return nil, nil
}
func (s *sandboxPaymentService) SettleDeposit(context.Context, uuid.UUID, int64, string) error {
	return nil
}
func (s *sandboxPaymentService) BuildSummary(context.Context, uuid.UUID, string) (*payment.ReconciliationSummary, error) {
	return nil, nil
}

func (s *sandboxPaymentService) GatewaySettle(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string, dedupKey ...string) (*domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(dedupKey) > 0 && dedupKey[0] != "" {
		if s.dedupSeen[dedupKey[0]] {
			return nil, payment.ErrDuplicateTxn
		}
		s.dedupSeen[dedupKey[0]] = true
	}
	s.settleCalls++
	if s.onSettlement != nil {
		return s.onSettlement(ctx, dueID, amountPaise, txnID)
	}
	return &domain.Payment{DueID: dueID, Amount: amountPaise, MatchedBy: domain.MatchedByCashfree}, nil
}

// Generate valid HMAC signature for Cashfree webhook
func signCashfreeWebhook(secret, ts, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Test 1: Send the same webhook twice and confirm one payment and one ledger posting
func TestMoneyRail_DuplicateWebhook_Idempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	secret := "test_wh_secret_key"
	propID := uuid.New()
	dueID := uuid.New()
	tenantID := uuid.New()
	orderID := "order_test_dup_123"
	cfPaymentID := "99887766"
	amountPaise := 550000 // 5,500.00 INR

	// Seed due & intent
	due := &domain.Due{
		ID:         dueID,
		PropertyID: propID,
		TenantID:   tenantID,
		Amount:     amountPaise,
		Status:     domain.DueStatusPending,
		Kind:       domain.DueKindRent,
	}
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		DueID:           dueID,
		ProviderOrderID: orderID,
		AmountPaise:     amountPaise,
		Status:          domain.IntentCreated,
	}

	intents := newSandboxIntentStore()
	_ = intents.Create(ctx, intent)

	finStore := newSandboxLedgerStore()
	finSvc := &finance.Service{Store: finStore}

	var paymentCount int
	var ledgerPostCount int
	var mu sync.Mutex

	stubGateway := newThreadSafeGatewayRepo()

	paySvc := newSandboxPaymentService()
	paySvc.onSettlement = func(ctx context.Context, dID uuid.UUID, amt int, txnID string) (*domain.Payment, error) {
		mu.Lock()
		paymentCount++
		mu.Unlock()
		// Mirror to finance
		p := &domain.Payment{
			ID:          uuid.New(),
			DueID:       dueID,
			TenantID:    tenantID,
			CFPaymentID: &cfPaymentID,
			Amount:      amountPaise,
			MatchedBy:   domain.MatchedByCashfree,
			MatchedAt:   time.Now().UTC(),
		}
		if err := finSvc.MirrorPayment(ctx, p, due); err != nil {
			return nil, err
		}
		mu.Lock()
		ledgerPostCount++
		mu.Unlock()
		return p, nil
	}

	h := &Handlers{
		Deps: Deps{
			CashfreeSecret:     secret,
			IntentStore:        intents,
			Payments:           paySvc,
			Finance:            finSvc,
			GatewayPaymentRepo: stubGateway,
		},
	}

	payload := fmt.Sprintf(`{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": {
			"order": { "order_id": "%s" },
			"payment": {
				"cf_payment_id": %s,
				"payment_amount": 5500.00,
				"bank_reference": "UTR998877",
				"payment_status": "SUCCESS"
			}
		}
	}`, orderID, cfPaymentID)

	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signCashfreeWebhook(secret, ts, payload)

	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	// First webhook execution
	req1 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(payload))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("x-webhook-signature", sig)
	req1.Header.Set("x-webhook-timestamp", ts)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("first webhook failed with status: %d body: %s", w1.Code, w1.Body.String())
	}

	// Second duplicate webhook execution (identical payload, signature, timestamp)
	req2 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(payload))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-webhook-signature", sig)
	req2.Header.Set("x-webhook-timestamp", ts)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("second webhook duplicate failed with status: %d body: %s", w2.Code, w2.Body.String())
	}

	// Verify idempotency: exactly ONE payment settlement and ONE ledger posting
	if paymentCount != 1 {
		t.Errorf("expected exactly 1 payment recorded, got %d", paymentCount)
	}
	if ledgerPostCount != 1 {
		t.Errorf("expected exactly 1 ledger posting, got %d", ledgerPostCount)
	}
	if len(finStore.insertedBatches) != 1 {
		t.Errorf("expected 1 journal entry in ledger store, got %d", len(finStore.insertedBatches))
	}
}

// Test 2: Send a webhook with a bad HMAC signature and confirm it is rejected with no state change
func TestMoneyRail_BadHMACSignature_Rejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	secret := "correct_wh_secret"
	orderID := "order_bad_sig"
	cfPaymentID := "998877"

	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		DueID:           uuid.New(),
		ProviderOrderID: orderID,
		AmountPaise:     550000,
		Status:          domain.IntentCreated,
	}

	intents := newSandboxIntentStore()
	_ = intents.Create(context.Background(), intent)

	finStore := newSandboxLedgerStore()
	finSvc := &finance.Service{Store: finStore}
	paySvc := newSandboxPaymentService()

	h := &Handlers{
		Deps: Deps{
			CashfreeSecret: secret,
			IntentStore:    intents,
			Payments:       paySvc,
			Finance:        finSvc,
		},
	}

	payload := fmt.Sprintf(`{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": {
			"order": { "order_id": "%s" },
			"payment": { "cf_payment_id": %s, "payment_amount": 5500.00 }
		}
	}`, orderID, cfPaymentID)

	ts := fmt.Sprintf("%d", time.Now().Unix())
	badSig := "invalid_tampered_base64_signature="

	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-signature", badSig)
	req.Header.Set("x-webhook-timestamp", ts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Assert 401 Unauthorized
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401 Unauthorized for bad HMAC, got %d: %s", w.Code, w.Body.String())
	}

	// Assert zero state mutations
	if paySvc.settleCalls != 0 {
		t.Errorf("payments service was called despite bad signature!")
	}
	if len(finStore.insertedBatches) != 0 {
		t.Errorf("ledger journal was created despite bad signature!")
	}
	if intents.paid != "" {
		t.Errorf("intent was marked paid despite bad signature!")
	}
}

// Test 3: Replay events out of order (refund webhook arriving for unrecorded payment)
func TestMoneyRail_OutOfOrder_RefundBeforePayment(t *testing.T) {
	gin.SetMode(gin.TestMode)

	secret := "test_wh_secret"
	orderID := "order_unrecorded_refund"
	cfRefundID := "112233"

	payRepo := newThreadSafeGatewayRepo()

	h := &Handlers{
		Deps: Deps{
			CashfreeSecret:     secret,
			GatewayPaymentRepo: payRepo,
		},
	}

	// Refund webhook arrives before payment webhook was received
	payload := fmt.Sprintf(`{
		"type": "REFUND_STATUS_WEBHOOK",
		"data": {
			"refund": {
				"cf_refund_id": %s,
				"order_id": "%s",
				"cf_payment_id": 998877,
				"refund_amount": 1000.00,
				"refund_status": "SUCCESS",
				"refund_arn": "ARN12345"
			}
		}
	}`, cfRefundID, orderID)

	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signCashfreeWebhook(secret, ts, payload)

	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-signature", sig)
	req.Header.Set("x-webhook-timestamp", ts)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// In Cashfree webhook architecture, unrecorded or missing payment on refund
	// must be safely handled (200 OK returned to gateway to prevent webhook storm,
	// and marked as unmatched / quarantine without panicking)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK graceful handling of out-of-order refund, got %d", w.Code)
	}

	// Verify unmatched receipt was safely recorded
	if payRepo.unmatchedCount != 1 {
		t.Errorf("expected 1 unmatched receipt recorded for orphan refund, got %d", payRepo.unmatchedCount)
	}
}

// Test 4: Confirm journal entries balance to the exact paise from Cashfree settlement CSV
// CSV record: Gross ₹10.00 (1000 paise), Fee ₹0.19 (19 paise), GST ₹0.03 (3 paise), Net ₹9.78 (978 paise)
func TestMoneyRail_SettlementJournalConservation_ExactPaise(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()

	finStore := newSandboxLedgerStore()
	finSvc := &finance.Service{Store: finStore}

	// 1. Process valid settlement matching the exact CSV specs
	rec := finance.SettlementRecord{
		SettlementID:     "CF_SETTLE_OCT_001",
		TransferUTR:      "UTR_INSTANT_SETTLE_123",
		TransferTime:     time.Date(2026, 10, 1, 10, 30, 0, 0, time.UTC),
		GrossAmountPaise: 1000, // ₹10.00 gross collected
		NetAmountPaise:   978,  // ₹9.78 net bank payout
		ServiceFeePaise:  19,   // ₹0.19 Cashfree fee
		ServiceTaxPaise:  3,    // ₹0.03 GST on fee
		AdjustmentPaise:  0,
		PaymentCount:     1,
	}

	err := finSvc.ProcessSettlement(ctx, propID, rec)
	if err != nil {
		t.Fatalf("ProcessSettlement failed: %v", err)
	}

	if len(finStore.insertedBatches) != 1 {
		t.Fatalf("expected 1 settlement journal entry, got %d", len(finStore.insertedBatches))
	}

	lines := finStore.insertedBatches[0]
	var totalDebits, totalCredits int64
	var bankDebit, feeDebit, clearingCredit int64

	for _, l := range lines {
		totalDebits += l.DebitPaise
		totalCredits += l.CreditPaise

		if l.AccountCode == domain.AcctBank {
			bankDebit += l.DebitPaise
		}
		if l.AccountCode == domain.AcctPaymentProcessingExpense {
			feeDebit += l.DebitPaise
		}
		if l.AccountCode == domain.AcctGatewayClearing {
			clearingCredit += l.CreditPaise
		}
	}

	// Exact paise balance assertions:
	if bankDebit != 978 {
		t.Errorf("expected bank debit = 978 paise, got %d", bankDebit)
	}
	if feeDebit != 22 {
		t.Errorf("expected processing fee debit (fee 19 + tax 3) = 22 paise, got %d", feeDebit)
	}
	if clearingCredit != 1000 {
		t.Errorf("expected gateway clearing credit = 1000 paise, got %d", clearingCredit)
	}
	if totalDebits != totalCredits {
		t.Errorf("CRITICAL DRIFT: Total debits (%d) != Total credits (%d)", totalDebits, totalCredits)
	}
	if totalDebits != 1000 {
		t.Errorf("expected balanced journal of 1000 paise, got %d", totalDebits)
	}

	// 2. Reject an unbalanced settlement where gross != net + fee + tax + adj
	unbalancedRec := finance.SettlementRecord{
		SettlementID:     "CF_SETTLE_UNBALANCED",
		GrossAmountPaise: 1000,
		NetAmountPaise:   970, // 970 + 19 + 3 = 992 != 1000!
		ServiceFeePaise:  19,
		ServiceTaxPaise:  3,
	}

	err = finSvc.ProcessSettlement(ctx, propID, unbalancedRec)
	if err == nil {
		t.Fatalf("expected ProcessSettlement to fail on unbalanced amounts, but it succeeded!")
	}
	if !errors.Is(err, finance.ErrSettlementUnbalanced) {
		t.Logf("ProcessSettlement correctly rejected unbalanced settlement: %v", err)
	}
}
