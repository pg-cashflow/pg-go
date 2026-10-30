package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func signHMACSHA256(secret, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// TestWebhookReplayParity_Payments verifies payments webhook rejects stale timestamps even when tolerance is 0 or unconfigured.
func TestWebhookReplayParity_Payments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-pay-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"ord_1"},"payment":{"cf_payment_id":123,"payment_status":"SUCCESS","payment_amount":100}}}`

	for _, tc := range []struct {
		name      string
		tolerance int
	}{
		{name: "configured tolerance 300", tolerance: 300},
		{name: "zero tolerance defaults to 300", tolerance: 0},
		{name: "negative tolerance defaults to 300", tolerance: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{Deps: Deps{
				CashfreeSecret:      secret,
				WebhookToleranceSec: tc.tolerance,
			}}
			r := gin.New()
			r.POST("/webhooks/cashfree", h.CashfreeWebhook)

			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewBufferString(body))
			req.Header.Set("x-webhook-timestamp", oldTS)
			req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized for stale payments webhook (tolerance=%d), got %d", tc.tolerance, w.Code)
			}
		})
	}
}

// TestWebhookReplayParity_Payouts verifies payouts webhook rejects stale timestamps even when tolerance is 0 or unconfigured.
func TestWebhookReplayParity_Payouts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-payout-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"TRANSFER_SUCCESS","data":{"transfer_id":"tr_1","status":"SUCCESS"}}`

	for _, tc := range []struct {
		name      string
		tolerance int
	}{
		{name: "configured tolerance 300", tolerance: 300},
		{name: "zero tolerance defaults to 300", tolerance: 0},
		{name: "negative tolerance defaults to 300", tolerance: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{Deps: Deps{
				CashfreePayoutWebhookSecret: secret,
				WebhookToleranceSec:         tc.tolerance,
			}}
			r := gin.New()
			r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/payouts", bytes.NewBufferString(body))
			req.Header.Set("x-webhook-timestamp", oldTS)
			req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized for stale payouts webhook (tolerance=%d), got %d", tc.tolerance, w.Code)
			}
		})
	}
}

// TestWebhookReplayParity_Settlement verifies settlement webhook rejects stale timestamps even when tolerance is 0 or unconfigured.
func TestWebhookReplayParity_Settlement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "whsec-settle-123"
	oldTS := fmt.Sprintf("%d", time.Now().Add(-600*time.Second).Unix())
	body := `{"type":"SETTLEMENT_SUCCESS","data":{"settlement_id":"set_1"}}`

	for _, tc := range []struct {
		name      string
		tolerance int
	}{
		{name: "configured tolerance 300", tolerance: 300},
		{name: "zero tolerance defaults to 300", tolerance: 0},
		{name: "negative tolerance defaults to 300", tolerance: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handlers{Deps: Deps{
				CashfreeSecret:      secret,
				WebhookToleranceSec: tc.tolerance,
			}}
			r := gin.New()
			r.POST("/webhooks/cashfree/settlements", h.CashfreeSettlementWebhook)

			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/settlements", bytes.NewBufferString(body))
			req.Header.Set("x-webhook-timestamp", oldTS)
			req.Header.Set("x-webhook-signature", signHMACSHA256(secret, oldTS+body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized for stale settlement webhook (tolerance=%d), got %d", tc.tolerance, w.Code)
			}
		})
	}
}
