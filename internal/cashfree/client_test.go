package cashfree

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyWebhookHMAC(t *testing.T) {
	secret := "test-secret"
	ts := "1746426425612"
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK"}`
	sig := hmacB64(secret, ts+body)
	if !VerifyWebhookHMAC(secret, ts, body, sig) {
		t.Fatal("expected match")
	}
	if VerifyWebhookHMAC(secret, ts, body+" ", sig) {
		t.Fatal("tampered body must fail")
	}
	if VerifyWebhookHMAC(secret, ts, body, "") {
		t.Fatal("empty signature must fail")
	}
	if VerifyWebhookHMAC("", ts, body, hmacB64("", ts+body)) {
		t.Fatal("empty secret must fail closed")
	}
	if VerifyWebhookHMAC("", ts, body, "") {
		t.Fatal("empty secret and empty signature must fail closed")
	}
}

func hmacB64(secret, msg string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestParseSuccessWebhook(t *testing.T) {
	raw := []byte(`{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"pg-ABC123-1"},"payment":{"cf_payment_id":"12345","payment_status":"SUCCESS","payment_amount":170.00,"bank_reference":"UTR99"}}}`)
	evt, ok, err := ParseSuccessWebhook(raw)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if evt.OrderID != "pg-ABC123-1" || evt.AmountPaise != 17000 || evt.TxnID() != "UTR99" || evt.CFPaymentID != "12345" {
		t.Fatalf("%+v", evt)
	}
}

func TestParseSuccessWebhookIgnoresFailed(t *testing.T) {
	raw := []byte(`{"type":"PAYMENT_FAILED_WEBHOOK","data":{"order":{"order_id":"x"}}}`)
	_, ok, err := ParseSuccessWebhook(raw)
	if err != nil || ok {
		t.Fatalf("failed webhooks must not settle ok=%v err=%v", ok, err)
	}
}

func TestCreateUPIOrderUPIOnlyExactRupees(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders" {
			t.Fatalf("path %s", r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payment_session_id":"sess_abc"}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(Config{AppID: "id", SecretKey: "secret", Env: "sandbox"})
	c.baseOverride = srv.URL
	session, _, err := c.CreateUPIOrder(context.Background(), "pg-ABC123-1", 1500000, "9999999999", "PG-ABC123")
	if err != nil {
		t.Fatal(err)
	}
	if session != "sess_abc" {
		t.Fatalf("session=%s", session)
	}
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatal(err)
	}
	amt, _ := body["order_amount"].(float64)
	if amt != 15000 {
		t.Fatalf("order_amount=%v want 15000 rupees (no surcharge)", body["order_amount"])
	}
	if _, ok := body["order_surcharge"]; ok {
		t.Fatal("must not send surcharge")
	}
	meta, _ := body["order_meta"].(map[string]any)
	if expiry, ok := body["order_expiry_time"].(string); !ok || expiry == "" {
		t.Fatal("order expiry must be a top-level request field")
	}
	if _, nested := meta["order_expiry_time"]; nested {
		t.Fatal("order expiry must not be nested in order_meta")
	}
	if meta["payment_methods"] != nil {
		t.Fatalf("payment_methods=%v, want all methods enabled (nil)", meta["payment_methods"])
	}
	cust, _ := body["customer_details"].(map[string]any)
	if cust["customer_id"] != "cust_9999999999" {
		t.Fatalf("customer_id=%v want cust_9999999999", cust["customer_id"])
	}
}

func TestCreateUPIOrderRequiresPhone(t *testing.T) {
	c := NewClient(Config{AppID: "id", SecretKey: "secret", Env: "sandbox"})
	if _, _, err := c.CreateUPIOrder(context.Background(), "pg-1", 100, "", "PG-1"); err == nil {
		t.Fatal("expected error when customer phone is empty")
	}
}

func TestCashfreePGClientDoesNotFollowRedirects(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		if got := r.Header.Get("x-client-secret"); got != "" {
			t.Errorf("client secret leaked to redirect destination: %q", got)
		}
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-client-secret") != "secret" {
			t.Errorf("expected credential at Cashfree origin")
		}
		http.Redirect(w, r, target.URL+"/steal", http.StatusFound)
	}))
	defer source.Close()

	c := NewClient(Config{AppID: "id", SecretKey: "secret", Env: "sandbox"}).WithBaseURL(source.URL)
	_, _, err := c.CreateUPIOrder(context.Background(), "pg-redirect-test", 1000, "9999999999", "test")
	if err == nil {
		t.Fatal("expected redirect response to be treated as an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 APIError, got %T %v", err, err)
	}
	if targetHits != 0 {
		t.Fatalf("redirect target was contacted %d times", targetHits)
	}
}

