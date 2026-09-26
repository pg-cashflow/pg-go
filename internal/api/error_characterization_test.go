package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestPaymentClientErrorCharacterization(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantMsg    string
		wantCode   apierr.Code
	}{
		{payment.ErrDuplicateTxn, http.StatusConflict, payment.ErrDuplicateTxn.Error(), apierr.CodePaymentDuplicateTxn},
		{payment.ErrCashPartialNotAllowed, http.StatusBadRequest, payment.ErrCashPartialNotAllowed.Error(), apierr.CodePaymentCashPartialNotAllowed},
		{payment.ErrDueNotOpen, http.StatusBadRequest, payment.ErrDueNotOpen.Error(), apierr.CodePaymentDueNotOpen},
		{payment.ErrNoDepositDue, http.StatusBadRequest, payment.ErrNoDepositDue.Error(), apierr.CodePaymentNoDepositDue},
		{payment.ErrEmptyTxnID, http.StatusBadRequest, payment.ErrEmptyTxnID.Error(), apierr.CodePaymentEmptyTxnId},
		{payment.ErrAmbiguous, http.StatusBadRequest, payment.ErrAmbiguous.Error(), apierr.CodePaymentAmbiguousMatch},
		{payment.ErrNoMatch, http.StatusBadRequest, payment.ErrNoMatch.Error(), apierr.CodePaymentNoMatch},
	}

	for _, tc := range cases {
		t.Run(string(tc.wantCode), func(t *testing.T) {
			got := paymentClientErr(tc.err)
			var ce *ClientError
			if !errors.As(got, &ce) {
				t.Fatalf("expected ClientError, got %T: %v", got, got)
			}
			if ce.HTTPStatus != tc.wantStatus {
				t.Errorf("status = %d, want %d", ce.HTTPStatus, tc.wantStatus)
			}
			if ce.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", ce.Message, tc.wantMsg)
			}
			if ce.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", ce.Code, tc.wantCode)
			}
		})
	}
}

func TestFinanceClientErrorCharacterization(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantMsg    string
		wantCode   apierr.Code
	}{
		{finance.ErrDuplicateIdempotency, http.StatusConflict, "duplicate request", apierr.CodeFinanceDuplicateRequest},
		{finance.ErrIdempotencyRequired, http.StatusBadRequest, "Idempotency-Key required", apierr.CodeFinanceIdempotencyRequired},
		{finance.ErrInvalidAmount, http.StatusBadRequest, finance.ErrInvalidAmount.Error(), apierr.CodeFinanceInvalidAmount},
		{finance.ErrInvalidKind, http.StatusBadRequest, finance.ErrInvalidKind.Error(), apierr.CodeFinanceInvalidKind},
		{finance.ErrOverpay, http.StatusBadRequest, finance.ErrOverpay.Error(), apierr.CodeFinanceOverpay},
		{finance.ErrExpenseNotPayable, http.StatusBadRequest, finance.ErrExpenseNotPayable.Error(), apierr.CodeFinanceExpenseNotPayable},
		{finance.ErrPolicyExceeded, http.StatusBadRequest, finance.ErrPolicyExceeded.Error(), apierr.CodeFinancePolicyExceeded},
		{finance.ErrApprovalRequired, http.StatusBadRequest, finance.ErrApprovalRequired.Error(), apierr.CodeFinanceApprovalRequired},
		{finance.ErrPeriodNotCloseable, http.StatusBadRequest, finance.ErrPeriodNotCloseable.Error(), apierr.CodeFinancePeriodNotCloseable},
		{finance.ErrNotFound, http.StatusNotFound, "not found", apierr.CodeFinanceNotFound},
		{finance.ErrForbidden, http.StatusForbidden, "forbidden", apierr.CodeFinanceForbidden},
		{finance.ErrDisabled, http.StatusServiceUnavailable, "finance disabled", apierr.CodeFinanceDisabled},
	}

	for _, tc := range cases {
		t.Run(string(tc.wantCode), func(t *testing.T) {
			got := financeClientErr(tc.err)
			var ce *ClientError
			if !errors.As(got, &ce) {
				t.Fatalf("expected ClientError, got %T: %v", got, got)
			}
			if ce.HTTPStatus != tc.wantStatus {
				t.Errorf("status = %d, want %d", ce.HTTPStatus, tc.wantStatus)
			}
			if ce.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", ce.Message, tc.wantMsg)
			}
			if ce.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", ce.Code, tc.wantCode)
			}
		})
	}
}

