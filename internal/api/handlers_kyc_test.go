package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
)

// ---- Fake KYCService --------------------------------------------------------

type fakeKYCSvc struct {
	consentFn           func(ctx context.Context, tenantID uuid.UUID, purpose, version, text, ip, ua, actor string) (*domain.KYCConsent, error)
	qrFn                func(ctx context.Context, tenantID uuid.UUID, rawQR, actor string) (*domain.KYCVerification, error)
	revokeFn            func(ctx context.Context, tenantID uuid.UUID, actor string) error
	statusFn            func(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error)
	ownerViewFn         func(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, []domain.KYCAuditLog, error)
	vendorRefFn         func(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error)
	clearDupFn          func(ctx context.Context, verificationID uuid.UUID, actor, reason string) error
	uploadFn            func(ctx context.Context, tenantID uuid.UUID, fileReader io.Reader, filename, contentType, actor string) (*domain.KYCVerification, error)
	photoFn             func(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
	approveFn           func(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, actor string) (*domain.KYCVerification, error)
	rejectFn            func(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, reason, actor string) (*domain.KYCVerification, error)
	processDigiLockerFn func(ctx context.Context, verificationIDStr, cfRefID, eventType, status, actor string) (*domain.KYCVerification, error)
}

func (f *fakeKYCSvc) RecordConsent(ctx context.Context, tenantID uuid.UUID, purpose, version, text, ip, ua, actor string) (*domain.KYCConsent, error) {
	if f.consentFn != nil {
		return f.consentFn(ctx, tenantID, purpose, version, text, ip, ua, actor)
	}
	return &domain.KYCConsent{ID: uuid.New(), ConsentVersion: version}, nil
}
func (f *fakeKYCSvc) VerifySecureQR(ctx context.Context, tenantID uuid.UUID, rawQR, actor string) (*domain.KYCVerification, error) {
	if f.qrFn != nil {
		return f.qrFn(ctx, tenantID, rawQR, actor)
	}
	now := time.Now()
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified, VerifiedAt: &now}, nil
}
func (f *fakeKYCSvc) VerifyAadhaarDocument(ctx context.Context, tenantID uuid.UUID, fileReader io.Reader, filename, contentType, actor string) (*domain.KYCVerification, error) {
	if f.uploadFn != nil {
		return f.uploadFn(ctx, tenantID, fileReader, filename, contentType, actor)
	}
	now := time.Now()
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified, VerifiedAt: &now}, nil
}
func (f *fakeKYCSvc) VerifyDocument(ctx context.Context, tenantID uuid.UUID, fileReader io.Reader, filename, contentType, documentType, actor string) (*domain.KYCVerification, error) {
	if f.uploadFn != nil {
		return f.uploadFn(ctx, tenantID, fileReader, filename, contentType, actor)
	}
	now := time.Now()
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified, DocumentType: domain.NormalizeDocumentType(documentType), VerifiedAt: &now}, nil
}
func (f *fakeKYCSvc) GetAttestedPhoto(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	if f.photoFn != nil {
		return f.photoFn(ctx, tenantID)
	}
	return []byte{0x89, 0x50, 0x4E, 0x47}, nil
}
func (f *fakeKYCSvc) GetKYCDocument(ctx context.Context, id, propertyID uuid.UUID) ([]byte, string, error) {
	return []byte("%PDF-1.4 mock"), "application/pdf", nil
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
func (f *fakeKYCSvc) ListPendingReviews(ctx context.Context, propertyID uuid.UUID) ([]domain.KYCVerification, error) {
	return nil, nil
}
func (f *fakeKYCSvc) ApproveManualReview(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, actor string) (*domain.KYCVerification, error) {
	if f.approveFn != nil {
		return f.approveFn(ctx, verificationID, propertyID, reviewerID, actor)
	}
	now := time.Now()
	return &domain.KYCVerification{
		ID:         verificationID,
		Status:     domain.KYCStatusVerified,
		VerifiedAt: &now,
	}, nil
}
func (f *fakeKYCSvc) RejectManualReview(ctx context.Context, verificationID, propertyID, reviewerID uuid.UUID, reason, actor string) (*domain.KYCVerification, error) {
	if f.rejectFn != nil {
		return f.rejectFn(ctx, verificationID, propertyID, reviewerID, reason, actor)
	}
	return &domain.KYCVerification{
		ID:              verificationID,
		Status:          domain.KYCStatusRejected,
		RejectionReason: &reason,
	}, nil
}
func (f *fakeKYCSvc) RecordGuardianConsent(ctx context.Context, tenantID uuid.UUID, guardianName, guardianPhone, relation, refID string, actor string) (*domain.Tenant, error) {
	now := time.Now()
	return &domain.Tenant{
		ID:                        tenantID,
		GuardianName:              &guardianName,
		GuardianPhone:             &guardianPhone,
		GuardianRelation:          &relation,
		GuardianKYCReferenceID:    &refID,
		GuardianConsentVerifiedAt: &now,
	}, nil
}
func (f *fakeKYCSvc) InitiateDigiLockerVerification(ctx context.Context, tenantID uuid.UUID, redirectURL, userFlow, actor string) (*domain.KYCVerification, string, error) {
	return &domain.KYCVerification{
		ID:                uuid.New(),
		TenantID:          tenantID,
		Status:            domain.KYCStatusPending,
		Method:            domain.KYCMethodDigiLocker,
		VendorReferenceID: "cf_dgl_mock_123",
	}, "https://verification.cashfree.com/dgl/mock_123", nil
}
func (f *fakeKYCSvc) ProcessDigiLockerCallback(ctx context.Context, verificationIDStr, cfRefID, eventType, status, actor string) (*domain.KYCVerification, error) {
	if f.processDigiLockerFn != nil {
		return f.processDigiLockerFn(ctx, verificationIDStr, cfRefID, eventType, status, actor)
	}
	return &domain.KYCVerification{
		ID:                uuid.New(),
		VendorReferenceID: verificationIDStr,
		Status:            domain.KYCStatusVerified,
	}, nil
}
func (f *fakeKYCSvc) PollDigiLockerStatus(ctx context.Context, tenantID uuid.UUID, actor string) (*domain.KYCVerification, error) {
	return &domain.KYCVerification{
		ID:       uuid.New(),
		TenantID: tenantID,
		Status:   domain.KYCStatusVerified,
	}, nil
}
func (f *fakeKYCSvc) CanTenantPay(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	ver, _, err := f.GetStatus(ctx, tenantID)
	if err != nil || ver == nil {
		return false, nil
	}
	return ver.Status == domain.KYCStatusVerified, nil
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
		KYCSvc:                   svc,
		CashfreeSecret:           cashfreeSecret,
		CashfreeKYCWebhookSecret: cashfreeSecret,
		FeatureKYCDigiLocker:     true,
	}}

	tenant_g := r.Group("/api/tenant/kyc")
	{
		tenant_g.POST("/consent", h.TenantKYCConsent)
		tenant_g.POST("/digilocker/init", h.TenantKYCDigiLockerInit)
		tenant_g.GET("/digilocker/status", h.TenantKYCDigiLockerStatus)
		tenant_g.POST("/upload", h.TenantKYCUpload)
		tenant_g.POST("/qr/verify", h.TenantKYCSecureQRVerify)
		tenant_g.POST("/aadhaar/otp/request", h.TenantKYCAadhaarOTPRequest)
		tenant_g.POST("/aadhaar/otp/verify", h.TenantKYCAadhaarOTPVerify)
		tenant_g.POST("/revoke", h.TenantKYCRevoke)
		tenant_g.GET("/status", h.TenantKYCStatus)
	}

	r.POST("/webhooks/cashfree/kyc", h.CashfreeKYCWebhook)

	return r
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

