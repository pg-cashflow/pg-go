package api

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"net/http"
	"net/http/httptest"
	"testing"
)

type refundOTPDispatch struct {
	stubPayoutAuthService
	purpose string
	calls   int
}

func (s *refundOTPDispatch) RequestStepUpOTPWithChannel(_ context.Context, _, purpose string, _ *uuid.UUID) (string, string, error) {
	s.calls++
	s.purpose = purpose
	return "sms", "+919000000001", nil
}
func TestRefundOTPIsScopedToPaymentAndProperty(t *testing.T) {
	pid, uid, paymentID, tenantID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	repo := newMockRefundPaymentRepo()
	repo.payments[paymentID] = &domain.Payment{ID: paymentID, TenantID: tenantID}
	for _, sameProperty := range []bool{true, false} {
		tenantPID := pid
		if !sameProperty {
			tenantPID = uuid.New()
		}
		dispatch := &refundOTPDispatch{}
		h := &Handlers{Deps: Deps{
			GatewayPaymentRepo: repo,
			TenantStore:        &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: tenantPID}},
			UserStore:          &stubPayoutUserStore{user: &domain.User{ID: uid, Phone: "+919000000001"}},
			Auth:               dispatch,
		}}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/refund/request-otp", nil)
		c.Params = gin.Params{{Key: "id", Value: paymentID.String()}}
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: uid, PropertyID: &pid, Role: domain.RoleOwner})
		h.RequestPaymentRefundOTP(c)
		if sameProperty {
			if w.Code != http.StatusOK || dispatch.purpose != paymentRefundPurpose(paymentID) || len(dispatch.purpose) > 32 {
				t.Fatalf("OTP not bound to payment: %d %s", w.Code, dispatch.purpose)
			}
			if dispatch.purpose == paymentRefundPurpose(uuid.New()) {
				t.Fatal("different payments shared a purpose")
			}
		} else if w.Code != http.StatusNotFound || dispatch.calls != 0 {
			t.Fatalf("cross-property OTP sent: %d", w.Code)
		}
	}
}
