package api_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// inMemoryLogBuffer captures structured logs to verify zero PII leaks
type inMemoryLogBuffer struct {
	buf *bytes.Buffer
}

func (h *inMemoryLogBuffer) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *inMemoryLogBuffer) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	b.WriteString("\n")
	h.buf.WriteString(b.String())
	return nil
}
func (h *inMemoryLogBuffer) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *inMemoryLogBuffer) WithGroup(_ string) slog.Handler      { return h }

type stubPIITenantStore struct {
	ten *domain.Tenant
}

func (s *stubPIITenantStore) GetByID(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
	return s.ten, nil
}
func (s *stubPIITenantStore) ListByProperty(_ context.Context, _ uuid.UUID) ([]domain.Tenant, error) {
	return []domain.Tenant{*s.ten}, nil
}
func (s *stubPIITenantStore) Update(_ context.Context, t *domain.Tenant) error {
	s.ten = t
	return nil
}
func (s *stubPIITenantStore) GetIDPhoto(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return []byte("fake-photo"), nil
}

type stubPIITenantService struct {
	attachedPhone string
}

func (s *stubPIITenantService) CreateTenant(_ context.Context, in domain.NewTenantInput, _ int64) (*domain.Tenant, error) {
	return &domain.Tenant{
		ID:         uuid.New(),
		PropertyID: in.PropertyID,
		Name:       in.Name,
		Phone:      in.Phone,
		RoomNumber: in.RoomNumber,
		RentAmount: in.RentAmount,
		DueDay:     &in.DueDay,
		Status:     domain.TenantStatusActive,
	}, nil
}
func (s *stubPIITenantService) UpdateTenant(_ context.Context, _ *domain.Tenant) error { return nil }
func (s *stubPIITenantService) Vacate(_ context.Context, _ uuid.UUID) error             { return nil }
func (s *stubPIITenantService) LogNotice(_ context.Context, _ uuid.UUID, _ time.Time) error {
	return nil
}
func (s *stubPIITenantService) AttachPhone(_ context.Context, _ uuid.UUID, phone string) error {
	s.attachedPhone = phone
	return nil
}

type stubPIIKYCService struct{}

func (s *stubPIIKYCService) RecordConsent(_ context.Context, _ uuid.UUID, _, _, _, _, _, _ string) (*domain.KYCConsent, error) {
	return &domain.KYCConsent{ID: uuid.New()}, nil
}
func (s *stubPIIKYCService) VerifySecureQR(_ context.Context, _ uuid.UUID, _, _ string) (*domain.KYCVerification, error) {
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified}, nil
}
func (s *stubPIIKYCService) VerifyAadhaarDocument(_ context.Context, _ uuid.UUID, _ io.Reader, _, _, _ string) (*domain.KYCVerification, error) {
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified}, nil
}
func (s *stubPIIKYCService) GetAttestedPhoto(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return []byte("fake-attested-photo"), nil
}
func (s *stubPIIKYCService) GetKYCDocument(_ context.Context, _, _ uuid.UUID) ([]byte, string, error) {
	return []byte("fake-document"), "application/pdf", nil
}
func (s *stubPIIKYCService) RevokeConsent(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}
func (s *stubPIIKYCService) GetStatus(_ context.Context, _ uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
	return nil, nil, nil
}
func (s *stubPIIKYCService) GetOwnerView(_ context.Context, _ uuid.UUID) (*domain.KYCVerification, []domain.KYCAuditLog, error) {
	return nil, nil, nil
}
func (s *stubPIIKYCService) GetVerificationByVendorRefID(_ context.Context, _ string) (*domain.KYCVerification, error) {
	return nil, nil
}
func (s *stubPIIKYCService) ClearDuplicateFlag(_ context.Context, _ uuid.UUID, _, _ string) error {
	return nil
}
func (s *stubPIIKYCService) InitiateAadhaarOTP(_ context.Context, _ uuid.UUID, _, _ string) (string, error) {
	return "test_ref", nil
}
func (s *stubPIIKYCService) CompleteAadhaarOTP(_ context.Context, _ uuid.UUID, _, _, _ string) (*domain.KYCVerification, error) {
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified}, nil
}
func (s *stubPIIKYCService) ListPendingReviews(_ context.Context, _ uuid.UUID) ([]domain.KYCVerification, error) {
	return nil, nil
}
func (s *stubPIIKYCService) ApproveManualReview(_ context.Context, _, _, _ uuid.UUID, _ string) (*domain.KYCVerification, error) {
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusVerified}, nil
}
func (s *stubPIIKYCService) RejectManualReview(_ context.Context, _, _, _ uuid.UUID, _, _ string) (*domain.KYCVerification, error) {
	return &domain.KYCVerification{ID: uuid.New(), Status: domain.KYCStatusRejected}, nil
}
func (s *stubPIIKYCService) RecordGuardianConsent(_ context.Context, tenantID uuid.UUID, guardianName, guardianPhone, relation, refID string, _ string) (*domain.Tenant, error) {
	now := time.Now().UTC()
	return &domain.Tenant{
		ID:                        tenantID,
		GuardianName:              &guardianName,
		GuardianPhone:             &guardianPhone,
		GuardianRelation:          &relation,
		GuardianKYCReferenceID:    &refID,
		GuardianConsentVerifiedAt: &now,
	}, nil
}