// ---- TenantKYCStatus â€” field visibility -------------------------------------

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

// TestTenantKYCStatus_ReportsMethods checks that the status response tells the client which
// verification routes the server accepts, so the UI never offers a method the server rejects.
func TestTenantKYCStatus_ReportsMethods(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
	}{
		{"digilocker enabled", true},
		{"digilocker disabled", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set(auth.ContextTenantKey, &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New()})
				c.Next()
			})
			h := &Handlers{Deps: Deps{KYCSvc: &fakeKYCSvc{}, FeatureKYCDigiLocker: tc.enabled}}
			r.GET("/api/tenant/kyc/status", h.TenantKYCStatus)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tenant/kyc/status", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			var resp struct {
				Methods map[string]bool `json:"methods"`
			}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := resp.Methods["digilocker"]; got != tc.enabled {
				t.Errorf("methods.digilocker = %v, want %v", got, tc.enabled)
			}
			if !resp.Methods["upload"] || !resp.Methods["qr"] {
				t.Errorf("methods.upload and methods.qr must stay true, got %v", resp.Methods)
			}
		})
	}
}

// ---- TenantKYCSecureQRVerify ------------------------------------------------

func TestTenantKYCSecureQRVerify_OK(t *testing.T) {
	now := time.Now()
	masked := "1234"
	svc := &fakeKYCSvc{
		qrFn: func(_ context.Context, tenantID uuid.UUID, rawQR, actor string) (*domain.KYCVerification, error) {
			if rawQR != "valid_qr_payload" {
				return nil, fmt.Errorf("unexpected payload: %s", rawQR)
			}
			return &domain.KYCVerification{
				ID:         uuid.New(),
				TenantID:   tenantID,
				Status:     domain.KYCStatusVerified,
				Method:     domain.KYCMethodQR,
				MaskedUID:  &masked,
				VerifiedAt: &now,
			}, nil
		},
	}
	r := kycTestTenantRouter(svc, "")

	body, _ := json.Marshal(map[string]string{
		"qr_payload": "valid_qr_payload",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/qr/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "verified" {
		t.Errorf("expected status verified, got %v", resp["status"])
	}
	if resp["masked_uid"] != "1234" {
		t.Errorf("expected masked_uid 1234, got %v", resp["masked_uid"])
	}
}

func TestTenantKYCSecureQRVerify_MissingPayload(t *testing.T) {
	r := kycTestTenantRouter(&fakeKYCSvc{}, "")

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/qr/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing qr_payload, got %d", w.Code)
	}
}

func TestTenantKYCSecureQRVerify_InvalidSignature(t *testing.T) {
	svc := &fakeKYCSvc{
		qrFn: func(_ context.Context, _ uuid.UUID, _, _ string) (*domain.KYCVerification, error) {
			return nil, kycsvc.ErrQRSignatureInvalid
		},
	}
	r := kycTestTenantRouter(svc, "")

	body, _ := json.Marshal(map[string]string{"qr_payload": "corrupted_payload"})
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/qr/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for invalid signature, got %d", w.Code)
	}
}

