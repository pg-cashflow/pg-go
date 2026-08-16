package cashfree

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	if meta["payment_methods"] != "upi" {
		t.Fatalf("payment_methods=%v", meta["payment_methods"])
	}
}

func TestCreateUPIOrderRequiresPhone(t *testing.T) {
	c := NewClient(Config{AppID: "id", SecretKey: "secret", Env: "sandbox"})
	if _, _, err := c.CreateUPIOrder(context.Background(), "pg-1", 100, "", "PG-1"); err == nil {
		t.Fatal("expected error when customer phone is empty")
	}
}