func TestCreateUPIOrderWithReturnURL(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"payment_session_id": "sess_return"})
	}))
	defer srv.Close()

	c := NewClient(Config{
		AppID:     "id",
		SecretKey: "secret",
		Env:       "sandbox",
		ReturnURL: "https://example.com/tenant/pay-return?order_id={order_id}",
	}).WithBaseURL(srv.URL)

	sess, _, err := c.CreateUPIOrder(context.Background(), "order_ret_1", 10000, "9876543210", "Note")
	if err != nil {
		t.Fatalf("CreateUPIOrder failed: %v", err)
	}
	if sess != "sess_return" {
		t.Fatalf("expected sess_return, got %s", sess)
	}

	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatal(err)
	}
	meta, _ := body["order_meta"].(map[string]any)
	if meta["return_url"] != "https://example.com/tenant/pay-return?order_id={order_id}" {
		t.Fatalf("expected return_url to be configured, got %v", meta["return_url"])
	}
}

func TestCreateRefund(t *testing.T) {
	var gotBody []byte
	var gotIdempotency string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders/order_123/refunds" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotIdempotency = r.Header.Get("x-idempotency-key")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"cf_refund_id": "999888",
			"refund_id": "rf_test_123",
			"order_id": "order_123",
			"cf_payment_id": "777666",
			"refund_status": "SUCCESS",
			"refund_amount": 5500.00,
			"refund_type": "PAYMENT_REVERSAL",
			"refund_reason": "Tenant moved out"
		}`))
	}))
	defer srv.Close()

	c := NewClient(Config{AppID: "app_1", SecretKey: "sec_1", Env: "sandbox"})
	c.baseOverride = srv.URL

	ref, err := c.CreateRefund(context.Background(), "order_123", "rf_test_123", 550000, "Tenant moved out", "idemp_test_key")
	if err != nil {
		t.Fatalf("CreateRefund failed: %v", err)
	}
	if gotIdempotency != "idemp_test_key" {
		t.Fatalf("expected idempotency header 'idemp_test_key', got: %s", gotIdempotency)
	}
	var reqBody CreateRefundRequest
	if err := json.Unmarshal(gotBody, &reqBody); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if reqBody.RefundAmount.String() != "5500.00" || reqBody.RefundID != "rf_test_123" || reqBody.RefundNote != "Tenant moved out" || reqBody.RefundSpeed != "STANDARD" {
		t.Fatalf("unexpected request body: %+v", reqBody)
	}
	if ref.CFRefundID != "999888" || ref.AmountPaise != 550000 || ref.RefundStatus != "SUCCESS" {
		t.Fatalf("unexpected refund details: %+v", ref)
	}

	// Test configurable refund speed (INSTANT)
	cInstant := NewClient(Config{AppID: "app_1", SecretKey: "sec_1", Env: "sandbox", DefaultRefundSpeed: "INSTANT"})
	cInstant.baseOverride = srv.URL
	_, err = cInstant.CreateRefund(context.Background(), "order_123", "rf_test_456", 550000, "Instant refund", "idemp_instant")
	if err != nil {
		t.Fatalf("CreateRefund with INSTANT failed: %v", err)
	}
	var instantBody CreateRefundRequest
	if err := json.Unmarshal(gotBody, &instantBody); err != nil {
		t.Fatalf("unmarshal instant body: %v", err)
	}
	if instantBody.RefundSpeed != "INSTANT" {
		t.Fatalf("expected refund_speed 'INSTANT', got: %s", instantBody.RefundSpeed)
	}
}

func TestClient_WithBaseURL(t *testing.T) {
	var requestedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payment_session_id":"sess_override_123"}`))
	}))
	defer srv.Close()

	c := NewClient(Config{AppID: "app_test", SecretKey: "sec_test", Env: "sandbox"})
	c.WithBaseURL(srv.URL)

	if c.endpoint() != srv.URL {
		t.Fatalf("expected endpoint %s, got %s", srv.URL, c.endpoint())
	}

	sessionID, _, err := c.CreateUPIOrder(context.Background(), "order_override_1", 10000, "9876543210", "Test Override")
	if err != nil {
		t.Fatalf("CreateUPIOrder failed: %v", err)
	}
	if sessionID != "sess_override_123" {
		t.Fatalf("expected session ID sess_override_123, got %s", sessionID)
	}
	if requestedPath != "/orders" {
		t.Fatalf("expected request to /orders, got %s", requestedPath)
	}
}