type stubPIIAuthService struct {
	requestedPhone string
}

func (s *stubPIIAuthService) RequestOTP(_ context.Context, phone string) error {
	s.requestedPhone = phone
	return nil
}
func (s *stubPIIAuthService) RequestOTPWithPurpose(_ context.Context, phone, _ string) error {
	s.requestedPhone = phone
	return nil
}
func (s *stubPIIAuthService) RequestStepUpOTP(_ context.Context, phone, _ string, _ *uuid.UUID) error {
	s.requestedPhone = phone
	return nil
}
func (s *stubPIIAuthService) RequestStepUpOTPWithChannel(_ context.Context, phone, _ string, _ *uuid.UUID) (string, string, error) {
	s.requestedPhone = phone
	return "email", "admin@example.com", nil
}
func (s *stubPIIAuthService) VerifyOTPAndIssueToken(_ context.Context, phone, _ string) (string, *domain.User, error) {
	return "test-token", &domain.User{ID: uuid.New(), Phone: phone, Role: domain.RoleTenant}, nil
}
func (s *stubPIIAuthService) VerifyStepUpOTP(_ context.Context, _, _ string) error {
	return nil
}
func (s *stubPIIAuthService) VerifyStepUpOTPSpecific(_ context.Context, _, _, _ string, _ *uuid.UUID) error {
	return nil
}
func (s *stubPIIAuthService) VerifyFirebaseAndIssueToken(_ context.Context, _, _ string) (string, *domain.User, error) {
	return "test-token", &domain.User{ID: uuid.New(), Role: domain.RoleTenant}, nil
}
func (s *stubPIIAuthService) VerifyFirebaseStepUp(_ context.Context, _ string, _ time.Duration) (auth.FirebaseIdentity, error) {
	return auth.FirebaseIdentity{}, nil
}
func (s *stubPIIAuthService) IssueSession(_ context.Context, _ *domain.User) (string, string, error) {
	return "acc", "ref", nil
}
func (s *stubPIIAuthService) RotateRefreshToken(_ context.Context, _ string) (string, string, *domain.User, error) {
	return "acc", "ref", &domain.User{ID: uuid.New(), Role: domain.RoleTenant}, nil
}
func (s *stubPIIAuthService) RevokeSession(_ context.Context, _ string) error {
	return nil
}
func (s *stubPIIAuthService) RevokeUserSessions(_ context.Context, _ uuid.UUID) error {
	return nil
}
func (s *stubPIIAuthService) IssueSessionWithClient(_ context.Context, _ *domain.User, _, _ string) (string, string, error) {
	return "acc", "ref", nil
}
func (s *stubPIIAuthService) RotateRefreshTokenWithClient(_ context.Context, _, _, _ string) (string, string, *domain.User, error) {
	return "acc", "ref", &domain.User{ID: uuid.New(), Role: domain.RoleTenant}, nil
}
func (s *stubPIIAuthService) ListSessions(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ string) ([]domain.ActiveSession, error) {
	return nil, nil
}
func (s *stubPIIAuthService) RevokeDeviceSession(_ context.Context, _, _ uuid.UUID) error {
	return nil
}
func (s *stubPIIAuthService) RevokeOtherSessions(_ context.Context, _ uuid.UUID, _ *uuid.UUID, _ string) error {
	return nil
}