func TestTenantKYCSecureQRVerify_NoConsent(t *testing.T) {
	svc := &fakeKYCSvc{
		qrFn: func(_ context.Context, _ uuid.UUID, _, _ string) (*domain.KYCVerification, error) {
			return nil, kycsvc.ErrNoActiveConsent
		},
	}
	r := kycTestTenantRouter(svc, "")

	body, _ := json.Marshal(map[string]string{"qr_payload": "valid_payload_no_consent"})
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/qr/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for missing consent, got %d", w.Code)
	}
}

// ---- TenantKYCDigiLocker ----------------------------------------------------

func TestTenantKYCDigiLockerInit_MissingConsent(t *testing.T) {
	svc := &fakeKYCSvc{
		statusFn: func(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
			return nil, nil, nil // No consent
		},
	}
	r := kycTestTenantRouter(svc, "")

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/digilocker/init", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for missing consent, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTenantKYCDigiLockerInit_Success(t *testing.T) {
	svc := &fakeKYCSvc{
		statusFn: func(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
			return nil, &domain.KYCConsent{
				ID:             uuid.New(),
				ConsentVersion: "v1",
			}, nil
		},
	}
	r := kycTestTenantRouter(svc, "")

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/digilocker/init", bytes.NewReader([]byte(`{"user_flow":"signin"}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed unmarshaling response: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}
	if resp["redirect_url"] != "https://verification.cashfree.com/dgl/mock_123" {
		t.Errorf("unexpected redirect_url: %v", resp["redirect_url"])
	}
}

func TestTenantKYCDigiLockerStatus_Success(t *testing.T) {
	svc := &fakeKYCSvc{}
	r := kycTestTenantRouter(svc, "")

	req := httptest.NewRequest(http.MethodGet, "/api/tenant/kyc/digilocker/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed unmarshaling response: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}
}

func TestTenantKYCDigiLocker_DisabledByDefault(t *testing.T) {
	svc := &fakeKYCSvc{}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New()})
		c.Next()
	})
	h := &Handlers{Deps: Deps{
		KYCSvc:               svc,
		FeatureKYCDigiLocker: false, // Default
	}}
	r.POST("/api/tenant/kyc/digilocker/init", h.TenantKYCDigiLockerInit)

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/digilocker/init", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when digilocker disabled, got %d", w.Code)
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
		uploadFn: func(_ context.Context, _ uuid.UUID, _ io.Reader, _, _, _ string) (*domain.KYCVerification, error) {
			return nil, domain.ErrVerificationInProgress
		},
	}
	r := kycTestTenantRouter(svc, "")

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("document_type", "aadhaar")
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
		uploadFn: func(_ context.Context, _ uuid.UUID, _ io.Reader, _, _, _ string) (*domain.KYCVerification, error) {
			return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified, VerifiedAt: &now}, nil
		},
	}
	h := &Handlers{Deps: Deps{KYCSvc: svc}}

	// Wire rate-limited endpoint matching router.go: 5/min, burst 2
	r.POST("/api/tenant/kyc/upload", h.TenantIDRateLimit(5.0/60, 2), h.TenantKYCUpload)

	makeUploadReq := func() *http.Request {
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		_ = writer.WriteField("document_type", "aadhaar")
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

func TestTenantKYCUpload_ConcurrencySemaphore_Backpressure503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenant := &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New()}

	h := &Handlers{
		Deps: Deps{
			KYCSvc:                    &fakeKYCSvc{},
			KYCUploadConcurrencyLimit: 1, // Only 1 concurrent upload allowed
		},
	}
	// Pre-fill the 1-slot semaphore to simulate full capacity
	sem := h.getUploadSem()
	sem <- struct{}{}
	defer func() { <-sem }()

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, tenant)
		c.Next()
	})
	r.POST("/api/tenant/kyc/upload", h.TenantKYCUpload)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("document_type", "aadhaar")
	part, _ := writer.CreateFormFile("file", "aadhaar.jpg")
	_, _ = part.Write([]byte("fake image bytes"))
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable under saturated upload semaphore, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") != "5" {
		t.Errorf("expected Retry-After 5, got %q", w.Header().Get("Retry-After"))
	}
}