func TestCancelRefund(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotContentType string
	var gotAppID string
	var gotSecret string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotAppID = r.Header.Get("x-client-id")
		gotSecret = r.Header.Get("x-client-secret")
		gotBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"cf_refund_id": "888777",
			"refund_id": "rf_cancel_123",
			"order_id": "order_cancel_123",
			"cf_payment_id": "999000",
			"refund_status": "CANCELLED",
			"refund_amount": 2500.00,
			"refund_type": "PAYMENT_REVERSAL",
			"refund_reason": "Requested by owner"
		}`))
	}))
	defer srv.Close()

	c := NewClient(Config{AppID: "test_app_id", SecretKey: "test_secret_key", Env: "sandbox"})
	c.baseOverride = srv.URL

	ref, err := c.CancelRefund(context.Background(), "order_cancel_123", "rf_cancel_123", "Requested by owner")
	if err != nil {
		t.Fatalf("CancelRefund failed: %v", err)
	}

	if gotMethod != http.MethodPut {
		t.Fatalf("expected HTTP PUT, got: %s", gotMethod)
	}
	if gotPath != "/orders/order_cancel_123/refunds/rf_cancel_123" {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("expected Content-Type application/json, got: %s", gotContentType)
	}
	if gotAppID != "test_app_id" || gotSecret != "test_secret_key" {
		t.Fatalf("unexpected credentials: app_id=%s secret=%s", gotAppID, gotSecret)
	}

	var reqBody CancelRefundRequest
	if err := json.Unmarshal(gotBody, &reqBody); err != nil {
		t.Fatalf("unmarshal cancel request body: %v", err)
	}
	if reqBody.RefundStatus != "CANCELLED" || reqBody.Remarks != "Requested by owner" {
		t.Fatalf("unexpected cancel request body: %+v", reqBody)
	}

	if ref.CFRefundID != "888777" || ref.RefundStatus != "CANCELLED" || ref.AmountPaise != 250000 {
		t.Fatalf("unexpected refund response details: %+v", ref)
	}
}

func TestGetOrder(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-client-id") != "app_1" || r.Header.Get("x-client-secret") != "sec_1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/orders/order_found" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"cf_order_id": "2149460581",
				"order_id": "order_found",
				"order_status": "ACTIVE",
				"payment_session_id": "session_abc123",
				"order_amount": 150.00,
				"order_expiry_time": "2026-12-01T12:00:00Z"
			}`))
			return
		}
		if r.URL.Path == "/orders/order_missing" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message": "order not found", "code": "order_not_found"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := NewClient(Config{AppID: "app_1", SecretKey: "sec_1"}).WithBaseURL(ts.URL)

	ord, err := c.GetOrder(context.Background(), "order_found")
	if err != nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if ord.OrderID != "order_found" || ord.CFOrderID != "2149460581" || ord.OrderStatus != "ACTIVE" {
		t.Fatalf("unexpected order: %+v", ord)
	}
	if ord.PaymentSessionID != "session_abc123" || ord.OrderAmountPaise != 15000 {
		t.Fatalf("unexpected session or amount: %+v", ord)
	}
	if ord.ExpiresAt == nil || ord.ExpiresAt.IsZero() {
		t.Fatal("expected non-nil ExpiresAt")
	}

	_, notFoundErr := c.GetOrder(context.Background(), "order_missing")
	if notFoundErr == nil {
		t.Fatal("expected error for missing order")
	}
	var apiErr *APIError
	if !errors.As(notFoundErr, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 APIError, got: %v", notFoundErr)
	}
}