func TestJoinClientErrorCharacterization(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantMsg    string
		wantCode   apierr.Code
	}{
		{joinsvc.ErrInvalidInvite, http.StatusNotFound, joinsvc.ErrInvalidInvite.Error(), apierr.CodeJoinInvalidInvite},
		{joinsvc.ErrNotPending, http.StatusNotFound, joinsvc.ErrNotPending.Error(), apierr.CodeJoinNoPendingRequest},
		{joinsvc.ErrNotFound, http.StatusNotFound, joinsvc.ErrNotFound.Error(), apierr.CodeJoinNotFound},
		{joinsvc.ErrAlreadyOnboarded, http.StatusConflict, joinsvc.ErrAlreadyOnboarded.Error(), apierr.CodeJoinAlreadyOnboarded},
		{joinsvc.ErrAlreadyActive, http.StatusConflict, joinsvc.ErrAlreadyActive.Error(), apierr.CodeJoinAlreadyActive},
		{joinsvc.ErrNameRequired, http.StatusBadRequest, joinsvc.ErrNameRequired.Error(), apierr.CodeJoinNameRequired},
		{joinsvc.ErrConsentRequired, http.StatusBadRequest, joinsvc.ErrConsentRequired.Error(), apierr.CodeJoinConsentRequired},
		{joinsvc.ErrPhotoRequired, http.StatusBadRequest, joinsvc.ErrPhotoRequired.Error(), apierr.CodeJoinPhotoRequired},
		{joinsvc.ErrProfileIncomplete, http.StatusBadRequest, joinsvc.ErrProfileIncomplete.Error(), apierr.CodeJoinProfileIncomplete},
		{joinsvc.ErrNotPendingOwner, http.StatusBadRequest, joinsvc.ErrNotPendingOwner.Error(), apierr.CodeJoinRequestNotPending},
		{joinsvc.ErrNotAwaitingAssign, http.StatusBadRequest, joinsvc.ErrNotAwaitingAssign.Error(), apierr.CodeJoinNotAwaitingAssignment},
	}

	for _, tc := range cases {
		t.Run(string(tc.wantCode), func(t *testing.T) {
			got := joinHTTPError(tc.err)
			var ce *ClientError
			if !errors.As(got, &ce) {
				t.Fatalf("expected ClientError, got %T: %v", got, got)
			}
			if ce.HTTPStatus != tc.wantStatus {
				t.Errorf("status = %d, want %d", ce.HTTPStatus, tc.wantStatus)
			}
			if ce.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", ce.Message, tc.wantMsg)
			}
			if ce.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", ce.Code, tc.wantCode)
			}
		})
	}
}

func TestAuthMiddlewareCharacterization(t *testing.T) {
	jwtSecret := "test-secret-characterization-32bytes"

	t.Run("missing bearer token", func(t *testing.T) {
		r := gin.New()
		r.Use(auth.RequireOwner(jwtSecret, nil))
		r.GET("/test", func(c *gin.Context) { c.Status(200) })

		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
		expectedBody := `{"error":"missing bearer token","code":"auth.missingToken"}`
		if w.Body.String() != expectedBody {
			t.Errorf("got body %q, want %q", w.Body.String(), expectedBody)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		r := gin.New()
		r.Use(auth.RequireOwner(jwtSecret, nil))
		r.GET("/test", func(c *gin.Context) { c.Status(200) })

		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "Bearer invalid-junk-token")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
		expectedBody := `{"error":"invalid token","code":"auth.invalidToken"}`
		if w.Body.String() != expectedBody {
			t.Errorf("got body %q, want %q", w.Body.String(), expectedBody)
		}
	})

	t.Run("forbidden role", func(t *testing.T) {
		token, err := auth.IssueToken(jwtSecret, &domain.User{Role: domain.RoleTenant, TokenVersion: 1})
		if err != nil {
			t.Fatal(err)
		}

		r := gin.New()
		r.Use(auth.RequireOwner(jwtSecret, nil))
		r.GET("/test", func(c *gin.Context) { c.Status(200) })

		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", w.Code)
		}
		expectedBody := `{"error":"forbidden","code":"auth.forbidden"}`
		if w.Body.String() != expectedBody {
			t.Errorf("got body %q, want %q", w.Body.String(), expectedBody)
		}
	})
}

