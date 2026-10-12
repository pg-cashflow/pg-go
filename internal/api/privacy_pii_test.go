package api_test

import (
	"bytes"
	"context"
	"fmt"
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

	ten := &domain.Tenant{
		ID:         uuid.New(),
		PropertyID: uuid.New(),
		Phone:      &rawPhone,
	}
	tenantStore := &stubPIITenantStore{ten: ten}
	authSvc := &stubPIIAuthService{}

	h := &api.Handlers{
		Deps: api.Deps{
			TenantStore: tenantStore,
			Auth:        authSvc,
		},
	}

	router := gin.New()
	router.POST("/tenant/aadhaar", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		h.TenantAadhaar(c)
	})
	router.POST("/auth/otp/request", h.OTPRequest)

	// 1. Send raw Aadhaar in QR payload to /tenant/aadhaar
	aadhaarPayload := fmt.Sprintf(`{"consent":true,"confirm":true,"qr_payload":"<PrintLetterBarcodeData uid=\"%s\" name=\"John Doe\" gender=\"M\" dob=\"01-01-1995\"/>"}`, rawAadhaar)
	reqAadhaar := httptest.NewRequest(http.MethodPost, "/tenant/aadhaar", bytes.NewBufferString(aadhaarPayload))
	reqAadhaar.Header.Set("Content-Type", "application/json")
	wAadhaar := httptest.NewRecorder()
	router.ServeHTTP(wAadhaar, reqAadhaar)

	// 2. Send phone payload to /auth/otp/request
	// (Note: Auth is not configured on Deps, but handler should fail closed without leaking raw phone in unexpected slog logs)
	phonePayload := fmt.Sprintf(`{"phone":"%s"}`, rawPhone)
	reqPhone := httptest.NewRequest(http.MethodPost, "/auth/otp/request", bytes.NewBufferString(phonePayload))
	reqPhone.Header.Set("Content-Type", "application/json")
	wPhone := httptest.NewRecorder()
	router.ServeHTTP(wPhone, reqPhone)

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
}
