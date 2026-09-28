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
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type stubIntentStore struct {
	mu        sync.RWMutex
	byOrder   map[string]*domain.PaymentIntent
	byCF      map[string]*domain.PaymentIntent
	paid      string
	lookupErr error
}

func (s *stubIntentStore) GetByOrderID(_ context.Context, orderID string) (*domain.PaymentIntent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	p := s.byOrder[orderID]
	if p == nil {
		return nil, pgx.ErrNoRows
	}
	return p, nil
}
func (s *stubIntentStore) GetByCFPaymentID(_ context.Context, cfID string) (*domain.PaymentIntent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.byCF[cfID]
	if p == nil {
		return nil, payment.ErrDueNotOpen
	}
	return p, nil
}
func (s *stubIntentStore) MarkPaid(_ context.Context, id uuid.UUID, cfPaymentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paid = cfPaymentID
	return nil
}
func (s *stubIntentStore) GetDuesSnapshot(_ context.Context, _ uuid.UUID) ([]domain.PaymentIntentDue, error) {
	return nil, nil
}
func (s *stubIntentStore) Create(_ context.Context, p *domain.PaymentIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byOrder == nil {
		s.byOrder = make(map[string]*domain.PaymentIntent)
	}
	s.byOrder[p.ProviderOrderID] = p
	return nil
}
func (s *stubIntentStore) LatestOpenForDue(_ context.Context, _ uuid.UUID) (*domain.PaymentIntent, error) {
	return nil, pgx.ErrNoRows
}


type stubPay struct {
	n         int
	txn       string
	dedup     string
	settleErr error
}

func (s *stubPay) MatchPayment(context.Context, uuid.UUID, string, int, time.Time, string) (*domain.Payment, error) {
	panic("unused")
}
func (s *stubPay) SuggestMatch(context.Context, uuid.UUID, int, time.Time, string) (*payment.MatchResult, error) {
	panic("unused")
}
func (s *stubPay) ManualMatch(context.Context, uuid.UUID, int, string, uuid.UUID) (*domain.Payment, error) {
	panic("unused")
}
func (s *stubPay) MarkCashPaid(context.Context, uuid.UUID, int, uuid.UUID, string) (*domain.Payment, error) {
	panic("unused")
}
func (s *stubPay) SettleDeposit(context.Context, uuid.UUID, int64, string) error { panic("unused") }
func (s *stubPay) BuildSummary(context.Context, uuid.UUID, string) (*payment.ReconciliationSummary, error) {
	panic("unused")
}
func (s *stubPay) GatewaySettle(_ context.Context, dueID uuid.UUID, amountPaise int, txnID string, dedupKey ...string) (*domain.Payment, error) {
	s.n++
	s.txn = txnID
	if len(dedupKey) > 0 {
		s.dedup = dedupKey[0]
	}
	if s.settleErr != nil {
		return nil, s.settleErr
	}
	return &domain.Payment{DueID: dueID, Amount: amountPaise, MatchedBy: domain.MatchedByCashfree}, nil
}

func TestCashfreeWebhookSettlesSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dueID := uuid.New()
	intent := &domain.PaymentIntent{ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-ABC-1", AmountPaise: 1050}
	intents := &stubIntentStore{
		byOrder: map[string]*domain.PaymentIntent{"pg-ABC-1": intent},
		byCF:    map[string]*domain.PaymentIntent{},
	}
	pay := &stubPay{}
	h := &Handlers{Deps: Deps{
		CashfreeSecret: "whsec",
		IntentStore:    intents,
		Payments:       pay,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"pg-ABC-1"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`
	ts := "100"
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sign("whsec", ts+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if pay.n != 1 || pay.txn != "UTR1" || intents.paid != "9" || pay.dedup != "cashfree:pg:9" {
		t.Fatalf("pay=%+v paid=%s", pay, intents.paid)
	}
}

func TestCashfreeWebhookRejectsBadHMAC(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{Deps: Deps{CashfreeSecret: "whsec"}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(`{}`))
	req.Header.Set("x-webhook-timestamp", "1")
	req.Header.Set("x-webhook-signature", "nope")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestCashfreeWebhookDormantWithoutSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{Deps: Deps{}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(`{"type":"PAYMENT_SUCCESS_WEBHOOK"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestCashfreeWebhookUnknownOrderOK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pay := &stubPay{}
	h := &Handlers{Deps: Deps{
		CashfreeSecret: "whsec",
		IntentStore:    &stubIntentStore{byOrder: map[string]*domain.PaymentIntent{}, byCF: map[string]*domain.PaymentIntent{}},
		Payments:       pay,
	}}
	code := postSignedWebhook(t, h, `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"missing"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`)
	if code != http.StatusOK || pay.n != 0 {
		t.Fatalf("code=%d n=%d", code, pay.n)
	}
}

func TestCashfreeWebhookLookupError500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{Deps: Deps{
		CashfreeSecret: "whsec",
		IntentStore:    &stubIntentStore{lookupErr: errors.New("db down")},
		Payments:       &stubPay{},
	}}
	code := postSignedWebhook(t, h, `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"pg-ABC-1"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("code=%d", code)
	}
}

func TestCashfreeWebhookAmountMismatchDoesNotMarkPaid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dueID := uuid.New()
	intent := &domain.PaymentIntent{ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-ABC-1", AmountPaise: 9999}
	intents := &stubIntentStore{
		byOrder: map[string]*domain.PaymentIntent{"pg-ABC-1": intent},
		byCF:    map[string]*domain.PaymentIntent{},
	}
	pay := &stubPay{}
	h := &Handlers{Deps: Deps{CashfreeSecret: "whsec", IntentStore: intents, Payments: pay}}
	code := postSignedWebhook(t, h, `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"pg-ABC-1"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`)
	if code != http.StatusOK || pay.n != 0 || intents.paid != "" {
		t.Fatalf("code=%d n=%d paid=%s", code, pay.n, intents.paid)
	}
}

func TestCashfreeWebhookDueNotOpenDoesNotMarkPaid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dueID := uuid.New()
	intent := &domain.PaymentIntent{ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-ABC-1", AmountPaise: 1050}
	intents := &stubIntentStore{
		byOrder: map[string]*domain.PaymentIntent{"pg-ABC-1": intent},
		byCF:    map[string]*domain.PaymentIntent{},
	}
	pay := &stubPay{settleErr: payment.ErrDueNotOpen}
	h := &Handlers{Deps: Deps{CashfreeSecret: "whsec", IntentStore: intents, Payments: pay}}
	code := postSignedWebhook(t, h, `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"pg-ABC-1"},"payment":{"cf_payment_id":"9","payment_amount":10.5,"bank_reference":"UTR1"}}}`)
	if code != http.StatusOK || intents.paid != "" {
		t.Fatalf("code=%d paid=%s", code, intents.paid)
	}
}

type stubGatewayRepo struct {
	webhookEvents []*domain.WebhookEvent
	deadLettered  bool
	lastStatus    string
	refunds       map[string]*domain.GatewayRefund
}

func (s *stubGatewayRepo) Create(context.Context, *domain.Payment) error { return nil }
func (s *stubGatewayRepo) GetByID(context.Context, uuid.UUID) (*domain.Payment, error) { return nil, nil }
func (s *stubGatewayRepo) GetByUPITxnID(context.Context, string) (*domain.Payment, error) { return nil, nil }
func (s *stubGatewayRepo) GetByCFPaymentID(context.Context, string) (*domain.Payment, error) { return nil, nil }
func (s *stubGatewayRepo) RecordProcessedEvent(context.Context, string, string, string, string) (bool, error) { return true, nil }
func (s *stubGatewayRepo) CreateAllocation(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil }
func (s *stubGatewayRepo) ListAllocationsByPayment(context.Context, uuid.UUID) ([]domain.PaymentAllocation, error) { return nil, nil }
func (s *stubGatewayRepo) CreateWebhookEvent(_ context.Context, evt *domain.WebhookEvent) error {
	evt.ID = uuid.New()
	s.webhookEvents = append(s.webhookEvents, evt)
	return nil
}
func (s *stubGatewayRepo) UpdateWebhookEventStatus(_ context.Context, _ uuid.UUID, status string, _ *string) error {
	s.lastStatus = status
	if status == "dead_letter" {
		s.deadLettered = true
	}
	return nil
}
func (s *stubGatewayRepo) RecordUnmatchedReceipt(context.Context, string, string, *uuid.UUID, int64, string, []byte) error { return nil }
func (s *stubGatewayRepo) GetRefundByCFRefundID(_ context.Context, cfRefundID string) (*domain.GatewayRefund, error) {
	if s.refunds != nil {
		return s.refunds[cfRefundID], nil
	}
	return nil, nil
}
func (s *stubGatewayRepo) CreateOrUpdateRefund(_ context.Context, ref *domain.GatewayRefund) error {
	if s.refunds == nil {
		s.refunds = make(map[string]*domain.GatewayRefund)
	}
	if ref.CFRefundID != nil {
		s.refunds[*ref.CFRefundID] = ref
	}
	return nil
}
func (s *stubGatewayRepo) CreateRefundAllocation(context.Context, *domain.RefundAllocation) error { return nil }
func (s *stubGatewayRepo) GetDueNetPaidPaise(context.Context, uuid.UUID) (int64, error) { return 0, nil }
func (s *stubGatewayRepo) ListStaleNonTerminalRefunds(context.Context, time.Time) ([]domain.GatewayRefund, error) { return nil, nil }
func (s *stubGatewayRepo) GetRefundByID(context.Context, uuid.UUID) (*domain.GatewayRefund, error) { return nil, nil }
func (s *stubGatewayRepo) GetRefundByReference(context.Context, string) (*domain.GatewayRefund, error) { return nil, nil }
func (s *stubGatewayRepo) GetRefundByPaymentAndIdempotency(context.Context, uuid.UUID, string) (*domain.GatewayRefund, error) { return nil, nil }
func (s *stubGatewayRepo) GetPaymentRefundedPaise(context.Context, uuid.UUID) (int64, error) { return 0, nil }
func (s *stubGatewayRepo) ListRefundsByPayment(context.Context, uuid.UUID) ([]domain.GatewayRefund, error) { return nil, nil }

func TestCashfreeWebhookDeadLetterReturns200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gwRepo := &stubGatewayRepo{}
	h := &Handlers{Deps: Deps{
		CashfreeSecret:     "whsec",
		GatewayPaymentRepo: gwRepo,
		IntentStore:        &stubIntentStore{},
	}}

	// Malformed JSON payload
	badJSON := `{ "type": "PAYMENT_SUCCESS_WEBHOOK", "data": { "unclosed`
	code := postSignedWebhook(t, h, badJSON)
	if code != http.StatusOK {
		t.Fatalf("expected 200 OK on dead-letter to prevent retry flood, got %d", code)
	}
	if !gwRepo.deadLettered {
		t.Fatal("expected webhook to be marked dead_letter in audit log")
	}
}

func TestCashfreeWebhookTimestampDrift401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{Deps: Deps{
		CashfreeSecret:      "whsec",
		WebhookToleranceSec: 300,
	}}
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	// Old timestamp (600s in the past)
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", oldTS)
	req.Header.Set("x-webhook-signature", sign("whsec", oldTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for timestamp drift > 300s, got %d", w.Code)
	}
}

type stubOutboxStore struct {
	events []*domain.OutboxEvent
}

func (s *stubOutboxStore) InsertEvent(_ context.Context, evt *domain.OutboxEvent) error {
	s.events = append(s.events, evt)
	return nil
}

type stubPayDueStore struct {
	dues map[uuid.UUID]*domain.Due
}

func (s *stubPayDueStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	if s.dues != nil {
		return s.dues[id], nil
	}
	return nil, nil
}
func (s *stubPayDueStore) List(context.Context, postgres.DueListFilter) ([]domain.Due, error) {
	return nil, nil
}
func (s *stubPayDueStore) ListByTenant(context.Context, uuid.UUID) ([]domain.Due, error) {
	return nil, nil
}

func TestCashfreeWebhook_DisputeFailSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gwRepo := &stubGatewayRepo{}
	outbox := &stubOutboxStore{}
	propID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()
	dueStore := &stubPayDueStore{
		dues: map[uuid.UUID]*domain.Due{
			dueID: {
				ID:         dueID,
				PropertyID: propID,
				TenantID:   tenantID,
			},
		},
	}
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		DueID:           dueID,
		ProviderOrderID: "order_disp_123",
	}
	intents := &stubIntentStore{
		byOrder: map[string]*domain.PaymentIntent{"order_disp_123": intent},
		byCF:    map[string]*domain.PaymentIntent{},
	}

	h := &Handlers{Deps: Deps{
		CashfreeSecret:     "whsec",
		GatewayPaymentRepo: gwRepo,
		IntentStore:        intents,
		DueStore:           dueStore,
		OutboxEvents:       outbox,
	}}

	disputeJSON := `{
		"type": "PAYMENT_DISPUTE_CREATED_WEBHOOK",
		"data": {
			"dispute": {
				"dispute_id": "DISP_555",
				"dispute_type": "CHARGEBACK",
				"dispute_status": "ACTION_REQUIRED",
				"order_id": "order_disp_123",
				"cf_payment_id": "999899",
				"dispute_amount": "8000.00",
				"reason_code": "UNAUTHORIZED_TRANSACTION",
				"reason_description": "Customer claims unauthorized payment",
				"respond_by": "2026-10-10T12:00:00Z"
			}
		}
	}`

	code := postSignedWebhook(t, h, disputeJSON)
	if code != http.StatusOK {
		t.Fatalf("expected 200 OK on dispute webhook to prevent retry storms, got %d", code)
	}
	if gwRepo.lastStatus != "dispute_action_required" {
		t.Fatalf("expected webhook status dispute_action_required, got %s", gwRepo.lastStatus)
	}
	if len(outbox.events) != 1 || outbox.events[0].EventType != string(domain.EvtPaymentDisputed) {
		t.Fatalf("expected 1 EvtPaymentDisputed outbox event, got %+v", outbox.events)
	}
	if outbox.events[0].PropertyID != propID {
		t.Fatalf("expected propertyID %s, got %s", propID, outbox.events[0].PropertyID)
	}
}


