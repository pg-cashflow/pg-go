package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
)

// ---- Fake KYCService --------------------------------------------------------

type fakeKYCSvc struct {
	consentFn    func(ctx context.Context, tenantID uuid.UUID, purpose, version, text, ip, ua, actor string) (*domain.KYCConsent, error)
	initiateFn   func(ctx context.Context, tenantID uuid.UUID, actor string) (string, error)
	completionFn func(ctx context.Context, vendorRefID, failedReason, actor string) error
	qrFn         func(ctx context.Context, tenantID uuid.UUID, rawQR, actor string) (*domain.KYCVerification, error)
	revokeFn     func(ctx context.Context, tenantID uuid.UUID, actor string) error
	statusFn     func(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error)
	ownerViewFn  func(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, []domain.KYCAuditLog, error)
	vendorRefFn  func(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error)
	clearDupFn   func(ctx context.Context, verificationID uuid.UUID, actor, reason string) error
	uploadFn     func(ctx context.Context, tenantID uuid.UUID, fileReader io.Reader, filename, actor string) (*domain.KYCVerification, error)
	returnFn     func(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error)
	photoFn      func(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
	completeAadhaarOTPFn func(ctx context.Context, tenantID uuid.UUID, refID, otp, actor string) (*domain.KYCVerification, error)
}

func (f *fakeKYCSvc) RecordConsent(ctx context.Context, tenantID uuid.UUID, purpose, version, text, ip, ua, actor string) (*domain.KYCConsent, error) {
	if f.consentFn != nil {
		return f.consentFn(ctx, tenantID, purpose, version, text, ip, ua, actor)
	}
	return &domain.KYCConsent{ID: uuid.New(), ConsentVersion: version}, nil
}
func (f *fakeKYCSvc) InitiateDigiLocker(ctx context.Context, tenantID uuid.UUID, actor string) (string, error) {
	if f.initiateFn != nil {
		return f.initiateFn(ctx, tenantID, actor)
	}
	return "https://digilocker.gov.in/verify?session=test", nil
}
func (f *fakeKYCSvc) ProcessDigiLockerCompletion(ctx context.Context, vendorRefID, failedReason, actor string) error {
	if f.completionFn != nil {
		return f.completionFn(ctx, vendorRefID, failedReason, actor)
	}
	return nil
}
func (f *fakeKYCSvc) VerifySecureQR(ctx context.Context, tenantID uuid.UUID, rawQR, actor string) (*domain.KYCVerification, error) {
	if f.qrFn != nil {
		return f.qrFn(ctx, tenantID, rawQR, actor)
	}
	now := time.Now()
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified, VerifiedAt: &now}, nil
}
func (f *fakeKYCSvc) VerifyAadhaarDocument(ctx context.Context, tenantID uuid.UUID, fileReader io.Reader, filename, actor string) (*domain.KYCVerification, error) {
	if f.uploadFn != nil {
		return f.uploadFn(ctx, tenantID, fileReader, filename, actor)
	}
	now := time.Now()
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified, VerifiedAt: &now}, nil
}
func (f *fakeKYCSvc) GetDigiLockerReturnStatus(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error) {
	if f.returnFn != nil {
		return f.returnFn(ctx, vendorRefID)
	}
	now := time.Now()
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified, VerifiedAt: &now}, nil
}
func (f *fakeKYCSvc) GetAttestedPhoto(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	if f.photoFn != nil {
		return f.photoFn(ctx, tenantID)
	}
	return []byte{0x89, 0x50, 0x4E, 0x47}, nil
}
func (f *fakeKYCSvc) RevokeConsent(ctx context.Context, tenantID uuid.UUID, actor string) error {
	if f.revokeFn != nil {
		return f.revokeFn(ctx, tenantID, actor)
	}
	return nil
}
func (f *fakeKYCSvc) GetStatus(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
	if f.statusFn != nil {
		return f.statusFn(ctx, tenantID)
	}
	return nil, nil, nil
}
func (f *fakeKYCSvc) GetOwnerView(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, []domain.KYCAuditLog, error) {
	if f.ownerViewFn != nil {
		return f.ownerViewFn(ctx, tenantID)
	}
	return nil, []domain.KYCAuditLog{}, nil
}
func (f *fakeKYCSvc) GetVerificationByVendorRefID(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error) {
	if f.vendorRefFn != nil {
		return f.vendorRefFn(ctx, vendorRefID)
	}
	return nil, domain.ErrVerificationNotFound
}
func (f *fakeKYCSvc) ClearDuplicateFlag(ctx context.Context, verificationID uuid.UUID, actor, reason string) error {
	if f.clearDupFn != nil {
		return f.clearDupFn(ctx, verificationID, actor, reason)
	}
	return nil
}
func (f *fakeKYCSvc) InitiateAadhaarOTP(ctx context.Context, tenantID uuid.UUID, aadhaarNumber, actor string) (string, error) {
	return "test_aadhaar_ref_123", nil
}
func (f *fakeKYCSvc) CompleteAadhaarOTP(ctx context.Context, tenantID uuid.UUID, refID, otp, actor string) (*domain.KYCVerification, error) {
	if f.completeAadhaarOTPFn != nil {
		return f.completeAadhaarOTPFn(ctx, tenantID, refID, otp, actor)
	}
	now := time.Now()
	return &domain.KYCVerification{
		ID:         uuid.New(),
		TenantID:   tenantID,
		Status:     domain.KYCStatusVerified,
		Method:     domain.KYCMethodAadhaarOTP,
		VerifiedAt: &now,
	}, nil
}
func (f *fakeKYCSvc) ListPendingReviews(ctx context.Context, propertyID uuid.UUID) ([]domain.KYCVerification, error) {
	return nil, nil
}
func (f *fakeKYCSvc) ApproveManualReview(ctx context.Context, verificationID, reviewerID uuid.UUID, actor string) (*domain.KYCVerification, error) {
	now := time.Now()
	return &domain.KYCVerification{
		ID:         verificationID,
		Status:     domain.KYCStatusVerified,
		VerifiedAt: &now,
	}, nil
}
func (f *fakeKYCSvc) RejectManualReview(ctx context.Context, verificationID, reviewerID uuid.UUID, reason, actor string) (*domain.KYCVerification, error) {
	return &domain.KYCVerification{
		ID:              verificationID,
		Status:          domain.KYCStatusRejected,
		RejectionReason: &reason,
	}, nil
}

