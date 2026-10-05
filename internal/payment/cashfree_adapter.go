package payment

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/pg-cashflow/pg-go/internal/cashfree"
)

// CashfreeAdapter implements PaymentProvider using the internal/cashfree client.
type CashfreeAdapter struct {
	client *cashfree.Client
	cfg    cashfree.Config
}

func NewCashfreeAdapter(cfg cashfree.Config) *CashfreeAdapter {
	return &CashfreeAdapter{
		client: cashfree.NewClient(cfg),
		cfg:    cfg,
	}
}

// Client returns the underlying cashfree.Client if direct access is required for legacy code.
func (a *CashfreeAdapter) Client() *cashfree.Client {
	return a.client
}

func (a *CashfreeAdapter) Name() ProviderName {
	return ProviderCashfree
}

func (a *CashfreeAdapter) CreateOrder(ctx context.Context, req CreateOrderRequest) (*ProviderOrder, error) {
	sessionID, exp, err := a.client.CreateUPIOrder(ctx, req.OrderID, req.AmountPaise, req.CustomerPhone, req.Note)
	if err != nil {
		return nil, err
	}
	return &ProviderOrder{
		OrderID:          req.OrderID,
		PaymentSessionID: sessionID,
		AmountPaise:      req.AmountPaise,
		Currency:         "INR",
		Status:           "ACTIVE",
		ExpiresAt:        exp,
	}, nil
}

// CreateUPIOrder satisfies the legacy collector.CashfreeOrders interface.
func (a *CashfreeAdapter) CreateUPIOrder(ctx context.Context, orderID string, amountPaise int64, customerPhone, note string) (string, *time.Time, error) {
	order, err := a.CreateOrder(ctx, CreateOrderRequest{
		OrderID:       orderID,
		AmountPaise:   amountPaise,
		CustomerPhone: customerPhone,
		Note:          note,
	})
	if err != nil {
		return "", nil, err
	}
	return order.PaymentSessionID, order.ExpiresAt, nil
}

func (a *CashfreeAdapter) FetchPayment(ctx context.Context, orderID string) (*ProviderPayment, error) {
	cfPaymentID, bankRef, amountPaise, ok, err := a.client.FetchSuccessfulPayment(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cashfree payment not found or not successful for order %s", orderID)
	}
	return &ProviderPayment{
		PaymentID:     cfPaymentID,
		OrderID:       orderID,
		AmountPaise:   int64(amountPaise),
		Currency:      "INR",
		Status:        "SUCCESS",
		BankReference: bankRef,
		PaymentTime:   time.Now(),
	}, nil
}

// FetchSuccessfulPayment satisfies the jobs.CashfreeFetcher interface.
func (a *CashfreeAdapter) FetchSuccessfulPayment(ctx context.Context, orderID string) (cfPaymentID, bankRef string, amountPaise int64, ok bool, err error) {
	return a.client.FetchSuccessfulPayment(ctx, orderID)
}

func (a *CashfreeAdapter) FetchRefund(ctx context.Context, orderID, refundID string) (*ProviderRefund, error) {
	details, err := a.client.FetchRefundStatus(ctx, orderID, refundID)
	if err != nil {
		return nil, err
	}
	return &ProviderRefund{
		RefundID:    details.CFRefundID,
		PaymentID:   details.CFPaymentID,
		AmountPaise: details.AmountPaise,
		Status:      details.RefundStatus,
		Reason:      details.RefundReason,
		Source:      details.RefundType,
		ProcessedAt: time.Now(),
	}, nil
}

// FetchRefundStatus satisfies the jobs.RefundStatusFetcher interface.
func (a *CashfreeAdapter) FetchRefundStatus(ctx context.Context, orderID, refundID string) (*cashfree.RefundDetails, error) {
	return a.client.FetchRefundStatus(ctx, orderID, refundID)
}

func (a *CashfreeAdapter) VerifyWebhook(headers http.Header, rawBody []byte) error {
	sig := headers.Get("x-webhook-signature")
	ts := headers.Get("x-webhook-timestamp")
	if sig == "" {
		return fmt.Errorf("cashfree: missing x-webhook-signature header")
	}
	if err := cashfree.VerifyWebhookTimestamp(ts, 300, time.Now()); err != nil {
		return err
	}
	if !cashfree.VerifyWebhookHMAC(a.cfg.SecretKey, ts, string(rawBody), sig) {
		return fmt.Errorf("cashfree: invalid webhook signature")
	}
	return nil
}

func (a *CashfreeAdapter) ParseWebhook(headers http.Header, rawBody []byte) (*NormalizedWebhookEvent, error) {
	parsed, evType, err := cashfree.ParseWebhook(rawBody)
	if err != nil {
		return nil, err
	}
	norm := &NormalizedWebhookEvent{
		Provider:   ProviderCashfree,
		RawPayload: rawBody,
		Headers:    headers,
		EventTime:  time.Now(),
	}
	switch v := parsed.(type) {
	case cashfree.SuccessWebhook:
		norm.EventType = EventTypePaymentSuccess
		norm.EventStatus = "SUCCESS"
		norm.ProviderReferenceID = v.CFPaymentID
		norm.OrderID = v.OrderID
		norm.PaymentID = v.CFPaymentID
		norm.AmountPaise = int64(v.AmountPaise)
	case cashfree.FailedWebhook:
		norm.EventType = EventTypePaymentFailed
		norm.EventStatus = "FAILED"
		norm.ProviderReferenceID = v.CFPaymentID
		norm.OrderID = v.OrderID
		norm.PaymentID = v.CFPaymentID
		norm.AmountPaise = int64(v.AmountPaise)
	case cashfree.RefundWebhook:
		if v.IsAutoRefund {
			norm.EventType = EventTypeAutoRefund
		} else {
			norm.EventType = EventTypeRefundStatus
		}
		norm.EventStatus = v.RefundStatus
		norm.ProviderReferenceID = v.CFRefundID
		norm.OrderID = v.OrderID
		norm.PaymentID = v.CFPaymentID
		norm.RefundID = v.CFRefundID
		norm.AmountPaise = v.RefundAmount
	default:
		norm.EventType = EventTypeUnknown
		norm.EventStatus = evType
	}
	return norm, nil
}