// TestGate14_ZeroPIILeakInAPIHandlers tests that API handlers touching phone and Aadhaar
// do not leak raw 12-digit Aadhaar numbers or unmasked 10-digit phone numbers into logs.
func TestGate14_ZeroPIILeakInAPIHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logBuf := &bytes.Buffer{}
	handler := &inMemoryLogBuffer{buf: logBuf}
	logger := slog.New(handler)
	prevLogger := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prevLogger)

	rawAadhaar := "293847561028" // 12-digit Aadhaar format
	rawPhone := "9876543210"     // 10-digit Indian phone format
	propID := uuid.New()
	tenantID := uuid.New()

	ten := &domain.Tenant{
		ID:         tenantID,
		PropertyID: propID,
		Phone:      &rawPhone,
	}
	tenantStore := &stubPIITenantStore{ten: ten}
	tenantSvc := &stubPIITenantService{}
	kycSvc := &stubPIIKYCService{}
	authSvc := &stubPIIAuthService{}

	h := &api.Handlers{
		Deps: api.Deps{
			TenantStore: tenantStore,
			Tenants:     tenantSvc,
			KYCSvc:      kycSvc,
			Auth:        authSvc,
		},
	}

	router := gin.New()

	// Tenant route touching Aadhaar
	router.POST("/tenant/aadhaar", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		h.TenantAadhaar(c)
	})

	// Auth routes touching phone
	router.POST("/auth/otp/request", h.OTPRequest)
	router.POST("/auth/otp/verify", h.OTPVerify)

	// Owner routes touching phone and tenant identity photos
	ownerGroup := router.Group("/owner")
	ownerGroup.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     uuid.New(),
			Role:       domain.RoleOwner,
			PropertyID: &propID,
		})
		c.Next()
	})
	ownerGroup.POST("/tenants/:id/attach-phone", h.TenantAttachPhone)
	ownerGroup.POST("/tenants", h.CreateTenant)
	ownerGroup.GET("/tenants/:id/kyc-photo", h.OwnerTenantKYCPhoto)
	ownerGroup.GET("/tenants/:id/id-photo", h.TenantIDPhoto)

	// 1. Send raw Aadhaar in QR payload to /tenant/aadhaar
	aadhaarPayload := fmt.Sprintf(`{"consent":true,"confirm":true,"qr_payload":"<PrintLetterBarcodeData uid=\"%s\" name=\"John Doe\" gender=\"M\" dob=\"01-01-1995\"/>"}`, rawAadhaar)
	reqAadhaar := httptest.NewRequest(http.MethodPost, "/tenant/aadhaar", bytes.NewBufferString(aadhaarPayload))
	reqAadhaar.Header.Set("Content-Type", "application/json")
	wAadhaar := httptest.NewRecorder()
	router.ServeHTTP(wAadhaar, reqAadhaar)

	// 2. Send phone payload to /auth/otp/request
	phonePayload := fmt.Sprintf(`{"phone":"%s"}`, rawPhone)
	reqPhone := httptest.NewRequest(http.MethodPost, "/auth/otp/request", bytes.NewBufferString(phonePayload))
	reqPhone.Header.Set("Content-Type", "application/json")
	wPhone := httptest.NewRecorder()
	router.ServeHTTP(wPhone, reqPhone)

	// 3. Send phone payload to /auth/otp/verify
	verifyPayload := fmt.Sprintf(`{"phone":"%s","otp":"123456"}`, rawPhone)
	reqVerify := httptest.NewRequest(http.MethodPost, "/auth/otp/verify", bytes.NewBufferString(verifyPayload))
	reqVerify.Header.Set("Content-Type", "application/json")
	wVerify := httptest.NewRecorder()
	router.ServeHTTP(wVerify, reqVerify)

	// 4. Send phone payload to /owner/tenants/:id/attach-phone
	attachPayload := fmt.Sprintf(`{"phone":"%s"}`, rawPhone)
	reqAttach := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/tenants/%s/attach-phone", tenantID), bytes.NewBufferString(attachPayload))
	reqAttach.Header.Set("Content-Type", "application/json")
	wAttach := httptest.NewRecorder()
	router.ServeHTTP(wAttach, reqAttach)

	// 5. Send phone payload to /owner/tenants
	createPayload := fmt.Sprintf(`{"name":"Jane Doe","phone":"%s","room_number":"101","rent_amount":1500000,"due_day":1}`, rawPhone)
	reqCreate := httptest.NewRequest(http.MethodPost, "/owner/tenants", bytes.NewBufferString(createPayload))
	reqCreate.Header.Set("Content-Type", "application/json")
	wCreate := httptest.NewRecorder()
	router.ServeHTTP(wCreate, reqCreate)

	// 6. Request KYC photo
	reqKYCPhoto := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/owner/tenants/%s/kyc-photo", tenantID), nil)
	wKYCPhoto := httptest.NewRecorder()
	router.ServeHTTP(wKYCPhoto, reqKYCPhoto)

	// 7. Request ID photo
	reqIDPhoto := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/owner/tenants/%s/id-photo", tenantID), nil)
	wIDPhoto := httptest.NewRecorder()
	router.ServeHTTP(wIDPhoto, reqIDPhoto)

	logs := logBuf.String()

	// Regulatory check: Zero 12-digit Aadhaar numbers in application logs
	aadhaarRegex := regexp.MustCompile(`\b[2-9]{1}[0-9]{3}[0-9]{4}[0-9]{4}\b`)
	if matches := aadhaarRegex.FindAllString(logs, -1); len(matches) > 0 {
		t.Fatalf("[LEAK DETECTED] Raw 12-digit Aadhaar number found in log output: %v", matches)
	}

	// Regulatory check: Assert logs do not contain raw Aadhaar string
	if strings.Contains(logs, rawAadhaar) {
		t.Fatalf("[LEAK DETECTED] Raw Aadhaar %s found in logs: %s", rawAadhaar, logs)
	}

	// Regulatory check: Assert logs do not leak raw phone number
	if strings.Contains(logs, rawPhone) {
		t.Fatalf("[LEAK DETECTED] Raw phone %s found in logs: %s", rawPhone, logs)
	}
}
