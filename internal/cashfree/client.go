package cashfree

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type Config struct {
	AppID                      string
	SecretKey                  string
	Env                        string // sandbox | production
	APIVersion                 string // e.g. "2025-01-01"
	OrderExpiryDuration        time.Duration
	WebhookReplayWindowSeconds int // replay window in seconds; defaults to 300 if <= 0
	ReturnURL                  string
	KYCAuthMode                string // "ip" (default) | "signature"
	KYCPublicKey               *rsa.PublicKey
	KYCClientID                string
	Now                        func() time.Time
}

func (c Config) WebhookReplayWindow() int {
	if c.WebhookReplayWindowSeconds <= 0 {
		return 300
	}
	return c.WebhookReplayWindowSeconds
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
	kycHTTP      *http.Client
	now          func() time.Time
	baseOverride string
}

func (c *Client) endpoint() string {
	if c.baseOverride != "" {
		return strings.TrimRight(c.baseOverride, "/")
	}
	return c.cfg.baseURL()
}

func (c *Client) kycClient() *http.Client {
	if c.kycHTTP != nil {
		return c.kycHTTP
	}
	return c.http
}

func NewClient(cfg Config) *Client {
	return &Client{
		cfg:     cfg,
		http:    &http.Client{Timeout: 15 * time.Second},
		kycHTTP: &http.Client{Timeout: 35 * time.Second},
		now:     time.Now,
	}
}

// WithBaseURL overrides the Cashfree API base endpoint (e.g. for local test harness or fake gateway).
func (c *Client) WithBaseURL(override string) *Client {
	c.baseOverride = strings.TrimRight(override, "/")
	return c
}

type createOrderBody struct {
	OrderID       string         `json:"order_id"`
	OrderAmount   json.Number    `json:"order_amount"`
	OrderCurrency string         `json:"order_currency"`
	Customer      map[string]any `json:"customer_details"`
	OrderMeta     map[string]any `json:"order_meta"`
	OrderNote     string         `json:"order_note,omitempty"`
}

type createOrderResp struct {
	PaymentSessionID string `json:"payment_session_id"`
}

func (c *Client) CreateUPIOrder(ctx context.Context, orderID string, amountPaise int64, customerPhone, note string) (string, *time.Time, error) {
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

	orderMeta := map[string]any{
		"payment_methods":   "upi",
		"order_expiry_time": exp.Format(time.RFC3339),
	}
	if c.cfg.ReturnURL != "" {
		orderMeta["return_url"] = c.cfg.ReturnURL
	}

	customerID := orderID
	if customerPhone != "" {
		customerID = "cust_" + strings.TrimPrefix(customerPhone, "+")
	}

	body := createOrderBody{
		OrderID:       orderID,
		OrderAmount:   json.Number(FormatPaiseToRupees(amountPaise)),
		OrderCurrency: "INR",
		Customer:      map[string]any{"customer_id": customerID, "customer_phone": customerPhone},
		OrderMeta:     orderMeta,
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

func (c *Client) FetchSuccessfulPayment(ctx context.Context, orderID string) (cfPaymentID, bankRef string, amountPaise int64, ok bool, err error) {
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
			return p.CFPaymentID.String(), p.BankReference, parsedPaise, true, nil
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
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(b),
		}
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
	RefundAmount json.Number `json:"refund_amount"`
	RefundID     string      `json:"refund_id"`
	RefundNote   string      `json:"refund_note,omitempty"`
	RefundSpeed  string      `json:"refund_speed,omitempty"`
}

func (c *Client) CreateRefund(ctx context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*RefundDetails, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	body := CreateRefundRequest{
		RefundAmount: json.Number(FormatPaiseToRupees(amountPaise)),
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
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(b),
		}
	}
	type refundRaw struct {
		CFRefundID   json.Number `json:"cf_refund_id"`
		RefundID     string      `json:"refund_id"`
		OrderID      string      `json:"order_id"`
		CFPaymentID  json.Number `json:"cf_payment_id"`
		RefundStatus string      `json:"refund_status"`
		RefundAmount json.Number `json:"refund_amount"`
		RefundType   string      `json:"refund_type"`
		RefundReason string      `json:"refund_reason"`
	}
	var raw refundRaw
	if err := json.Unmarshal(b, &raw); err != nil {
		var rawList []refundRaw
		if errList := json.Unmarshal(b, &rawList); errList == nil && len(rawList) > 0 {
			raw = rawList[0]
		} else {
			return nil, fmt.Errorf("cashfree create refund json: %w", err)
		}
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

// GenerateKYCSignature creates the Cashfree X-Cf-Signature header value for KYC / Secure ID endpoints.
// It formats "${clientID}.${unixTimestamp}" and encrypts it using RSA-OAEP with SHA-1,
// matching Cashfree's signature verification specification. The resulting ciphertext is Base64 encoded.
func GenerateKYCSignature(clientID string, pubKey *rsa.PublicKey, now time.Time) (string, error) {
	if strings.TrimSpace(clientID) == "" {
		return "", errors.New("cashfree kyc: client_id required for signature generation")
	}
	if pubKey == nil {
		return "", errors.New("cashfree kyc: public key required for signature generation")
	}
	plainText := fmt.Sprintf("%s.%d", clientID, now.Unix())
	cipherBytes, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pubKey, []byte(plainText), nil)
	if err != nil {
		return "", fmt.Errorf("cashfree kyc: encrypt signature: %w", err)
	}
	return base64.StdEncoding.EncodeToString(cipherBytes), nil
}

func (c *Client) signKYC(req *http.Request) error {
	clientID := c.cfg.KYCClientID
	if clientID == "" {
		clientID = c.cfg.AppID
	}
	req.Header.Set("x-client-id", clientID)
	req.Header.Set("x-client-secret", c.cfg.SecretKey)
	ver := c.cfg.APIVersion
	if ver == "" {
		ver = "2025-01-01"
	}
	req.Header.Set("x-api-version", ver)

	if strings.EqualFold(c.cfg.KYCAuthMode, "signature") {
		if c.cfg.KYCPublicKey == nil {
			return errors.New("cashfree kyc: auth_mode signature requires kyc public key")
		}
		now := time.Now().UTC()
		if c.cfg.Now != nil {
			now = c.cfg.Now()
		} else if c.now != nil {
			now = c.now()
		}
		sig, err := GenerateKYCSignature(clientID, c.cfg.KYCPublicKey, now)
		if err != nil {
			slog.Error("cashfree kyc: failed to generate X-Cf-Signature", "err", err)
			return fmt.Errorf("cashfree kyc: generate signature: %w", err)
		}
		req.Header.Set("X-Cf-Signature", sig)
	}
	return nil
}

// VerifyWebhookHMAC checks x-webhook-signature = Base64(HMAC-SHA256(timestamp+rawBody, secret)).
// Fails closed if secret or signature is empty.
func VerifyWebhookHMAC(secret, timestamp, rawBody, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + rawBody))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}