// ---- Test router helpers ----------------------------------------------------

// kycTestTenantRouter builds a minimal Gin engine with:
//   - A pre-middleware that injects a fake Tenant into context (bypasses real JWT auth)
//   - The KYC tenant routes wired to the given KYCService
//   - The KYC public webhook route
func kycTestTenantRouter(svc KYCService, cashfreeSecret string) *gin.Engine {
	return kycTestTenantRouterWithTenant(svc, cashfreeSecret, nil)
}

func kycTestTenantRouterWithTenant(svc KYCService, cashfreeSecret string, tenant *domain.Tenant) *gin.Engine {
	gin.SetMode(gin.TestMode)
	if tenant == nil {
		tenant = &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New()}
	}

	r := gin.New()
	// Inject fake tenant for all tenant routes without requiring a real JWT.
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, tenant)
		c.Next()
	})

	h := &Handlers{Deps: Deps{
		KYCSvc:         svc,
		CashfreeSecret: cashfreeSecret,
	}}

	tenant_g := r.Group("/api/tenant/kyc")
	{
		tenant_g.POST("/consent", h.TenantKYCConsent)
		tenant_g.POST("/upload", h.TenantKYCUpload)
		tenant_g.POST("/aadhaar/otp/request", h.TenantKYCAadhaarOTPRequest)
		tenant_g.POST("/aadhaar/otp/verify", h.TenantKYCAadhaarOTPVerify)
		tenant_g.POST("/revoke", h.TenantKYCRevoke)
		tenant_g.GET("/status", h.TenantKYCStatus)
	}

	pub := r.Group("/api/public")
	{
		pub.POST("/cashfree/kyc/webhook", h.CashfreeKYCWebhook)
	}

	return r
}