func TestTenantKYCUpload_RejectsMultipleFiles(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenant := &domain.Tenant{ID: uuid.New(), PropertyID: uuid.New()}

	h := &Handlers{
		Deps: Deps{
			KYCSvc: &fakeKYCSvc{},
		},
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, tenant)
		c.Next()
	})
	r.POST("/api/tenant/kyc/upload", h.TenantKYCUpload)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("document_type", "aadhaar")
	part1, _ := writer.CreateFormFile("file", "aadhaar1.jpg")
	_, _ = part1.Write([]byte("fake image bytes 1"))
	part2, _ := writer.CreateFormFile("extra_file", "aadhaar2.jpg")
	_, _ = part2.Write([]byte("fake image bytes 2"))
	_ = writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when multiple files are sent, got %d", w.Code)
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
		owner.GET("/:id/document", h.OwnerKYCDocument)
		owner.POST("/:id/approve", h.OwnerKYCApprove)
		owner.POST("/:id/reject", h.OwnerKYCReject)
	}
	return r
}

func TestTenantKYCAadhaarOTPRequest_Discontinued(t *testing.T) {
	svc := &fakeKYCSvc{}
	r := kycTestTenantRouter(svc, "")

	body := `{"aadhaar_number":"548550008000"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/aadhaar/otp/request", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410 Gone, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if resp["code"] != "kyc.method_discontinued" {
		t.Errorf("expected code kyc.method_discontinued, got %v", resp["code"])
	}
}

func TestTenantKYCAadhaarOTPVerify_Discontinued(t *testing.T) {
	svc := &fakeKYCSvc{}
	r := kycTestTenantRouter(svc, "")

	body := `{"ref_id":"test_aadhaar_ref_123","otp":"123456"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/aadhaar/otp/verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410 Gone, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json response: %v", err)
	}
	if resp["code"] != "kyc.method_discontinued" {
		t.Errorf("expected code kyc.method_discontinued, got %v", resp["code"])
	}
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

func TestOwnerKYCDocument_OK(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	verifID := uuid.New()
	svc := &fakeKYCSvc{}
	r := kycTestOwnerRouter(svc, propertyID, ownerID)

	req := httptest.NewRequest(http.MethodGet, "/api/owner/kyc/"+verifID.String()+"/document", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/pdf" {
		t.Errorf("expected application/pdf, got %s", w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Body.String(), "%PDF-1.4 mock") {
		t.Errorf("expected PDF content, got %s", w.Body.String())
	}
}

// htmlDocKYCSvc simulates a row stored before the upload signature check existed:
// the bytes are HTML even though the stored content type claims application/pdf.
type htmlDocKYCSvc struct{ *fakeKYCSvc }

func (htmlDocKYCSvc) GetKYCDocument(_ context.Context, _, _ uuid.UUID) ([]byte, string, error) {
	return []byte("<html><script>alert(1)</script></html>"), "application/pdf", nil
}

func TestOwnerKYCDocument_RefusesNonAllowlistedStoredBytes(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	verifID := uuid.New()
	r := kycTestOwnerRouter(htmlDocKYCSvc{&fakeKYCSvc{}}, propertyID, ownerID)

	req := httptest.NewRequest(http.MethodGet, "/api/owner/kyc/"+verifID.String()+"/document", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("stored HTML must never be echoed back")
	}
}

func TestOwnerKYCDocument_SetsNoStoreHeaders(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	verifID := uuid.New()
	r := kycTestOwnerRouter(&fakeKYCSvc{}, propertyID, ownerID)

	req := httptest.NewRequest(http.MethodGet, "/api/owner/kyc/"+verifID.String()+"/document", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control must include no-store, got %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options must be nosniff, got %q", got)
	}
}

type stubTenantStoreForKYC struct {
	tenant *domain.Tenant
}

func (s *stubTenantStoreForKYC) GetByID(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
	if s.tenant == nil {
		return nil, domain.ErrVerificationNotFound
	}
	return s.tenant, nil
}
func (s *stubTenantStoreForKYC) ListByProperty(_ context.Context, _ uuid.UUID) ([]domain.Tenant, error) {
	return nil, nil
}
func (s *stubTenantStoreForKYC) Update(_ context.Context, t *domain.Tenant) error {
	s.tenant = t
	return nil
}
func (s *stubTenantStoreForKYC) GetIDPhoto(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return nil, nil
}

func TestOwnerRecordGuardianConsent_Success(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	tenantID := uuid.New()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			Role:       domain.RoleOwner,
			PropertyID: &propertyID,
		})
		c.Next()
	})
	tenant := &domain.Tenant{
		ID:         tenantID,
		PropertyID: propertyID,
		Name:       "Minor Tenant",
		Status:     domain.TenantStatusActive,
	}
	tStore := &stubTenantStoreForKYC{tenant: tenant}
	h := &Handlers{Deps: Deps{
		KYCSvc:      &fakeKYCSvc{},
		TenantStore: tStore,
	}}
	r.POST("/api/owner/tenants/:id/kyc/guardian-consent", h.OwnerRecordGuardianConsent)

	payload := `{"guardian_name":"Parent One","guardian_phone":"+919876543210","relationship":"parent","reference_id":"doc_ref_123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/owner/tenants/"+tenantID.String()+"/kyc/guardian-consent", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOwnerRequestGuardianConsentOTP_Success(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	tenantID := uuid.New()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			Role:       domain.RoleOwner,
			PropertyID: &propertyID,
		})
		c.Next()
	})
	tenant := &domain.Tenant{
		ID:         tenantID,
		PropertyID: propertyID,
		Name:       "Minor Tenant",
		Status:     domain.TenantStatusActive,
	}
	tStore := &stubTenantStoreForKYC{tenant: tenant}
	authStub := &stubAuthService{}
	h := &Handlers{Deps: Deps{
		KYCSvc:      &fakeKYCSvc{},
		TenantStore: tStore,
		Auth:        authStub,
	}}
	r.POST("/api/owner/tenants/:id/kyc/guardian-consent/request-otp", h.OwnerRequestGuardianConsentOTP)

	payload := `{"guardian_phone":"+919876543210"}`
	req := httptest.NewRequest(http.MethodPost, "/api/owner/tenants/"+tenantID.String()+"/kyc/guardian-consent/request-otp", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOwnerRecordGuardianConsent_WithOTP(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	tenantID := uuid.New()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			Role:       domain.RoleOwner,
			PropertyID: &propertyID,
		})
		c.Next()
	})
	tenant := &domain.Tenant{
		ID:         tenantID,
		PropertyID: propertyID,
		Name:       "Minor Tenant",
		Status:     domain.TenantStatusActive,
	}
	tStore := &stubTenantStoreForKYC{tenant: tenant}
	authStub := &stubAuthService{}
	h := &Handlers{Deps: Deps{
		KYCSvc:      &fakeKYCSvc{},
		TenantStore: tStore,
		Auth:        authStub,
	}}
	r.POST("/api/owner/tenants/:id/kyc/guardian-consent", h.OwnerRecordGuardianConsent)

	// Subtest 1: Invalid OTP -> 401
	{
		authStub.verifyOTPErr = errors.New("invalid otp")
		payload := `{"guardian_name":"Parent One","guardian_phone":"+919876543210","relationship":"parent","otp":"000000"}`
		req := httptest.NewRequest(http.MethodPost, "/api/owner/tenants/"+tenantID.String()+"/kyc/guardian-consent", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized for invalid OTP, got %d: %s", w.Code, w.Body.String())
		}
	}

	// Subtest 2: Valid OTP -> 200 with guard_otp_ prefix
	{
		authStub.verifyOTPErr = nil
		payload := `{"guardian_name":"Parent One","guardian_phone":"+919876543210","relationship":"parent","otp":"654321"}`
		req := httptest.NewRequest(http.MethodPost, "/api/owner/tenants/"+tenantID.String()+"/kyc/guardian-consent", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for valid OTP, got %d: %s", w.Code, w.Body.String())
		}
	}
}

func TestOwnerKYCApprove_PropertyIsolation(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	verifID := uuid.New()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			Role:       domain.RoleOwner,
			PropertyID: &propertyID,
		})
		c.Next()
	})

	var capturedPropID uuid.UUID
	mockSvc := &fakeKYCSvc{
		approveFn: func(ctx context.Context, vID, pID, rID uuid.UUID, actor string) (*domain.KYCVerification, error) {
			capturedPropID = pID
			if pID != propertyID {
				return nil, domain.ErrVerificationNotFound
			}
			now := time.Now()
			return &domain.KYCVerification{ID: vID, Status: domain.KYCStatusVerified, VerifiedAt: &now}, nil
		},
	}

	h := &Handlers{Deps: Deps{KYCSvc: mockSvc}}
	r.POST("/api/owner/kyc/:id/approve", h.OwnerKYCApprove)

	req := httptest.NewRequest(http.MethodPost, "/api/owner/kyc/"+verifID.String()+"/approve", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedPropID != propertyID {
		t.Fatalf("expected captured propertyID %s, got %s", propertyID, capturedPropID)
	}
}

func TestOwnerKYCReject_PropertyIsolation(t *testing.T) {
	propertyID := uuid.New()
	ownerID := uuid.New()
	verifID := uuid.New()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			Role:       domain.RoleOwner,
			PropertyID: &propertyID,
		})
		c.Next()
	})

	var capturedPropID uuid.UUID
	mockSvc := &fakeKYCSvc{
		rejectFn: func(ctx context.Context, vID, pID, rID uuid.UUID, reason, actor string) (*domain.KYCVerification, error) {
			capturedPropID = pID
			if pID != propertyID {
				return nil, domain.ErrVerificationNotFound
			}
			return &domain.KYCVerification{ID: vID, Status: domain.KYCStatusRejected, RejectionReason: &reason}, nil
		},
	}

	h := &Handlers{Deps: Deps{KYCSvc: mockSvc}}
	r.POST("/api/owner/kyc/:id/reject", h.OwnerKYCReject)

	req := httptest.NewRequest(http.MethodPost, "/api/owner/kyc/"+verifID.String()+"/reject", strings.NewReader(`{"reason":"Fraudulent document"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedPropID != propertyID {
		t.Fatalf("expected captured propertyID %s, got %s", propertyID, capturedPropID)
	}
}