func TestFetchSuccessfulPayment_AmbiguousCaptures(t *testing.T) {
	t.Run("single_success", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"cf_payment_id": 1001, "payment_status": "FAILED", "payment_amount": 500.00},
				{"cf_payment_id": 1002, "payment_status": "SUCCESS", "payment_amount": 500.00, "bank_reference": "UTR1"}
			]`))
		}))
		defer ts.Close()

		c := NewClient(Config{AppID: "app_1", SecretKey: "sec_1"}).WithBaseURL(ts.URL)
		cfID, bankRef, amount, ok, err := c.FetchSuccessfulPayment(context.Background(), "ord_single")
		if err != nil || !ok {
			t.Fatalf("expected ok=true, err=nil; got ok=%v, err=%v", ok, err)
		}
		if cfID != "1002" || bankRef != "UTR1" || amount != 50000 {
			t.Fatalf("unexpected result: cfID=%s, bankRef=%s, amount=%d", cfID, bankRef, amount)
		}
	})

	t.Run("duplicate_same_cf_payment_id_deduped", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"cf_payment_id": 2001, "payment_status": "SUCCESS", "payment_amount": 300.00, "bank_reference": "UTR_DEDUP"},
				{"cf_payment_id": 2001, "payment_status": "SUCCESS", "payment_amount": 300.00, "bank_reference": "UTR_DEDUP"}
			]`))
		}))
		defer ts.Close()

		c := NewClient(Config{AppID: "app_1", SecretKey: "sec_1"}).WithBaseURL(ts.URL)
		cfID, _, amount, ok, err := c.FetchSuccessfulPayment(context.Background(), "ord_dup")
		if err != nil || !ok {
			t.Fatalf("expected dedup to succeed: ok=%v, err=%v", ok, err)
		}
		if cfID != "2001" || amount != 30000 {
			t.Fatalf("unexpected cfID=%s, amount=%d", cfID, amount)
		}
	})

	t.Run("multiple_distinct_successes_fails_closed", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"cf_payment_id": 3001, "payment_status": "SUCCESS", "payment_amount": 500.00, "bank_reference": "UTR_A"},
				{"cf_payment_id": 3002, "payment_status": "SUCCESS", "payment_amount": 500.00, "bank_reference": "UTR_B"}
			]`))
		}))
		defer ts.Close()

		c := NewClient(Config{AppID: "app_1", SecretKey: "sec_1"}).WithBaseURL(ts.URL)
		_, _, _, ok, err := c.FetchSuccessfulPayment(context.Background(), "ord_ambiguous")
		if err == nil {
			t.Fatalf("expected error for ambiguous multiple captures, got nil (ok=%v)", ok)
		}
		if ok {
			t.Fatalf("expected ok=false on ambiguous captures")
		}
	})

	t.Run("default_api_version_header_is_2026_01_01", func(t *testing.T) {
		var capturedVersion string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedVersion = r.Header.Get("x-api-version")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"payments":[]}`))
		}))
		defer ts.Close()

		c := NewClient(Config{AppID: "app_1", SecretKey: "sec_1"}).WithBaseURL(ts.URL)
		_, _, _, _, _ = c.FetchSuccessfulPayment(context.Background(), "ord_test_ver")
		if capturedVersion != "2026-01-01" {
			t.Fatalf("expected default x-api-version header to be 2026-01-01, got %q", capturedVersion)
		}
	})
}