// signHMAC builds a valid Cashfree webhook HMAC signature for the given secret.
func signHMAC(secret, timestamp, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// ---- TenantKYCConsent -------------------------------------------------------

func TestTenantKYCConsent_OK(t *testing.T) {
	r := kycTestTenantRouter(&fakeKYCSvc{}, "")

	body, _ := json.Marshal(map[string]string{
		"purpose":         "aadhaar_kyc",
		"consent_version": "v1",
		"consent_text":    "I consent to Aadhaar verification",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/consent", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTenantKYCConsent_MissingFields(t *testing.T) {
	r := kycTestTenantRouter(&fakeKYCSvc{}, "")

	body, _ := json.Marshal(map[string]string{"purpose": "test"}) // missing consent_version, consent_text
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/consent", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing required fields, got %d", w.Code)
	}
}

// ---- TenantKYCRevoke --------------------------------------------------------

func TestTenantKYCRevoke_OK(t *testing.T) {
	r := kycTestTenantRouter(&fakeKYCSvc{}, "")

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/revoke", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ---- TenantKYCStatus — field visibility -------------------------------------

func TestTenantKYCStatus_HidesInternalFields(t *testing.T) {
	uid := "XXXX-1234"
	hash := "sensitive-identity-hash"
	now := time.Now()

	svc := &fakeKYCSvc{
		statusFn: func(_ context.Context, _ uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
			return &domain.KYCVerification{
				ID:                uuid.New(),
				Status:            domain.KYCStatusVerified,
				Method:            domain.KYCMethodDigiLocker,
				MaskedUID:         &uid,
				IdentityHash:      &hash,
				DuplicateDetected: true, // flag must NOT appear in tenant view
				VendorReferenceID: "cf-secret-ref",
				VerifiedAt:        &now,
				CreatedAt:         now,
				UpdatedAt:         now,
			}, &domain.KYCConsent{ConsentVersion: "v1", ConsentGivenAt: now}, nil
		},
	}
	r := kycTestTenantRouter(svc, "")

	req := httptest.NewRequest(http.MethodGet, "/api/tenant/kyc/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	verif, _ := resp["verification"].(map[string]any)

	if _, exists := verif["identity_hash"]; exists {
		t.Error("SECURITY: identity_hash must NOT be exposed to tenants")
	}
	if _, exists := verif["duplicate_detected"]; exists {
		t.Error("SECURITY: duplicate_detected must NOT be exposed to tenants (fraud signal)")
	}
	if _, exists := verif["vendor_reference_id"]; exists {
		t.Error("vendor_reference_id must NOT be exposed to tenants")
	}
	// These SHOULD be present.
	if verif["status"] == nil {
		t.Error("status should be present in tenant-facing view")
	}
	if verif["masked_uid"] == nil {
		t.Error("masked_uid should be present in tenant-facing view")
	}
}

func TestTenantKYCStatus_NoRecords(t *testing.T) {
	r := kycTestTenantRouter(&fakeKYCSvc{}, "")

	req := httptest.NewRequest(http.MethodGet, "/api/tenant/kyc/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["has_consent"] != false {
		t.Errorf("expected has_consent=false when no records, got %v", resp["has_consent"])
	}
	if resp["has_verification"] != false {
		t.Errorf("expected has_verification=false when no records, got %v", resp["has_verification"])
	}
}

// ---- CashfreeKYCWebhook — HMAC verification ---------------------------------

func TestCashfreeKYCWebhook_UnconfiguredSecret_Returns503(t *testing.T) {
	svc := &fakeKYCSvc{}
	// Router initialized with empty webhook secret
	r := kycTestTenantRouter(svc, "")

	payload := `{"type":"VERIFICATION_COMPLETED","data":{"verification_id":"ver-001"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when CashfreeSecret is empty, got %d", w.Code)
	}
}

func TestCashfreeKYCWebhook_BadHMAC_Returns200NotError(t *testing.T) {
	// Bad signatures must return 200, not 4xx, to prevent Cashfree retry storms.
	completionCalled := false
	svc := &fakeKYCSvc{
		completionFn: func(_ context.Context, _, _, _ string) error {
			completionCalled = true
			return nil
		},
	}
	r := kycTestTenantRouter(svc, "real-secret")

	payload := `{"type":"VERIFICATION_COMPLETED","data":{"verification_id":"ver-001"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("x-webhook-signature", "wrong-signature")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("bad HMAC should return 200 (not retry-triggering 4xx), got %d", w.Code)
	}
	if completionCalled {
		t.Error("completion must NOT be called when HMAC fails")
	}
}

func TestCashfreeKYCWebhook_CompletedEvent(t *testing.T) {
	completionCalled := false
	capturedVendorRef := ""
	capturedFailedReason := "initial" // should end up empty for COMPLETED

	svc := &fakeKYCSvc{
		completionFn: func(_ context.Context, vendorRefID, failedReason, actor string) error {
			completionCalled = true
			capturedVendorRef = vendorRefID
			capturedFailedReason = failedReason
			return nil
		},
	}
	secret := "test-webhook-secret"
	r := kycTestTenantRouter(svc, secret)

	payload := `{"type":"VERIFICATION_COMPLETED","data":{"verification_id":"ver-completed","status":"COMPLETED"}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signHMAC(secret, ts, payload)

	req := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !completionCalled {
		t.Error("expected ProcessDigiLockerCompletion to be called")
	}
	if capturedVendorRef != "ver-completed" {
		t.Errorf("expected vendor_ref=ver-completed, got %q", capturedVendorRef)
	}
	if capturedFailedReason != "" {
		t.Errorf("COMPLETED event should have empty failedReason, got %q", capturedFailedReason)
	}
}

func TestCashfreeKYCWebhook_FailedEvent(t *testing.T) {
	completionCalled := false
	capturedReason := ""

	svc := &fakeKYCSvc{
		completionFn: func(_ context.Context, _, failedReason, _ string) error {
			completionCalled = true
			capturedReason = failedReason
			return nil
		},
	}
	secret := "test-webhook-secret"
	r := kycTestTenantRouter(svc, secret)

	payload := `{"type":"VERIFICATION_FAILED","data":{"verification_id":"ver-fail","status":"FAILED","failed_reason":"user declined"}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signHMAC(secret, ts, payload)

	req := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !completionCalled {
		t.Error("expected ProcessDigiLockerCompletion to be called for FAILED event")
	}
	if capturedReason == "" {
		t.Error("VERIFICATION_FAILED must pass a non-empty failedReason to the service")
	}
}

func TestCashfreeKYCWebhook_UnknownEventType_IsNoOp(t *testing.T) {
	completionCalled := false
	svc := &fakeKYCSvc{
		completionFn: func(_ context.Context, _, _, _ string) error {
			completionCalled = true
			return nil
		},
	}
	secret := "test-webhook-secret"
	r := kycTestTenantRouter(svc, secret)

	payload := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signHMAC(secret, ts, payload)

	req := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if completionCalled {
		t.Error("non-KYC event types must not trigger ProcessDigiLockerCompletion")
	}
}

// ---- KYCSvc nil safety — 503 on every KYC route ----------------------------

func TestKYCHandlers_NilService_Returns503(t *testing.T) {
	// Build a router with KYCSvc=nil to confirm nil safety.
	gin.SetMode(gin.TestMode)
	tenant := &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New()}
	rr := gin.New()
	rr.Use(func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, tenant)
		c.Next()
	})
	h := &Handlers{Deps: Deps{}} // KYCSvc intentionally nil
	grp := rr.Group("/api/tenant/kyc")
	{
		grp.POST("/consent", h.TenantKYCConsent)
		grp.POST("/upload", h.TenantKYCUpload)
		grp.POST("/revoke", h.TenantKYCRevoke)
		grp.GET("/status", h.TenantKYCStatus)
	}

	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/tenant/kyc/consent"},
		{http.MethodPost, "/api/tenant/kyc/upload"},
		{http.MethodPost, "/api/tenant/kyc/revoke"},
		{http.MethodGet, "/api/tenant/kyc/status"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		rr.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: expected 503 when KYCSvc is nil, got %d", tc.method, tc.path, w.Code)
		}
	}

	// Also verify that the unauthenticated public webhook cleanly returns 503 rather than panicking.
	rr.POST("/api/public/cashfree/kyc/webhook", h.CashfreeKYCWebhook)
	req := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rr.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("webhook: expected 503 when KYCSvc is nil, got %d", w.Code)
	}
}