type stubAuthService struct {
	reqOTPErr    error
	verifyOTPTkn string
	verifyOTPUsr *domain.User
	verifyOTPErr error
	verifyFBTkn  string
	verifyFBUsr  *domain.User
	verifyFBErr  error
}

func (s *stubAuthService) RequestOTP(ctx context.Context, phone string) error {
	return s.reqOTPErr
}

func (s *stubAuthService) RequestOTPWithPurpose(ctx context.Context, phone, purpose string) error {
	return s.reqOTPErr
}

func (s *stubAuthService) VerifyOTPAndIssueToken(ctx context.Context, phone, otp string) (string, *domain.User, error) {
	return s.verifyOTPTkn, s.verifyOTPUsr, s.verifyOTPErr
}

func (s *stubAuthService) VerifyStepUpOTP(ctx context.Context, phone, otp string) error {
	return s.verifyOTPErr
}

func (s *stubAuthService) VerifyFirebaseAndIssueToken(ctx context.Context, idToken, inviteCode string) (string, *domain.User, error) {
	return s.verifyFBTkn, s.verifyFBUsr, s.verifyFBErr
}

func (s *stubAuthService) VerifyFirebaseStepUp(ctx context.Context, idToken string, maxAge time.Duration) (auth.FirebaseIdentity, error) {
	if s.verifyFBErr != nil {
		return auth.FirebaseIdentity{}, s.verifyFBErr
	}
	return auth.FirebaseIdentity{UID: "stub-firebase-uid", AuthTime: time.Now().UTC()}, nil
}

func TestOTPVerifyCharacterization(t *testing.T) {
	cases := []struct {
		name       string
		body       any
		stubErr    error
		wantStatus int
		wantMsg    string
		wantCode   apierr.Code
	}{
		{
			name:       "invalid body - empty",
			body:       map[string]string{},
			wantStatus: http.StatusBadRequest,
			wantMsg:    "phone and otp required",
			wantCode:   apierr.CodeRequestInvalidBody,
		},
		{
			name:       "account not found",
			body:       map[string]string{"phone": "+919876543210", "otp": "123456"},
			stubErr:    auth.ErrNoAccount,
			wantStatus: http.StatusNotFound,
			wantMsg:    auth.ErrNoAccount.Error(),
			wantCode:   apierr.CodeAuthNoAccount,
		},
		{
			name:       "tenant vacated",
			body:       map[string]string{"phone": "+919876543210", "otp": "123456"},
			stubErr:    auth.ErrTenantVacated,
			wantStatus: http.StatusForbidden,
			wantMsg:    auth.ErrTenantVacated.Error(),
			wantCode:   apierr.CodeAuthAccessRevoked,
		},
		{
			name:       "otp expired",
			body:       map[string]string{"phone": "+919876543210", "otp": "123456"},
			stubErr:    auth.ErrOTPExpired,
			wantStatus: http.StatusUnauthorized,
			wantMsg:    auth.ErrOTPExpired.Error(),
			wantCode:   apierr.CodeAuthOtpExpired,
		},
		{
			name:       "otp locked",
			body:       map[string]string{"phone": "+919876543210", "otp": "123456"},
			stubErr:    auth.ErrOTPLocked,
			wantStatus: http.StatusUnauthorized,
			wantMsg:    auth.ErrOTPLocked.Error(),
			wantCode:   apierr.CodeAuthOtpLocked,
		},
		{
			name:       "invalid otp",
			body:       map[string]string{"phone": "+919876543210", "otp": "000000"},
			stubErr:    auth.ErrInvalidOTP,
			wantStatus: http.StatusUnauthorized,
			wantMsg:    auth.ErrInvalidOTP.Error(),
			wantCode:   apierr.CodeAuthInvalidOtp,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubAuthService{verifyOTPErr: tc.stubErr}
			h := &Handlers{Deps: Deps{Auth: stub}}

			bodyBytes, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/auth/otp/verify", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			h.OTPVerify(c)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}

			var env apierr.ErrorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("failed to decode error envelope: %v", err)
			}
			if env.Error != tc.wantMsg {
				t.Errorf("error = %q, want %q", env.Error, tc.wantMsg)
			}
			if env.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Code, tc.wantCode)
			}
		})
	}
}

