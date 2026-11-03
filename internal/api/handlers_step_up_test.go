package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type authorizationUserStore struct {
	refundStepUpUserStore
	lookupErr error
}

func (s *authorizationUserStore) GetByPropertyAndRole(context.Context, uuid.UUID, domain.Role) ([]domain.User, error) {
	return s.owners, s.lookupErr
}

type authorizationFirebaseService struct {
	stubPayoutAuthService
	identity auth.FirebaseIdentity
}

func (s *authorizationFirebaseService) VerifyFirebaseStepUp(context.Context, string, time.Duration) (auth.FirebaseIdentity, error) {
	return s.identity, nil
}

func TestFinancialAuthorizationWithoutMakerRequiresStepUp(t *testing.T) {
	uid, pid := uuid.New(), uuid.New()
	user := &domain.User{ID: uid, Phone: "+919000000001"}
	h := &Handlers{Deps: Deps{
		UserStore: &refundStepUpUserStore{owners: []domain.User{*user, {ID: uuid.New()}}, user: user},
		Auth:      &refundStepUpAuthService{validOTP: "123456"},
	}}
	for _, otp := range []string{"", "123456"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/refund", nil)
		mode, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, StepUpAuthInput{OTP: otp})
		if otp == "" && ok {
			t.Fatal("multiple owners incorrectly substituted for approval")
		}
		if otp != "" && (!ok || mode != "solo_owner_reauth") {
			t.Fatalf("valid step-up rejected: %s %v", mode, ok)
		}
	}
}

func TestFinancialAuthorizationFailsClosedOnOwnerLookupError(t *testing.T) {
	h := &Handlers{Deps: Deps{UserStore: &authorizationUserStore{lookupErr: errors.New("database timeout")}}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/refund", nil)
	if _, ok := h.verifyDualControlOrStepUp(c, uuid.New(), uuid.New(), nil, StepUpAuthInput{}); ok || w.Code != http.StatusServiceUnavailable {
		t.Fatalf("lookup error must deny with 503, got %d", w.Code)
	}
}

func TestFinancialAuthorizationRejectsUnverifiedEmailFallback(t *testing.T) {
	uid := uuid.New()
	user := &domain.User{ID: uid, Email: "owner@example.test"}
	h := &Handlers{Deps: Deps{
		UserStore: &refundStepUpUserStore{user: user},
		Auth:      &authorizationFirebaseService{identity: auth.FirebaseIdentity{UID: "unlinked-user", Email: user.Email, EmailVerified: false}},
	}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/refund", nil)
	if _, ok := h.verifyDualControlOrStepUp(c, uuid.New(), uid, nil, StepUpAuthInput{FirebaseIDToken: "token"}); ok {
		t.Fatal("unverified email must not authorize another account's action")
	}
}

func TestVerifyDualControlOrStepUp_FailsClosedWhenUnconfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Completely unconfigured: UserStore == nil, Auth == nil
	h := &Handlers{
		Deps: Deps{},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	pid := uuid.New()
	uid := uuid.New()

	mode, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, StepUpAuthInput{})
	if ok {
		t.Fatalf("expected verifyDualControlOrStepUp to fail closed when unconfigured, but returned ok=true, mode=%q", mode)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500 InternalServerError, got %d", w.Code)
	}
}

func TestVerifyDualControlOrStepUp_DualControlWithMultipleOwners(t *testing.T) {
	gin.SetMode(gin.TestMode)
	makerID := uuid.New()
	checkerID := uuid.New()
	propID := uuid.New()

	h := &Handlers{
		Deps: Deps{
			UserStore: &mockDualControlUserStore{
				owners: []domain.User{
					{ID: makerID},
					{ID: checkerID},
				},
			},
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	// Checker approving maker's action
	mode, ok := h.verifyDualControlOrStepUp(c, propID, checkerID, &makerID, StepUpAuthInput{})
	if !ok || mode != "dual_control" {
		t.Fatalf("expected mode dual_control and ok=true, got ok=%v, mode=%q", ok, mode)
	}
}

func TestVerifyDualControlOrStepUp_MakerCannotBeChecker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	makerID := uuid.New()
	checkerID := uuid.New()
	propID := uuid.New()

	h := &Handlers{
		Deps: Deps{
			UserStore: &mockDualControlUserStore{
				owners: []domain.User{
					{ID: makerID},
					{ID: checkerID},
				},
			},
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	// Maker attempting to approve their own action
	mode, ok := h.verifyDualControlOrStepUp(c, propID, makerID, &makerID, StepUpAuthInput{})
	if ok {
		t.Fatalf("expected maker approval to be rejected, got ok=true, mode=%q", mode)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected status 403 Forbidden, got %d", w.Code)
	}
}