func TestCashfreeKYCWebhook_TransientError_Returns500(t *testing.T) {
	svc := &fakeKYCSvc{
		completionFn: func(_ context.Context, _, _, _ string) error {
			return fmt.Errorf("database connection lost: transient blip")
		},
	}
	secret := "test-webhook-secret"
	r := kycTestTenantRouter(svc, secret)

	payload := `{"type":"VERIFICATION_COMPLETED","data":{"verification_id":"ver-transient","status":"COMPLETED","entity":"AADHAAR"}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signHMAC(secret, ts, payload)

	req := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on transient DB error, got %d", w.Code)
	}
}

func TestCashfreeKYCWebhook_UnknownVendorRef_Returns200(t *testing.T) {
	svc := &fakeKYCSvc{
		completionFn: func(_ context.Context, _, _, _ string) error {
			return fmt.Errorf("kyc service: unknown vendor_ref_id: %w", domain.ErrVerificationNotFound)
		},
	}
	secret := "test-webhook-secret"
	r := kycTestTenantRouter(svc, secret)

	payload := `{"type":"VERIFICATION_COMPLETED","data":{"verification_id":"ver-unknown","status":"COMPLETED","entity":"AADHAAR"}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signHMAC(secret, ts, payload)

	req := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for unknown vendor ref (no retry wanted), got %d", w.Code)
	}
}

