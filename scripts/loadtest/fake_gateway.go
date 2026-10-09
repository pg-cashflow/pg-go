package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Server configuration flags.
var (
	port        = flag.Int("port", 8081, "Port for fake Cashfree gateway server")
	targetURL   = flag.String("target", "http://localhost:8080", "Target backend server URL")
	whSecret    = flag.String("secret", "test_wh_secret_key", "Cashfree webhook signing secret")
	latencyMs   = flag.Int("latency-ms", 0, "Simulated artificial network latency in ms")
	errorRate   = flag.Float64("error-rate", 0.0, "Fraction of order creations to fail (0.0 to 1.0)")
	webhookPath = flag.String("webhook-path", "/webhooks/cashfree", "Webhook path on target backend")
)

var (
	totalOrdersCreated uint64
	totalWebhooksSent  uint64
	totalWebhooksAcked uint64
)

// SignCashfreeWebhook computes Base64(HMAC-SHA256(timestamp + rawBody, secret)).
func SignCashfreeWebhook(secret, ts, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// CashfreeOrderRequest represents incoming order creation payload from backend.
type CashfreeOrderRequest struct {
	OrderID       string  `json:"order_id"`
	OrderAmount   float64 `json:"order_amount"`
	OrderCurrency string  `json:"order_currency"`
	CustomerInfo  struct {
		CustomerID    string `json:"customer_id"`
		CustomerPhone string `json:"customer_phone"`
		CustomerEmail string `json:"customer_email"`
	} `json:"customer_details"`
	OrderNote string `json:"order_note"`
}

// CashfreeOrderResponse represents the response expected by backend.
type CashfreeOrderResponse struct {
	CFOrderID        int64   `json:"cf_order_id"`
	OrderID          string  `json:"order_id"`
	OrderStatus      string  `json:"order_status"`
	PaymentSessionID string  `json:"payment_session_id"`
	OrderAmount      float64 `json:"order_amount"`
	OrderCurrency    string  `json:"order_currency"`
}

// DispatchWebhookRequest instructs fake gateway to send a webhook to target.
type DispatchWebhookRequest struct {
	OrderID        string  `json:"order_id"`
	CFPaymentID    int64   `json:"cf_payment_id"`
	PaymentAmount  float64 `json:"payment_amount"`
	BankReference  string  `json:"bank_reference"`
	DuplicateCount int     `json:"duplicate_count"` // 1 = normal, >1 = concurrent stampede
	DelayMs        int     `json:"delay_ms"`
}

func handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if *latencyMs > 0 {
		time.Sleep(time.Duration(*latencyMs) * time.Millisecond)
	}

	if *errorRate > 0 && rand.Float64() < *errorRate {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "simulated gateway error"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req CashfreeOrderRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	orderNum := atomic.AddUint64(&totalOrdersCreated, 1)
	cfID := int64(10000000 + orderNum)

	resp := CashfreeOrderResponse{
		CFOrderID:        cfID,
		OrderID:          req.OrderID,
		OrderStatus:      "ACTIVE",
		PaymentSessionID: fmt.Sprintf("session_%s_%d", req.OrderID, cfID),
		OrderAmount:      req.OrderAmount,
		OrderCurrency:    "INR",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func handleDispatchWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DispatchWebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}

	if req.CFPaymentID == 0 {
		req.CFPaymentID = int64(20000000 + rand.Intn(90000000))
	}
	if req.BankReference == "" {
		req.BankReference = fmt.Sprintf("UTR%08d", rand.Intn(100000000))
	}
	if req.DuplicateCount <= 0 {
		req.DuplicateCount = 1
	}

	payload := map[string]interface{}{
		"type":       "PAYMENT_SUCCESS_WEBHOOK",
		"event_time": time.Now().UTC().Format(time.RFC3339),
		"data": map[string]interface{}{
			"order": map[string]interface{}{
				"order_id":       req.OrderID,
				"order_amount":   req.PaymentAmount,
				"order_currency": "INR",
			},
			"payment": map[string]interface{}{
				"cf_payment_id":    req.CFPaymentID,
				"payment_amount":   req.PaymentAmount,
				"payment_currency": "INR",
				"payment_status":   "SUCCESS",
				"bank_reference":   req.BankReference,
			},
		},
	}

	rawJSON, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "marshal error", http.StatusInternalServerError)
		return
	}

	go func() {
		if req.DelayMs > 0 {
			time.Sleep(time.Duration(req.DelayMs) * time.Millisecond)
		}

		var wg sync.WaitGroup
		destURL := fmt.Sprintf("%s%s", *targetURL, *webhookPath)

		for i := 0; i < req.DuplicateCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				atomic.AddUint64(&totalWebhooksSent, 1)

				ts := fmt.Sprintf("%d", time.Now().UTC().Unix())
				sig := SignCashfreeWebhook(*whSecret, ts, string(rawJSON))

				httpReq, err := http.NewRequest(http.MethodPost, destURL, bytes.NewReader(rawJSON))
				if err != nil {
					log.Printf("[FakeGateway] Webhook request build failed: %v", err)
					return
				}
				httpReq.Header.Set("Content-Type", "application/json")
				httpReq.Header.Set("x-webhook-timestamp", ts)
				httpReq.Header.Set("x-webhook-signature", sig)

				client := &http.Client{Timeout: 5 * time.Second}
				resp, err := client.Do(httpReq)
				if err != nil {
					log.Printf("[FakeGateway] Webhook post error: %v", err)
					return
				}
				defer resp.Body.Close()

				if resp.StatusCode == http.StatusOK {
					atomic.AddUint64(&totalWebhooksAcked, 1)
				} else {
					log.Printf("[FakeGateway] Webhook returned status %d", resp.StatusCode)
				}
			}()
		}
		wg.Wait()
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "dispatched",
		"order_id":        req.OrderID,
		"cf_payment_id":   req.CFPaymentID,
		"duplicate_count": req.DuplicateCount,
	})
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	stats := map[string]interface{}{
		"status":          "ok",
		"orders_created":  atomic.LoadUint64(&totalOrdersCreated),
		"webhooks_sent":   atomic.LoadUint64(&totalWebhooksSent),
		"webhooks_acked":  atomic.LoadUint64(&totalWebhooksAcked),
		"target_url":      *targetURL,
		"target_path":     *webhookPath,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

func main() {
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/pg/orders", handleCreateOrder)
	mux.HandleFunc("/orders", handleCreateOrder)
	mux.HandleFunc("/mock/dispatch-webhook", handleDispatchWebhook)
	mux.HandleFunc("/stats", handleStats)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("==================================================")
	log.Printf("  Fake Cashfree Gateway Server started on %s", addr)
	log.Printf("  Target URL:    %s%s", *targetURL, *webhookPath)
	log.Printf("  HMAC Secret:   %s", *whSecret)
	log.Printf("==================================================")

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Fake gateway server failed: %v", err)
	}
}