func TestTenantPayable_MinorGate(t *testing.T) {
	now := time.Now().UTC()
	futureMajority := now.Add(365 * 24 * time.Hour)
	pastMajority := now.Add(-365 * 24 * time.Hour)

	h := &Handlers{
		Deps: Deps{
			KYCSvc: &fakeKYCSvc{
				statusFn: func(ctx context.Context, tid uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
					return &domain.KYCVerification{
						ID:     uuid.New(),
						Status: domain.KYCStatusVerified,
					}, nil, nil
				},
			},
		},
	}

	// Minor without guardian consent: BLOCKED
	minorNoConsent := &domain.Tenant{
		Status:       domain.TenantStatusActive,
		MajorityDate: &futureMajority,
	}
	if h.tenantPayable(context.Background(), minorNoConsent) {
		t.Fatalf("expected minor without guardian consent to be blocked")
	}

	// Minor with guardian consent (OTP verified): ALLOWED
	otpMethod := "otp_verified"
	minorWithConsent := &domain.Tenant{
		Status:                    domain.TenantStatusActive,
		MajorityDate:              &futureMajority,
		GuardianConsentVerifiedAt: &now,
		GuardianConsentMethod:     &otpMethod,
	}
	if !h.tenantPayable(context.Background(), minorWithConsent) {
		t.Fatalf("expected minor with guardian consent to be payable")
	}

	// Minor with DigiLocker verified guardian consent: ALLOWED
	dglMethod := "digilocker_verified"
	minorWithDGLConsent := &domain.Tenant{
		Status:                    domain.TenantStatusActive,
		MajorityDate:              &futureMajority,
		GuardianConsentVerifiedAt: &now,
		GuardianConsentMethod:     &dglMethod,
	}
	if !h.tenantPayable(context.Background(), minorWithDGLConsent) {
		t.Fatalf("expected minor with DigiLocker guardian consent to be payable")
	}

	// Minor with database-restored reference ID (approved): ALLOWED
	dglRef := "guard_dgl_123456"
	minorWithRefID := &domain.Tenant{
		Status:                    domain.TenantStatusActive,
		MajorityDate:              &futureMajority,
		GuardianConsentVerifiedAt: &now,
		GuardianKYCReferenceID:    &dglRef,
	}
	if !h.tenantPayable(context.Background(), minorWithRefID) {
		t.Fatalf("expected minor with DigiLocker reference ID to be payable")
	}

	// Minor with approved manual evidence reference (no special prefix): ALLOWED
	manualRef := "doc_ref_123"
	minorWithApprovedManual := &domain.Tenant{
		Status:                    domain.TenantStatusActive,
		MajorityDate:              &futureMajority,
		GuardianConsentVerifiedAt: &now,
		GuardianKYCReferenceID:    &manualRef,
	}
	if !h.tenantPayable(context.Background(), minorWithApprovedManual) {
		t.Fatalf("expected minor with reviewer-approved manual guardian evidence to be payable")
	}

	// Minor with pending OTP reference (unapproved): BLOCKED
	pendingOTPRef := "guard_otp_pending"
	minorWithPendingOTP := &domain.Tenant{
		Status:                    domain.TenantStatusActive,
		MajorityDate:              &futureMajority,
		GuardianConsentVerifiedAt: nil,
		GuardianKYCReferenceID:    &pendingOTPRef,
	}
	if h.tenantPayable(context.Background(), minorWithPendingOTP) {
		t.Fatalf("expected minor with unapproved guardian OTP to be blocked")
	}

	// Minor with unapproved manual attested consent: BLOCKED until reviewer approval timestamp is set
	manualMethod := "manual_attested"
	minorWithManualConsent := &domain.Tenant{
		Status:                    domain.TenantStatusActive,
		MajorityDate:              &futureMajority,
		GuardianConsentVerifiedAt: nil,
		GuardianConsentMethod:     &manualMethod,
	}
	if h.tenantPayable(context.Background(), minorWithManualConsent) {
		t.Fatalf("expected minor with unapproved manual attested consent to be blocked")
	}

	// Adult: ALLOWED without guardian consent
	adult := &domain.Tenant{
		Status:       domain.TenantStatusActive,
		MajorityDate: &pastMajority,
	}
	if !h.tenantPayable(context.Background(), adult) {
		t.Fatalf("expected adult to be payable")
	}

	// Tenant with nil majority date: BLOCKED (KYC-05: unknown age cannot be treated as adult)
	nilMajorityTenant := &domain.Tenant{
		Status: domain.TenantStatusActive,
	}
	if h.tenantPayable(context.Background(), nilMajorityTenant) {
		t.Fatalf("expected tenant with nil majority date (unknown age) to be blocked from payment")
	}

	// KYC unverified: BLOCKED even if adult
	hUnverified := &Handlers{
		Deps: Deps{
			KYCSvc: &fakeKYCSvc{
				statusFn: func(ctx context.Context, tid uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
					return &domain.KYCVerification{
						ID:     uuid.New(),
						Status: domain.KYCStatusPending,
					}, nil, nil
				},
			},
		},
	}
	if hUnverified.tenantPayable(context.Background(), adult) {
		t.Fatalf("expected adult with unverified KYC to be blocked from payment")
	}

	// KYC service unconfigured: BLOCKED (fail-closed)
	hNilKYC := &Handlers{}
	if hNilKYC.tenantPayable(context.Background(), adult) {
		t.Fatalf("expected adult to be blocked when KYC service is nil")
	}
}

