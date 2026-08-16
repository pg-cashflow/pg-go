package cashfree

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Config struct {
	AppID     string
	SecretKey string
	Env       string // sandbox | production
}

func (c Config) Enabled() bool {
	return strings.TrimSpace(c.AppID) != "" && strings.TrimSpace(c.SecretKey) != ""
}

func (c Config) baseURL() string {
	if strings.EqualFold(c.Env, "production") {
		return "https://api.cashfree.com/pg"
	}
	return "https://sandbox.cashfree.com/pg"
}

type Client struct {
	cfg          Config
	http         *http.Client
	now          func() time.Time
	baseOverride string
}

func (c *Client) endpoint() string {
	if c.baseOverride != "" {
		return strings.TrimRight(c.baseOverride, "/")
	}
	return c.cfg.baseURL()
}

func NewClient(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}, now: time.Now}
}

type createOrderBody struct {
	OrderID       string         `json:"order_id"`
	OrderAmount   float64        `json:"order_amount"`
	OrderCurrency string         `json:"order_currency"`
	Customer      map[string]any `json:"customer_details"`
	OrderMeta     map[string]any `json:"order_meta"`
	OrderNote     string         `json:"order_note,omitempty"`
}

type createOrderResp struct {
	PaymentSessionID string `json:"payment_session_id"`
}

func (c *Client) CreateUPIOrder(ctx context.Context, orderID string, amountPaise int, customerPhone, note string) (string, *time.Time, error) {
	if !c.cfg.Enabled() {
		return "", nil, fmt.Errorf("cashfree: not configured")
	}
	if customerPhone == "" {
		return "", nil, fmt.Errorf("cashfree: customer phone required")
	}
	body := createOrderBody{
		OrderID:       orderID,
		OrderAmount:   float64(amountPaise) / 100.0,
		OrderCurrency: "INR",
		Customer:      map[string]any{"customer_id": orderID, "customer_phone": customerPhone},
		OrderMeta:     map[string]any{"payment_methods": "upi"},
		OrderNote:     note,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint()+"/orders", bytes.NewReader(raw))
	if err != nil {
		return "", nil, err
	}
	c.sign(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("cashfree create order: %s %s", resp.Status, string(b))
	}
	var out createOrderResp
	if err := json.Unmarshal(b, &out); err != nil {
		return "", nil, err
	}
	exp := c.now().Add(24 * time.Hour)
	return out.PaymentSessionID, &exp, nil
}

type fetchPaymentsResp struct {
	Payments []struct {
		CFPaymentID   json.Number `json:"cf_payment_id"`
		PaymentStatus string      `json:"payment_status"`
		PaymentAmount float64     `json:"payment_amount"`
		BankReference string      `json:"bank_reference"`
	} `json:"payments"`
}

func (c *Client) FetchSuccessfulPayment(ctx context.Context, orderID string) (cfPaymentID, bankRef string, amountPaise int, ok bool, err error) {
	if !c.cfg.Enabled() {
		return "", "", 0, false, fmt.Errorf("cashfree: not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint()+"/orders/"+orderID+"/payments", nil)
	if err != nil {
		return "", "", 0, false, err
	}
	c.sign(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", 0, false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", "", 0, false, fmt.Errorf("cashfree fetch: %s %s", resp.Status, string(b))
	}
	var out fetchPaymentsResp
	if err := json.Unmarshal(b, &out); err != nil {
		// some responses are a bare array
		var arr []struct {
			CFPaymentID   json.Number `json:"cf_payment_id"`
			PaymentStatus string      `json:"payment_status"`
			PaymentAmount float64     `json:"payment_amount"`
			BankReference string      `json:"bank_reference"`
		}
		if err2 := json.Unmarshal(b, &arr); err2 != nil {
			return "", "", 0, false, err
		}
		out.Payments = arr
	}
	for _, p := range out.Payments {
		if strings.EqualFold(p.PaymentStatus, "SUCCESS") {
			return p.CFPaymentID.String(), p.BankReference, int(p.PaymentAmount*100 + 0.5), true, nil
		}
	}
	return "", "", 0, false, nil
}

func (c *Client) sign(req *http.Request) {
	req.Header.Set("x-client-id", c.cfg.AppID)
	req.Header.Set("x-client-secret", c.cfg.SecretKey)
	req.Header.Set("x-api-version", "2023-08-01")
}

// VerifyWebhookHMAC checks x-webhook-signature = Base64(HMAC-SHA256(timestamp+rawBody, secret)).
func VerifyWebhookHMAC(secret, timestamp, rawBody, signature string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + rawBody))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}