func TestCashfreeKYCWebhook_DoubleDelivery(t *testing.T) {
	callCount := 0
	svc := &fakeKYCSvc{
		completionFn: func(_ context.Context, vendorRefID, failedReason, _ string) error {
			callCount++
			// First call transitions to terminal state, second call is an idempotent no-op.
			// Both return nil to signal success to the handler.
			return nil
		},
	}
	secret := "test-webhook-secret"
	r := kycTestTenantRouter(svc, secret)

	payload := `{"type":"VERIFICATION_COMPLETED","data":{"verification_id":"ver-double-delivery","status":"COMPLETED","entity":"AADHAAR"}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := signHMAC(secret, ts, payload)

	// First delivery: normal processing.
	req1 := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(payload))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("x-webhook-timestamp", ts)
	req1.Header.Set("x-webhook-signature", sig)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	if w1.Code != http.StatusOK {
		t.Errorf("first delivery: expected 200, got %d", w1.Code)
	}

	// Second delivery: identical raw bytes, identical signature.
	req2 := httptest.NewRequest(http.MethodPost, "/api/public/cashfree/kyc/webhook", bytes.NewBufferString(payload))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-webhook-timestamp", ts)
	req2.Header.Set("x-webhook-signature", sig)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("second delivery: expected 200, got %d", w2.Code)
	}

	if callCount != 2 {
		t.Errorf("expected ProcessDigiLockerCompletion to be invoked twice (service handles idempotency), got %d calls", callCount)
	}
}

// ---- actor format verification -----------------------------------------------

func TestOwnerKYCVerificationView_Fields(t *testing.T) {
	qrStatus := domain.QRStatusSecure
	v := &domain.KYCVerification{
		ID:                uuid.New(),
		Status:            domain.KYCStatusVerified,
		Method:            domain.KYCMethodOCR,
		QRStatus:          &qrStatus,
		NameMismatch:      true,
		PhotoStored:       true,
		DuplicateDetected: true,
		IsDedupable:       true,
	}
	view := ownerKYCVerificationView(v)
	if view["trust_tier"] != "document_secure_qr" {
		t.Errorf("expected trust_tier document_secure_qr, got %v", view["trust_tier"])
	}
	if view["qr_status"] != &qrStatus {
		t.Errorf("expected qr_status %v, got %v", qrStatus, view["qr_status"])
	}
	if view["photo_stored"] != true {
		t.Errorf("expected photo_stored true, got %v", view["photo_stored"])
	}
	if view["name_mismatch"] != true {
		t.Errorf("expected name_mismatch true, got %v", view["name_mismatch"])
	}
}

func TestTenantKYCUpload_InFlight_Returns409(t *testing.T) {
	svc := &fakeKYCSvc{
		uploadFn: func(_ context.Context, _ uuid.UUID, _ io.Reader, _, _ string) (*domain.KYCVerification, error) {
			return nil, domain.ErrVerificationInProgress
		},
	}
	r := kycTestTenantRouter(svc, "")

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "aadhaar.jpg")
	_, _ = part.Write([]byte("fake image bytes"))
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d", w.Code)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"] != "kyc.in_flight" {
		t.Errorf("expected code kyc.in_flight, got %v", resp["code"])
	}
}

func TestTenantKYCUpload_RateLimit_BurstEnforced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenant := &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New()}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, tenant)
		c.Next()
	})

	now := time.Now()
	svc := &fakeKYCSvc{
		uploadFn: func(_ context.Context, _ uuid.UUID, _ io.Reader, _, _ string) (*domain.KYCVerification, error) {
			return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified, VerifiedAt: &now}, nil
		},
	}
	h := &Handlers{Deps: Deps{KYCSvc: svc}}

	// Wire rate-limited endpoint matching router.go: 5/min, burst 2
	r.POST("/api/tenant/kyc/upload", h.TenantIDRateLimit(5.0/60, 2), h.TenantKYCUpload)

	makeUploadReq := func() *http.Request {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		part, _ := writer.CreateFormFile("file", "aadhaar.jpg")
		_, _ = part.Write([]byte("fake image bytes"))
		_ = writer.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/upload", body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		return req
	}

	// Requests 1 & 2 should succeed within burst
	for i := 1; i <= 2; i++ {
		req := makeUploadReq()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d expected 200, got %d", i, w.Code)
		}
	}

	// Request 3 should immediately exceed burst and return 429 Too Many Requests
	req := makeUploadReq()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("request 3 expected 429 Too Many Requests, got %d", w.Code)
	}
}

func kycTestOwnerRouter(svc KYCService, propertyID, ownerUserID uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerUserID,
			Role:       domain.RoleOwner,
			PropertyID: &propertyID,
		})
		c.Next()
	})
	h := &Handlers{Deps: Deps{
		KYCSvc: svc,
	}}
	owner := r.Group("/api/owner/kyc")
	{
		owner.GET("/queue", h.OwnerKYCReviewQueue)
		owner.POST("/:id/approve", h.OwnerKYCApprove)
		owner.POST("/:id/reject", h.OwnerKYCReject)
	}
	return r
}

func TestTenantKYCAadhaarOTPRequest_OK(t *testing.T) {
	svc := &fakeKYCSvc{}
	r := kycTestTenantRouter(svc, "")

	body := `{"aadhaar_number":"548550008000"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/aadhaar/otp/request", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if resp["ref_id"] != "test_aadhaar_ref_123" {
		t.Errorf("expected ref_id 'test_aadhaar_ref_123', got %v", resp["ref_id"])
	}
}

