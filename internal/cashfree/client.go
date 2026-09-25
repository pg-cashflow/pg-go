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
	AppID               string
	SecretKey           string
	Env                 string // sandbox | production
	APIVersion          string // e.g. "2025-01-01"
	OrderExpiryDuration time.Duration
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
	expiryDur := c.cfg.OrderExpiryDuration
	if expiryDur <= 0 {
		expiryDur = 30 * time.Minute
	}
	exp := c.now().Add(expiryDur).UTC()

	body := createOrderBody{
		OrderID:       orderID,
		OrderAmount:   float64(amountPaise) / 100.0,
		OrderCurrency: "INR",
		Customer:      map[string]any{"customer_id": orderID, "customer_phone": customerPhone},
		OrderMeta: map[string]any{
			"payment_methods":   "upi",
			"order_expiry_time": exp.Format(time.RFC3339),
		},
		OrderNote: note,
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
	return out.PaymentSessionID, &exp, nil
}

type rawPaymentItem struct {
	CFPaymentID   json.Number `json:"cf_payment_id"`
	PaymentStatus string      `json:"payment_status"`
	PaymentAmount json.Number `json:"payment_amount"`
	BankReference string      `json:"bank_reference"`
}

type fetchPaymentsResp struct {
	Payments []rawPaymentItem `json:"payments"`
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
		var arr []rawPaymentItem
		if err2 := json.Unmarshal(b, &arr); err2 != nil {
			return "", "", 0, false, err
		}
		out.Payments = arr
	}
	for _, p := range out.Payments {
		if strings.EqualFold(p.PaymentStatus, "SUCCESS") {
			parsedPaise, pErr := ParseRupeesToPaise(p.PaymentAmount.String())
			if pErr != nil {
				return "", "", 0, false, fmt.Errorf("cashfree fetch parse amount: %w", pErr)
			}
			return p.CFPaymentID.String(), p.BankReference, int(parsedPaise), true, nil
		}
	}
	return "", "", 0, false, nil
}

type RefundDetails struct {
	CFRefundID   string
	RefundID     string
	OrderID      string
	CFPaymentID  string
	RefundStatus string
	AmountPaise  int64
	RefundType   string
	RefundReason string
}

func (c *Client) FetchRefundStatus(ctx context.Context, orderID, refundID string) (*RefundDetails, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint()+"/orders/"+orderID+"/refunds/"+refundID, nil)
	if err != nil {
		return nil, err
	}
	c.sign(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cashfree fetch refund: %s %s", resp.Status, string(b))
	}
	var raw struct {
		CFRefundID   json.Number `json:"cf_refund_id"`
		RefundID     string      `json:"refund_id"`
		OrderID      string      `json:"order_id"`
		CFPaymentID  json.Number `json:"cf_payment_id"`
		RefundStatus string      `json:"refund_status"`
		RefundAmount json.Number `json:"refund_amount"`
		RefundType   string      `json:"refund_type"`
		RefundReason string      `json:"refund_reason"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("cashfree fetch refund json: %w", err)
	}
	parsedAmount, err := ParseRupeesToPaise(raw.RefundAmount.String())
	if err != nil {
		return nil, fmt.Errorf("cashfree parse refund amount: %w", err)
	}
	return &RefundDetails{
		CFRefundID:   raw.CFRefundID.String(),
		RefundID:     raw.RefundID,
		OrderID:      raw.OrderID,
		CFPaymentID:  raw.CFPaymentID.String(),
		RefundStatus: strings.ToUpper(strings.TrimSpace(raw.RefundStatus)),
		AmountPaise:  parsedAmount,
		RefundType:   raw.RefundType,
		RefundReason: raw.RefundReason,
	}, nil
}

type CreateRefundRequest struct {
	RefundAmount float64 `json:"refund_amount"`
	RefundID     string  `json:"refund_id"`
	RefundNote   string  `json:"refund_note,omitempty"`
	RefundSpeed  string  `json:"refund_speed,omitempty"`
}

func (c *Client) CreateRefund(ctx context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*RefundDetails, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	body := CreateRefundRequest{
		RefundAmount: float64(amountPaise) / 100.0,
		RefundID:     refundID,
		RefundNote:   reason,
		RefundSpeed:  "STANDARD",
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cashfree marshal refund: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint()+"/orders/"+orderID+"/refunds", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.sign(req)
	if idempotencyKey != "" {
		req.Header.Set("x-idempotency-key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cashfree create refund: %s %s", resp.Status, string(b))
	}
	var raw struct {
		CFRefundID   json.Number `json:"cf_refund_id"`
		RefundID     string      `json:"refund_id"`
		OrderID      string      `json:"order_id"`
		CFPaymentID  json.Number `json:"cf_payment_id"`
		RefundStatus string      `json:"refund_status"`
		RefundAmount json.Number `json:"refund_amount"`
		RefundType   string      `json:"refund_type"`
		RefundReason string      `json:"refund_reason"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("cashfree create refund json: %w", err)
	}
	parsedAmount, err := ParseRupeesToPaise(raw.RefundAmount.String())
	if err != nil {
		return nil, fmt.Errorf("cashfree parse refund amount: %w", err)
	}
	return &RefundDetails{
		CFRefundID:   raw.CFRefundID.String(),
		RefundID:     raw.RefundID,
		OrderID:      raw.OrderID,
		CFPaymentID:  raw.CFPaymentID.String(),
		RefundStatus: strings.ToUpper(strings.TrimSpace(raw.RefundStatus)),
		AmountPaise:  parsedAmount,
		RefundType:   raw.RefundType,
		RefundReason: raw.RefundReason,
	}, nil
}

func (c *Client) sign(req *http.Request) {
	req.Header.Set("x-client-id", c.cfg.AppID)
	req.Header.Set("x-client-secret", c.cfg.SecretKey)
	ver := c.cfg.APIVersion
	if ver == "" {
		ver = "2025-01-01"
	}
	req.Header.Set("x-api-version", ver)
}

// VerifyWebhookHMAC checks x-webhook-signature = Base64(HMAC-SHA256(timestamp+rawBody, secret)).
func VerifyWebhookHMAC(secret, timestamp, rawBody, signature string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + rawBody))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}
