package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type stubIntentStore struct {
	byOrder   map[string]*domain.PaymentIntent
	byCF      map[string]*domain.PaymentIntent
	paid      string
	lookupErr error
}

func (s *stubIntentStore) GetByOrderID(_ context.Context, orderID string) (*domain.PaymentIntent, error) {
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
	p := s.byCF[cfID]
	if p == nil {
		return nil, payment.ErrDueNotOpen
	}
	return p, nil
}
func (s *stubIntentStore) MarkPaid(_ context.Context, id uuid.UUID, cfPaymentID string) error {
	s.paid = cfPaymentID
	return nil
}

type stubPay struct {
	n         int
	txn       string
	settleErr error
}

func (s *stubPay) MatchPayment(context.Context, uuid.UUID, string, int, time.Time, string) (*domain.Payment, error) {
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
func (s *stubPay) GatewaySettle(_ context.Context, dueID uuid.UUID, amountPaise int, txnID string) (*domain.Payment, error) {
	s.n++
	s.txn = txnID
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
	if pay.n != 1 || pay.txn != "UTR1" || intents.paid != "9" {
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

func (s *payTestDueStore) ListByTenant(_ context.Context, _ uuid.UUID) ([]domain.Due, error) {
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