func TestTerminateOrderRequiresReadAfterWriteAndReturnsRequestedState(t *testing.T) {
	patchCalls := 0
	getCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orders/order-terminate-1" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		if r.Header.Get("x-client-id") != "test-id" || r.Header.Get("x-client-secret") != "test-secret" {
			t.Fatal("Cashfree credentials were not attached to the request")
		}
		switch r.Method {
		case http.MethodPatch:
			patchCalls++
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["order_status"] != "TERMINATED" {
				t.Fatalf("termination body = %#v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"order_status":"TERMINATION_REQUESTED"}`))
		case http.MethodGet:
			getCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"order_id":"order-terminate-1","order_status":"TERMINATION_REQUESTED","order_amount":100.00,"payment_session_id":"sess_x"}`))
		default:
			t.Fatalf("unexpected method: %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)

	client := NewClient(Config{AppID: "test-id", SecretKey: "test-secret", Env: "sandbox"}).WithBaseURL(server.URL)
	order, err := client.TerminateOrder(context.Background(), "order-terminate-1")
	if err != nil {
		t.Fatalf("TerminateOrder returned error: %v", err)
	}
	if patchCalls != 1 || getCalls != 1 {
		t.Fatalf("PATCH calls=%d GET calls=%d; want one each", patchCalls, getCalls)
	}
	if order == nil || order.OrderStatus != "TERMINATION_REQUESTED" {
		t.Fatalf("order status = %#v; must preserve non-terminal requested state", order)
	}
}

func TestTerminateOrderReturnsProviderConfirmedTerminalState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"order_id":"order-terminate-2","order_status":"TERMINATED","order_amount":100.00}`))
	}))
	t.Cleanup(server.Close)

	client := NewClient(Config{AppID: "id", SecretKey: "secret", Env: "sandbox"}).WithBaseURL(server.URL)
	order, err := client.TerminateOrder(context.Background(), "order-terminate-2")
	if err != nil {
		t.Fatalf("TerminateOrder returned error: %v", err)
	}
	if order == nil || order.OrderStatus != "TERMINATED" {
		t.Fatalf("expected provider-confirmed TERMINATED state, got %#v", order)
	}
}

