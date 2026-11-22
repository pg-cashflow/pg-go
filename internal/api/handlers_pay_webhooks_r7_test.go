package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// R7: When IntentStore is unavailable, payment-success webhook must return 503 rather than false 200 OK.
func TestPaymentSuccessWebhook_NilIntentStore_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewReader([]byte("{}")))

	h := &Handlers{
		Deps: Deps{
			IntentStore: nil,
		},
	}

	succ := cashfree.SuccessWebhook{
		OrderID:         "order_test_r7",
		CFPaymentID:     "cf_pay_123",
		PaymentStatus:   "SUCCESS",
		PaymentCurrency: "INR",
		AmountPaise:     100000,
	}
	evtRecord := &domain.WebhookEvent{ID: uuid.New()}

	h.handlePaymentSuccessWebhook(c, succ, evtRecord)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("R7: expected 503 Service Unavailable when IntentStore is nil, got %d", w.Code)
	}
}