func TestTenantKYCAadhaarOTPVerify_AutoVerified(t *testing.T) {
	svc := &fakeKYCSvc{}
	r := kycTestTenantRouter(svc, "")

	body := `{"ref_id":"test_aadhaar_ref_123","otp":"123456"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/aadhaar/otp/verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	// Aadhaar OTP is government-authenticated and auto-verified (instant verified status)
	if resp["status"] != string(domain.KYCStatusVerified) {
		t.Errorf("expected status 'verified', got %v", resp["status"])
	}
	if resp["method"] != string(domain.KYCMethodAadhaarOTP) {
		t.Errorf("expected method 'aadhaar_otp', got %v", resp["method"])
	}
}

func TestTenantKYCAadhaarOTPVerify_FailureClassification(t *testing.T) {
	t.Run("Terminal Failure (Mobile Not Linked)", func(t *testing.T) {
		svc := &fakeKYCSvc{
			completeAadhaarOTPFn: func(_ context.Context, _ uuid.UUID, _, _, _ string) (*domain.KYCVerification, error) {
				return nil, &kycsvc.AadhaarVerifyFailure{
					ReasonCode:        "MOBILE_NOT_LINKED",
					Class:             cashfree.FailureTerminal,
					Retryable:         false,
					CooldownSeconds:   0,
					AttemptsRemaining: 0,
				}
			},
		}
		r := kycTestTenantRouter(svc, "")

		body := `{"ref_id":"test_aadhaar_ref_123","otp":"123456"}`
		req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/aadhaar/otp/verify", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid json response: %v", err)
		}
		if resp["status"] != "failed" {
			t.Errorf("expected status 'failed', got %v", resp["status"])
		}
		if resp["reason_code"] != "MOBILE_NOT_LINKED" {
			t.Errorf("expected reason_code 'MOBILE_NOT_LINKED', got %v", resp["reason_code"])
		}
		if resp["retryable"] != false {
			t.Errorf("expected retryable false, got %v", resp["retryable"])
		}
		if resp["cooldown_seconds"] != float64(0) {
			t.Errorf("expected cooldown_seconds 0, got %v", resp["cooldown_seconds"])
		}
		if resp["attempts_remaining"] != float64(0) {
			t.Errorf("expected attempts_remaining 0, got %v", resp["attempts_remaining"])
		}
	})

	t.Run("Recoverable Failure (Wrong OTP with remaining attempts)", func(t *testing.T) {
		svc := &fakeKYCSvc{
			completeAadhaarOTPFn: func(_ context.Context, _ uuid.UUID, _, _, _ string) (*domain.KYCVerification, error) {
				return nil, &kycsvc.AadhaarVerifyFailure{
					ReasonCode:        "WRONG_OTP",
					Class:             cashfree.FailureRecoverable,
					Retryable:         true,
					CooldownSeconds:   50,
					AttemptsRemaining: 2,
				}
			},
		}
		r := kycTestTenantRouter(svc, "")

		body := `{"ref_id":"test_aadhaar_ref_123","otp":"999999"}`
		req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/aadhaar/otp/verify", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("invalid json response: %v", err)
		}
		if resp["status"] != "failed" {
			t.Errorf("expected status 'failed', got %v", resp["status"])
		}
		if resp["reason_code"] != "WRONG_OTP" {
			t.Errorf("expected reason_code 'WRONG_OTP', got %v", resp["reason_code"])
		}
		if resp["retryable"] != true {
			t.Errorf("expected retryable true, got %v", resp["retryable"])
		}
		if resp["cooldown_seconds"] != float64(50) {
			t.Errorf("expected cooldown_seconds 50, got %v", resp["cooldown_seconds"])
		}
		if resp["attempts_remaining"] != float64(2) {
			t.Errorf("expected attempts_remaining 2, got %v", resp["attempts_remaining"])
		}
	})
}

func TestOwnerKYCReviewQueue_OK(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	svc := &fakeKYCSvc{}
	r := kycTestOwnerRouter(svc, propertyID, ownerID)

	req := httptest.NewRequest(http.MethodGet, "/api/owner/kyc/queue", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if _, ok := resp["items"]; !ok {
		t.Errorf("expected 'items' in response, got %v", resp)
	}
}

func TestOwnerKYCApprove_OK(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	verifID := uuid.New()
	svc := &fakeKYCSvc{}
	r := kycTestOwnerRouter(svc, propertyID, ownerID)

	req := httptest.NewRequest(http.MethodPost, "/api/owner/kyc/"+verifID.String()+"/approve", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if resp["status"] != string(domain.KYCStatusVerified) {
		t.Errorf("expected status 'verified', got %v", resp["status"])
	}
}

func TestOwnerKYCReject_OK(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	verifID := uuid.New()
	svc := &fakeKYCSvc{}
	r := kycTestOwnerRouter(svc, propertyID, ownerID)

	body := `{"reason":"Document illegible, corners cropped"}`
	req := httptest.NewRequest(http.MethodPost, "/api/owner/kyc/"+verifID.String()+"/reject", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if resp["status"] != string(domain.KYCStatusRejected) {
		t.Errorf("expected status 'rejected', got %v", resp["status"])
	}
}