func TestCashfree_URLPathEscaping(t *testing.T) {
	recordedPaths := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordedPaths[r.URL.RawPath] = true
		if r.URL.RawPath == "" {
			recordedPaths[r.URL.Path] = true
		}
		w.Header().Set("Content-Type", "application/json")
		escapedPath := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodGet && escapedPath == "/orders/order%2Fspecial+id/payments":
			_, _ = w.Write([]byte(`{"payments":[{"cf_payment_id":"123","payment_status":"SUCCESS","payment_amount":"100.00"}]}`))
		case r.Method == http.MethodGet && escapedPath == "/orders/order%2Fspecial+id":
			_, _ = w.Write([]byte(`{"cf_order_id":"1","order_id":"order/special+id","order_status":"PAID","order_amount":"100.00"}`))
		case r.Method == http.MethodGet && escapedPath == "/orders/order%2Fspecial+id/refunds/ref%2Fspecial+id":
			_, _ = w.Write([]byte(`{"cf_refund_id":"r1","refund_id":"ref/special+id","order_id":"order/special+id","refund_status":"SUCCESS","refund_amount":"100.00"}`))
		case r.Method == http.MethodPut && escapedPath == "/orders/order%2Fspecial+id/refunds/ref%2Fspecial+id":
			_, _ = w.Write([]byte(`{"cf_refund_id":"r1","refund_id":"ref/special+id","order_id":"order/special+id","refund_status":"CANCELLED","refund_amount":"100.00"}`))
		case r.Method == http.MethodPost && escapedPath == "/orders/order%2Fspecial+id/refunds":
			_, _ = w.Write([]byte(`{"cf_refund_id":"r1","refund_id":"ref/special+id","order_id":"order/special+id","refund_status":"SUCCESS","refund_amount":"100.00"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client := NewClient(Config{AppID: "id", SecretKey: "secret", Env: "sandbox"}).WithBaseURL(server.URL)
	ctx := context.Background()
	specialOrderID := "order/special+id"
	specialRefundID := "ref/special+id"

	// 1. FetchSuccessfulPayment
	cfID, _, _, ok, err := client.FetchSuccessfulPayment(ctx, specialOrderID)
	if err != nil || !ok || cfID != "123" {
		t.Fatalf("FetchSuccessfulPayment failed: ok=%v, cfID=%s, err=%v", ok, cfID, err)
	}

	// 2. GetOrder
	order, err := client.GetOrder(ctx, specialOrderID)
	if err != nil || order.OrderStatus != "PAID" {
		t.Fatalf("GetOrder failed: order=%+v, err=%v", order, err)
	}

	// 3. FetchRefundStatus
	refund, err := client.FetchRefundStatus(ctx, specialOrderID, specialRefundID)
	if err != nil || refund.RefundStatus != "SUCCESS" {
		t.Fatalf("FetchRefundStatus failed: refund=%+v, err=%v", refund, err)
	}

	// 4. CancelRefund
	cancelled, err := client.CancelRefund(ctx, specialOrderID, specialRefundID, "test remarks")
	if err != nil || cancelled.RefundStatus != "CANCELLED" {
		t.Fatalf("CancelRefund failed: cancelled=%+v, err=%v", cancelled, err)
	}

	// 5. CreateRefund
	created, err := client.CreateRefund(ctx, specialOrderID, specialRefundID, 10000, "test refund", "idem-1")
	if err != nil || created.RefundStatus != "SUCCESS" {
		t.Fatalf("CreateRefund failed: created=%+v, err=%v", created, err)
	}
}

func TestCreatePaymentOrder_MinAmountPhoneAndNotifyURL(t *testing.T) {
	var called int
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payment_session_id":"sess_100"}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(Config{
		AppID:     "id",
		SecretKey: "secret",
		Env:       "sandbox",
		ReturnURL: "https://app.example.com/payment/return",
		NotifyURL: "https://api.example.com/webhooks/cashfree",
	}).WithBaseURL(srv.URL)

	// 1. Sub-₹1 amount (< 100 paise) must fail before hitting Cashfree
	if _, _, err := c.CreatePaymentOrder(context.Background(), "ord_99", 99, "+919876543210", "Note"); err == nil {
		t.Fatal("expected error for amount < 100 paise")
	}
	if called != 0 {
		t.Fatalf("expected 0 HTTP calls for amount < 100 paise, got %d", called)
	}

	// 2. Boundary 100 paise (₹1) + Indian E.164 normalization -> 10 digits + notify_url
	sess, _, err := c.CreatePaymentOrder(context.Background(), "ord_100", 100, "+919876543210", "Note")
	if err != nil || sess != "sess_100" {
		t.Fatalf("CreatePaymentOrder 100 paise failed: sess=%s, err=%v", sess, err)
	}
	cust, _ := gotBody["customer_details"].(map[string]any)
	if cust["customer_phone"] != "9876543210" {
		t.Fatalf("expected 10-digit normalized phone 9876543210, got %v", cust["customer_phone"])
	}
	meta, _ := gotBody["order_meta"].(map[string]any)
	if meta["notify_url"] != "https://api.example.com/webhooks/cashfree" {
		t.Fatalf("expected notify_url in order_meta, got %v", meta["notify_url"])
	}

	// 3. Non-Indian E.164 must NOT be blindly stripped
	_, _, err = c.CreatePaymentOrder(context.Background(), "ord_intl", 100, "+14155552671", "Note")
	if err != nil {
		t.Fatalf("international E.164 failed: %v", err)
	}
	cust, _ = gotBody["customer_details"].(map[string]any)
	if cust["customer_phone"] != "+14155552671" {
		t.Fatalf("expected international E.164 preserved, got %v", cust["customer_phone"])
	}
}

func TestFetchOrderPayments_AllAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payments":[
			{"cf_payment_id":"101","payment_status":"FAILED","payment_amount":500.00},
			{"cf_payment_id":"102","payment_status":"PENDING","payment_amount":500.00}
		]}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(Config{AppID: "id", SecretKey: "secret", Env: "sandbox"}).WithBaseURL(srv.URL)
	attempts, err := c.FetchOrderPayments(context.Background(), "ord_attempts_1")
	if err != nil {
		t.Fatalf("FetchOrderPayments failed: %v", err)
	}
	if len(attempts) != 2 || attempts[0].PaymentStatus != "FAILED" || attempts[1].PaymentStatus != "PENDING" {
		t.Fatalf("unexpected attempts: %+v", attempts)
	}
}