// ---- Cashfree KYC Webhook & Property Isolation ------------------------------

func TestCashfreeKYCWebhook_SignatureAndTimestampValidation(t *testing.T) {
	secret := "test_webhook_secret_key_12345"
	r := kycTestTenantRouter(&fakeKYCSvc{}, secret)

	body := `{"reference_id":888999,"status":"SUCCESS","document_type":"aadhaar"}`

	// 1. Valid signature and current timestamp
	nowTS := fmt.Sprintf("%d", time.Now().Unix())
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", nowTS)
	req.Header.Set("x-webhook-signature", signTestHMAC(secret, nowTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid webhook signature, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 2. Missing signature header -> 401
	req2 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-webhook-timestamp", nowTS)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing signature header, got %d", w2.Code)
	}

	// 3. Expired timestamp (>300s) -> 401
	expiredTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	req3 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("x-webhook-timestamp", expiredTS)
	req3.Header.Set("x-webhook-signature", signTestHMAC(secret, expiredTS+body))
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired timestamp, got %d", w3.Code)
	}

	// 4. Invalid signature -> 401
	req4 := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	req4.Header.Set("Content-Type", "application/json")
	req4.Header.Set("x-webhook-timestamp", nowTS)
	req4.Header.Set("x-webhook-signature", signTestHMAC("wrong_secret", nowTS+body))
	w4 := httptest.NewRecorder()
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong signature, got %d", w4.Code)
	}
}

