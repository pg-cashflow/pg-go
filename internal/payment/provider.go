package payment

import (
	"context"
	"net/http"
	"time"
)

// ProviderName represents the payment gateway provider identifier.
type ProviderName string

const (
	ProviderCashfree ProviderName = "cashfree"
	ProviderRazorpay ProviderName = "razorpay"
	ProviderPayU     ProviderName = "payu"
	ProviderPhonePe  ProviderName = "phonepe"
	ProviderManual   ProviderName = "manual"
)

// CreateOrderRequest contains provider-agnostic parameters for creating a payment order.
type CreateOrderRequest struct {
	OrderID       string
	AmountPaise   int64
	Currency      string
	CustomerPhone string
	CustomerEmail string
	CustomerName  string
	CustomerID    string
	Note          string
	ExpiryTime    *time.Time
	ReturnURL     string
}

// ProviderOrder is the normalized response from order creation.
type ProviderOrder struct {
	OrderID          string
	PaymentSessionID string
	AmountPaise      int64
	Currency         string
	Status           string // "ACTIVE", "PAID", "EXPIRED"
	ExpiresAt        *time.Time
	RawResponse      []byte
}

// ProviderPayment is the normalized representation of a payment transaction.
type ProviderPayment struct {
	PaymentID     string
	OrderID       string
	AmountPaise   int64
	Currency      string
	Status        string // "SUCCESS", "FAILED", "PENDING", "CANCELLED", "USER_DROPPED"
	PaymentMethod string
	BankReference string
	PaymentTime   time.Time
	ErrorMessage  string
	RawResponse   []byte
}

// ProviderRefund is the normalized representation of a refund transaction.
type ProviderRefund struct {
	RefundID      string
	PaymentID     string
	AmountPaise   int64
	Status        string // "SUCCESS", "PENDING", "FAILED", "ON_HOLD", "CANCELLED"
	Reason        string
	Source        string
	ProcessedAt   time.Time
	RawResponse   []byte
}

// NormalizedEventType identifies common payment lifecycle events across all gateways.
type NormalizedEventType string

const (
	EventTypePaymentSuccess NormalizedEventType = "PAYMENT_SUCCESS"
	EventTypePaymentFailed  NormalizedEventType = "PAYMENT_FAILED"
	EventTypeRefundStatus   NormalizedEventType = "REFUND_STATUS"
	EventTypeAutoRefund     NormalizedEventType = "AUTO_REFUND"
	EventTypeUnknown        NormalizedEventType = "UNKNOWN"
)

// NormalizedWebhookEvent standardizes inbound webhook callbacks regardless of gateway.
type NormalizedWebhookEvent struct {
	Provider            ProviderName
	EventType           NormalizedEventType
	EventStatus         string // "SUCCESS", "FAILED", etc.
	ProviderReferenceID string // Payment ID or Refund ID used for dedup
	OrderID             string
	PaymentID           string
	RefundID            string
	AmountPaise         int64
	EventTime           time.Time
	RawPayload          []byte
	Headers             http.Header
}

// PaymentProvider abstracts interactions with any payment gateway.
type PaymentProvider interface {
	Name() ProviderName
	CreateOrder(ctx context.Context, req CreateOrderRequest) (*ProviderOrder, error)
	FetchPayment(ctx context.Context, paymentID string) (*ProviderPayment, error)
	FetchRefund(ctx context.Context, paymentID, refundID string) (*ProviderRefund, error)
	VerifyWebhook(headers http.Header, rawBody []byte) error
	ParseWebhook(headers http.Header, rawBody []byte) (*NormalizedWebhookEvent, error)
}