func TestFirebaseAuthCharacterization(t *testing.T) {
	cases := []struct {
		name       string
		body       any
		stubErr    error
		wantStatus int
		wantMsg    string
		wantCode   apierr.Code
	}{
		{
			name:       "invalid body - empty",
			body:       map[string]string{},
			wantStatus: http.StatusBadRequest,
			wantMsg:    "id_token required",
			wantCode:   apierr.CodeRequestInvalidBody,
		},
		{
			name:       "firebase not configured",
			body:       map[string]string{"id_token": "mock-fb-token"},
			stubErr:    auth.ErrFirebaseNotConfigured,
			wantStatus: http.StatusServiceUnavailable,
			wantMsg:    "firebase auth not configured",
			wantCode:   apierr.CodeAuthFirebaseNotConfigured,
		},
		{
			name:       "email not verified",
			body:       map[string]string{"id_token": "mock-fb-token"},
			stubErr:    auth.ErrEmailNotVerified,
			wantStatus: http.StatusForbidden,
			wantMsg:    "email is not verified with Google — please verify your email or use phone OTP",
			wantCode:   apierr.CodeAuthEmailNotVerified,
		},
		{
			name:       "account not found",
			body:       map[string]string{"id_token": "mock-fb-token"},
			stubErr:    auth.ErrNoAccount,
			wantStatus: http.StatusNotFound,
			wantMsg:    "account not found — if you are an owner, verify your registered phone/email; if you are a tenant, get the invite code from your owner",
			wantCode:   apierr.CodeAuthNoAccount,
		},
		{
			name:       "invalid invite",
			body:       map[string]string{"id_token": "mock-fb-token", "invite_code": "BADCODE"},
			stubErr:    auth.ErrInvalidInvite,
			wantStatus: http.StatusNotFound,
			wantMsg:    "get the PG invite code from your owner",
			wantCode:   apierr.CodeAuthInvalidInvite,
		},
		{
			name:       "tenant access revoked",
			body:       map[string]string{"id_token": "mock-fb-token"},
			stubErr:    auth.ErrTenantVacated,
			wantStatus: http.StatusForbidden,
			wantMsg:    "access revoked",
			wantCode:   apierr.CodeAuthAccessRevoked,
		},
		{
			name:       "invalid firebase token / generic auth error",
			body:       map[string]string{"id_token": "invalid-token"},
			stubErr:    errors.New("signature verification failed"),
			wantStatus: http.StatusUnauthorized,
			wantMsg:    "authentication failed",
			wantCode:   apierr.CodeAuthInvalidFirebaseToken,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubAuthService{verifyFBErr: tc.stubErr}
			h := &Handlers{Deps: Deps{Auth: stub}}

			bodyBytes, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/auth/firebase", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req

			h.FirebaseAuth(c)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}

			var env apierr.ErrorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("failed to decode error envelope: %v", err)
			}
			if env.Error != tc.wantMsg {
				t.Errorf("error = %q, want %q", env.Error, tc.wantMsg)
			}
			if env.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Code, tc.wantCode)
			}
		})
	}
}

func TestScopeClaimsCharacterization(t *testing.T) {
	t.Run("propertyIDFromClaims missing", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/test", nil)

		pid, ok := propertyIDFromClaims(c)
		if ok || pid != [16]byte{} {
			t.Fatalf("expected false and nil UUID, got ok=%v, pid=%v", ok, pid)
		}
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		var env apierr.ErrorEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Error != "no property scope" || env.Code != apierr.CodeAuthNoPropertyScope {
			t.Errorf("unexpected envelope: %+v", env)
		}
	})

	t.Run("userIDFromClaims missing", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/test", nil)

		uid, ok := userIDFromClaims(c)
		if ok || uid != [16]byte{} {
			t.Fatalf("expected false and nil UUID, got ok=%v, uid=%v", ok, uid)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		var env apierr.ErrorEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Error != "unauthorized" || env.Code != apierr.CodeAuthUnauthorized {
			t.Errorf("unexpected envelope: %+v", env)
		}
	})
}