func postSignedWebhook(t *testing.T, h *Handlers, body string) int {
	t.Helper()
	r := gin.New()
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)
	ts := "100"
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sign("whsec", ts+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func sign(secret, msg string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type stubReportStore struct {
	reports map[string]*domain.PaymentReport
	hashes  map[string]bool
}

func newStubReportStore() *stubReportStore {
	return &stubReportStore{
		reports: make(map[string]*domain.PaymentReport),
		hashes:  make(map[string]bool),
	}
}

func (s *stubReportStore) Create(_ context.Context, p *domain.PaymentReport) error {
	p.ID = uuid.New()
	s.reports[p.UPITxnID] = p
	if p.ImageHash != nil {
		s.hashes[*p.ImageHash] = true
	}
	return nil
}

func (s *stubReportStore) GetByID(_ context.Context, id uuid.UUID) (*domain.PaymentReport, error) {
	for _, r := range s.reports {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (s *stubReportStore) GetByUPITxnID(_ context.Context, txnID string) (*domain.PaymentReport, error) {
	r := s.reports[txnID]
	if r == nil {
		return nil, pgx.ErrNoRows
	}
	return r, nil
}

func (s *stubReportStore) ListByProperty(_ context.Context, _ uuid.UUID, _ *domain.PaymentReportStatus) ([]domain.PaymentReport, error) {
	var list []domain.PaymentReport
	for _, r := range s.reports {
		list = append(list, *r)
	}
	return list, nil
}

func (s *stubReportStore) UpdateReview(_ context.Context, p *domain.PaymentReport) error {
	s.reports[p.UPITxnID] = p
	return nil
}

func (s *stubReportStore) HasImageWithHash(_ context.Context, _ uuid.UUID, hash string) (bool, error) {
	return s.hashes[hash], nil
}

type payTestDueStore struct {
	dues map[uuid.UUID]*domain.Due
}

func (s *payTestDueStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	d := s.dues[id]
	if d == nil {
		return nil, pgx.ErrNoRows
	}
	return d, nil
}

func (s *payTestDueStore) List(_ context.Context, _ postgres.DueListFilter) ([]domain.Due, error) {
	return nil, nil
}

func (s *payTestDueStore) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]domain.Due, error) {
	var out []domain.Due
	for _, d := range s.dues {
		if d.TenantID == tenantID {
			out = append(out, *d)
		}
	}
	return out, nil
}

type payTestPropertyStore struct {
	prop *domain.Property
}

func (s *payTestPropertyStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Property, error) {
	return s.prop, nil
}
func (s *payTestPropertyStore) List(_ context.Context) ([]domain.Property, error) { return nil, nil }
func (s *payTestPropertyStore) GetByOwnerPhone(_ context.Context, phone string) (*domain.Property, error) {
	return nil, nil
}
func (s *payTestPropertyStore) GetByInviteCode(_ context.Context, code string) (*domain.Property, error) {
	return nil, nil
}


func TestTenantSubmitReport_DuplicateImageFlagged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()
	dueID := uuid.New()
	userID := uuid.New()

	due := &domain.Due{
		ID:         dueID,
		TenantID:   tenantID,
		PropertyID: propID,
		Amount:     100000,
		Status:     domain.DueStatusPending,
	}

	dueStore := &payTestDueStore{dues: map[uuid.UUID]*domain.Due{dueID: due}}
	reportStore := newStubReportStore()

	h := &Handlers{Deps: Deps{
		DueStore:    dueStore,
		ReportStore: reportStore,
	}}

	r := gin.New()
	r.POST("/api/tenant/dues/:id/reports", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, &domain.Tenant{ID: tenantID, PropertyID: propID})
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: userID})
		c.Params = gin.Params{{Key: "id", Value: dueID.String()}}
		h.TenantSubmitReport(c)
	})

	// Valid sample JPEG bytes (with magic bytes FF D8 FF)
	sampleImg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01}

	// 1. First submission with image
	var b1 bytes.Buffer
	w1 := multipart.NewWriter(&b1)
	_ = w1.WriteField("upi_txn_id", "UTR1001")
	_ = w1.WriteField("amount", "1000")
	fw1, _ := w1.CreateFormFile("image", "receipt1.jpg")
	_, _ = fw1.Write(sampleImg)
	_ = w1.Close()

	req1 := httptest.NewRequest(http.MethodPost, "/api/tenant/dues/"+dueID.String()+"/reports", &b1)
	req1.Header.Set("Content-Type", w1.FormDataContentType())
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on first submit, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var tenantResp1 map[string]any
	_ = json.Unmarshal(rec1.Body.Bytes(), &tenantResp1)
	if _, ok := tenantResp1["is_duplicate"]; ok {
		t.Fatal("security violation: leaked is_duplicate signal to submitting tenant")
	}
	if _, ok := tenantResp1["image_hash"]; ok {
		t.Fatal("security violation: leaked image_hash to submitting tenant")
	}
	stored1 := reportStore.reports["UTR1001"]
	if stored1 == nil || stored1.ImageHash == nil {
		t.Fatal("expected image_hash to be stored in backend")
	}
	if stored1.IsDuplicate {
		t.Fatal("expected is_duplicate to be false for initial submission")
	}

	// 2. Second submission with the exact same image bytes, but different UTR
	var b2 bytes.Buffer
	w2 := multipart.NewWriter(&b2)
	_ = w2.WriteField("upi_txn_id", "UTR1002")
	_ = w2.WriteField("amount", "1000")
	fw2, _ := w2.CreateFormFile("image", "receipt2.jpg")
	_, _ = fw2.Write(sampleImg)
	_ = w2.Close()

	req2 := httptest.NewRequest(http.MethodPost, "/api/tenant/dues/"+dueID.String()+"/reports", &b2)
	req2.Header.Set("Content-Type", w2.FormDataContentType())
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on duplicate submit (flag, don't block), got %d: %s", rec2.Code, rec2.Body.String())
	}
	var tenantResp2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &tenantResp2)
	if _, ok := tenantResp2["is_duplicate"]; ok {
		t.Fatal("security violation: leaked is_duplicate signal to submitting tenant on duplicate upload")
	}
	stored2 := reportStore.reports["UTR1002"]
	if stored2 == nil || stored2.ImageHash == nil {
		t.Fatal("expected image_hash to be stored in backend")
	}
	if !stored2.IsDuplicate {
		t.Fatal("expected is_duplicate to be true in backend for duplicate image submission")
	}

	// 3. Verify owner endpoint DOES expose is_duplicate
	ownerRouter := gin.New()
	ownerRouter.GET("/api/owner/payment-reports", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: userID, PropertyID: &propID})
		h.ListPaymentReports(c)
	})
	ownerReq := httptest.NewRequest(http.MethodGet, "/api/owner/payment-reports", nil)
	ownerRec := httptest.NewRecorder()
	ownerRouter.ServeHTTP(ownerRec, ownerReq)
	if ownerRec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from owner list, got %d", ownerRec.Code)
	}
	var ownerListResp struct {
		Reports []domain.PaymentReport `json:"payment_reports"`
	}
	_ = json.Unmarshal(ownerRec.Body.Bytes(), &ownerListResp)
	var foundDupFlag bool
	for _, rep := range ownerListResp.Reports {
		if rep.UPITxnID == "UTR1002" && rep.IsDuplicate {
			foundDupFlag = true
		}
	}
	if !foundDupFlag {
		t.Fatal("expected owner listing to surface is_duplicate=true for flagged receipt")
	}

	// 4. Third submission with NO image (UTR only)
	var b3 bytes.Buffer
	w3 := multipart.NewWriter(&b3)
	_ = w3.WriteField("upi_txn_id", "UTR1003")
	_ = w3.WriteField("amount", "1000")
	_ = w3.Close()

	req3 := httptest.NewRequest(http.MethodPost, "/api/tenant/dues/"+dueID.String()+"/reports", &b3)
	req3.Header.Set("Content-Type", w3.FormDataContentType())
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on no-image submit, got %d: %s", rec3.Code, rec3.Body.String())
	}
	stored3 := reportStore.reports["UTR1003"]
	if stored3 == nil {
		t.Fatal("expected report to be stored")
	}
	if stored3.ImageHash != nil {
		t.Fatal("expected nil image_hash when no image uploaded")
	}
	if stored3.IsDuplicate {
		t.Fatal("expected is_duplicate to be false when no image uploaded")
	}
}

func TestTenantDuePayBatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()
	now := time.Now().UTC()

	d1 := &domain.Due{
		ID:             uuid.New(),
		DueCode:        "DUE001",
		TenantID:       tenantID,
		PropertyID:     propID,
		Amount:         550000,
		OriginalAmount: 550000,
		Status:         domain.DueStatusPending,
		DueDate:        now.Add(-30 * 24 * time.Hour),
		Kind:           domain.DueKindRent,
	}
	d2 := &domain.Due{
		ID:             uuid.New(),
		DueCode:        "DUE002",
		TenantID:       tenantID,
		PropertyID:     propID,
		Amount:         550000,
		OriginalAmount: 550000,
		Status:         domain.DueStatusPending,
		DueDate:        now,
		Kind:           domain.DueKindRent,
	}

	prop := &domain.Property{
		ID:          propID,
		PaymentMode: domain.PaymentModeManual,
		UPIVPA:      "test@upi",
		OwnerName:   "Owner Test",
	}

	dueStore := &payTestDueStore{dues: map[uuid.UUID]*domain.Due{d1.ID: d1, d2.ID: d2}}
	propStore := &payTestPropertyStore{prop: prop}
	col := collector.New(&stubIntentStore{}, nil)

	h := &Handlers{Deps: Deps{
		DueStore:      dueStore,
		PropertyStore: propStore,
		Collector:     col,
	}}

	r := gin.New()
	r.GET("/api/tenant/dues/options", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, &domain.Tenant{ID: tenantID, PropertyID: propID, Status: domain.TenantStatusActive})
		h.TenantDuesOptions(c)
	})
	r.POST("/api/tenant/dues/pay-batch", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, &domain.Tenant{ID: tenantID, PropertyID: propID, Status: domain.TenantStatusActive})
		h.TenantDuePayBatch(c)
	})

	// 1. Test GET /options
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenant/dues/options", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from options, got %d: %s", rec.Code, rec.Body.String())
	}
	var optResp struct {
		TotalOutstandingPaise int                    `json:"total_outstanding_paise"`
		Options               []domain.PaymentOption `json:"options"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &optResp); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if optResp.TotalOutstandingPaise != 1100000 {
		t.Fatalf("expected 1100000, got %d", optResp.TotalOutstandingPaise)
	}
	if len(optResp.Options) != 2 {
		t.Fatalf("expected 2 options, got %d", len(optResp.Options))
	}

	// 2. Test POST /pay-batch with option_type = "all"
	bodyAll, _ := json.Marshal(PayBatchRequest{OptionType: domain.OptionAll})
	recAll := httptest.NewRecorder()
	reqAll := httptest.NewRequest(http.MethodPost, "/api/tenant/dues/pay-batch", bytes.NewReader(bodyAll))
	reqAll.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(recAll, reqAll)

	if recAll.Code != http.StatusOK {
		t.Fatalf("expected 200 from pay-batch all, got %d: %s", recAll.Code, recAll.Body.String())
	}
	var intentAll domain.PayIntent
	_ = json.Unmarshal(recAll.Body.Bytes(), &intentAll)
	if intentAll.AmountPaise != 1100000 || intentAll.DueCount != 2 || !intentAll.Payable {
		t.Fatalf("unexpected intentAll: %+v", intentAll)
	}

	// 3. Test POST /pay-batch with option_type = "oldest_1"
	body1, _ := json.Marshal(PayBatchRequest{OptionType: domain.OptionOldest1})
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/api/tenant/dues/pay-batch", bytes.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("expected 200 from pay-batch oldest_1, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var intent1 domain.PayIntent
	_ = json.Unmarshal(rec1.Body.Bytes(), &intent1)
	if intent1.AmountPaise != 550000 || intent1.DueCount != 1 || !intent1.Payable {
		t.Fatalf("unexpected intent1: %+v", intent1)
	}

	// 4. Test invalid option_type
	bodyInv, _ := json.Marshal(PayBatchRequest{OptionType: "invalid_option"})
	recInv := httptest.NewRecorder()
	reqInv := httptest.NewRequest(http.MethodPost, "/api/tenant/dues/pay-batch", bytes.NewReader(bodyInv))
	reqInv.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(recInv, reqInv)
	if recInv.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid option, got %d", recInv.Code)
	}
}