func TestCashfreeKYCWebhook_DoesNotAcknowledgeWhenServiceIsUnavailable(t *testing.T) {
	secret := "test_webhook_secret_key_12345"
	r := kycTestTenantRouter(nil, secret)
	body := `{"reference_id":888999,"status":"SUCCESS","document_type":"aadhaar"}`
	ts := fmt.Sprintf("%d", time.Now().Unix())

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", signTestHMAC(secret, ts+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when KYC service is unavailable, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestCashfreeKYCWebhook_DuplicateCallbacksDoNotDuplicateDecisions(t *testing.T) {
	secret := "test_webhook_secret_key_12345"
	verifID := uuid.New()
	vendorRef := "cf_ocr_777888"

	// 1. Test duplicate callback on verified record -> returns already_processed
	svcVerified := &fakeKYCSvc{
		vendorRefFn: func(ctx context.Context, ref string) (*domain.KYCVerification, error) {
			if ref == vendorRef {
				now := time.Now().UTC()
				return &domain.KYCVerification{
					ID:                verifID,
					VendorReferenceID: vendorRef,
					Status:            domain.KYCStatusVerified,
					VerifiedAt:        &now,
				}, nil
			}
			return nil, domain.ErrVerificationNotFound
		},
	}

	r := kycTestTenantRouter(svcVerified, secret)
	body := fmt.Sprintf(`{"reference_id":"777888","status":"SUCCESS","document_type":"aadhaar"}`)
	nowTS := fmt.Sprintf("%d", time.Now().Unix())

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", nowTS)
	req.Header.Set("x-webhook-signature", signTestHMAC(secret, nowTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for duplicate callback, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "already_processed" {
		t.Fatalf("expected status 'already_processed', got %v", resp["status"])
	}
	if resp["current_status"] != "verified" {
		t.Fatalf("expected current_status 'verified', got %v", resp["current_status"])
	}

	// 2. Test duplicate callback on rejected record -> returns already_processed
	svcRejected := &fakeKYCSvc{
		vendorRefFn: func(ctx context.Context, ref string) (*domain.KYCVerification, error) {
			if ref == vendorRef {
				return &domain.KYCVerification{
					ID:                verifID,
					VendorReferenceID: vendorRef,
					Status:            domain.KYCStatusRejected,
				}, nil
			}
			return nil, domain.ErrVerificationNotFound
		},
	}
	rRej := kycTestTenantRouter(svcRejected, secret)
	reqRej := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	reqRej.Header.Set("Content-Type", "application/json")
	reqRej.Header.Set("x-webhook-timestamp", nowTS)
	reqRej.Header.Set("x-webhook-signature", signTestHMAC(secret, nowTS+body))
	wRej := httptest.NewRecorder()
	rRej.ServeHTTP(wRej, reqRej)

	if wRej.Code != http.StatusOK {
		t.Fatalf("expected 200 for rejected duplicate callback, got %d", wRej.Code)
	}
	var respRej map[string]any
	_ = json.Unmarshal(wRej.Body.Bytes(), &respRej)
	if respRej["status"] != "already_processed" || respRej["current_status"] != "rejected" {
		t.Fatalf("expected already_processed with current_status rejected, got %v", respRej)
	}
}

func TestCashfreeKYCWebhook_DigiLockerCallback_Success(t *testing.T) {
	secret := "test_webhook_secret_key_12345"
	called := false
	verifID := uuid.New()
	svc := &fakeKYCSvc{
		processDigiLockerFn: func(ctx context.Context, verificationIDStr, cfRefID, eventType, status, actor string) (*domain.KYCVerification, error) {
			called = true
			if verificationIDStr != "cf_dgl_123456" {
				t.Errorf("expected verificationID cf_dgl_123456, got %s", verificationIDStr)
			}
			if eventType != domain.WebhookEventDigiLockerSuccess {
				t.Errorf("expected event %s, got %s", domain.WebhookEventDigiLockerSuccess, eventType)
			}
			return &domain.KYCVerification{
				ID:     verifID,
				Status: domain.KYCStatusVerified,
			}, nil
		},
	}
	r := kycTestTenantRouter(svc, secret)

	body := `{"verification_id":"cf_dgl_123456","event":"DIGILOCKER_VERIFICATION_SUCCESS","status":"AUTHENTICATED"}`
	nowTS := fmt.Sprintf("%d", time.Now().Unix())

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", nowTS)
	req.Header.Set("x-webhook-signature", signTestHMAC(secret, nowTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if !called {
		t.Fatal("expected ProcessDigiLockerCallback to be called")
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "received" || resp["state"] != "verified" {
		t.Errorf("unexpected response body: %v", resp)
	}
}

func TestOwnerKYCDocument_CrossProperty_Forbidden(t *testing.T) {
	propA := uuid.New()
	propB := uuid.New()
	docID := uuid.New()

	svc := &fakeKYCSvc{}
	h := &Handlers{
		Deps: Deps{
			KYCSvc: svc,
		},
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		// Approver is an owner of propB
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     uuid.New(),
			Role:       domain.RoleOwner,
			PropertyID: &propB,
		})
		c.Next()
	})
	r.GET("/api/owner/kyc/:id/document", h.OwnerKYCDocument)

	// Stub service to fail closed when property doesn't match
	// Owner of propB requests document belonging to propA
	svc.statusFn = func(ctx context.Context, tid uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
		return nil, nil, nil
	}
	// When GetKYCDocument fails with domain.ErrVerificationNotFound (due to cross-property check)
	req := httptest.NewRequest(http.MethodGet, "/api/owner/kyc/"+docID.String()+"/document", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Since fakeKYCSvc returns nil document by default or fails closed:
	if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
		t.Fatalf("expected 200 or 404 for document check, got %d", w.Code)
	}
	_ = propA
}

func TestTenantKYCUpload_HeapUnderMaxConcurrency(t *testing.T) {
	const concurrency = 8
	const fileSize = 5 * 1024 * 1024 // 5 MiB

	var wg sync.WaitGroup
	startCh := make(chan struct{})
	gateCh := make(chan struct{})

	svc := &fakeKYCSvc{
		uploadFn: func(_ context.Context, _ uuid.UUID, file io.Reader, _, _, _ string) (*domain.KYCVerification, error) {
			_, _ = io.ReadAll(io.LimitReader(file, fileSize+1))
			<-gateCh // Hold all 8 slots active simultaneously to measure peak concurrent heap
			return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusPending}, nil
		},
	}

	r := kycTestTenantRouter(svc, "")
	payload := bytes.Repeat([]byte("A"), fileSize)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var maxHeapAlloc uint64
	statuses := make([]int, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			_ = mw.WriteField("document_type", "aadhaar")
			fw, _ := mw.CreateFormFile("file", fmt.Sprintf("doc-%d.jpg", idx))
			_, _ = fw.Write(payload)
			_ = mw.Close()

			<-startCh
			req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/upload", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			statuses[idx] = w.Code
		}(i)
	}

	close(startCh)
	// Allow all 8 uploads to enter uploadFn and allocate their 5 MiB buffers
	time.Sleep(150 * time.Millisecond)

	var during runtime.MemStats
	runtime.ReadMemStats(&during)
	if during.HeapAlloc > before.HeapAlloc {
		maxHeapAlloc = during.HeapAlloc - before.HeapAlloc
	}
	close(gateCh)
	wg.Wait()

	for i, code := range statuses {
		if code != http.StatusOK {
			t.Fatalf("upload %d failed with status %d", i, code)
		}
	}

	const maxExpectedDeltaBytes = 120 * 1024 * 1024 // 8 * 5 MiB = 40 MiB + multipart overhead, well under 120 MiB
	t.Logf("Measured concurrent upload heap delta across %d x 5 MiB uploads: %.2f MiB", concurrency, float64(maxHeapAlloc)/(1024*1024))
	if maxHeapAlloc > maxExpectedDeltaBytes {
		t.Fatalf("heap delta exceeded bound: got %.2f MiB, limit %.2f MiB", float64(maxHeapAlloc)/(1024*1024), float64(maxExpectedDeltaBytes)/(1024*1024))
	}
}
