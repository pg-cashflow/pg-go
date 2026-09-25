package payment

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// MockPaymentProvider is a thread-safe mock implementation of PaymentProvider for unit & integration testing.
type MockPaymentProvider struct {
	mu          sync.Mutex
	name        ProviderName
	Orders      map[string]*ProviderOrder
	Payments    map[string]*ProviderPayment
	Refunds     map[string]*ProviderRefund
	CreateErr   error
	FetchPayErr error
	FetchRefErr error
	VerifyErr   error
	ParseEvent  *NormalizedWebhookEvent
}

func NewMockPaymentProvider(name ProviderName) *MockPaymentProvider {
	if name == "" {
		name = ProviderCashfree
	}
	return &MockPaymentProvider{
		name:     name,
		Orders:   make(map[string]*ProviderOrder),
		Payments: make(map[string]*ProviderPayment),
		Refunds:  make(map[string]*ProviderRefund),
	}
}

func (m *MockPaymentProvider) Name() ProviderName {
	return m.name
}

func (m *MockPaymentProvider) CreateOrder(ctx context.Context, req CreateOrderRequest) (*ProviderOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.CreateErr != nil {
		return nil, m.CreateErr
	}
	exp := time.Now().Add(30 * time.Minute)
	if req.ExpiryTime != nil {
		exp = *req.ExpiryTime
	}
	order := &ProviderOrder{
		OrderID:          req.OrderID,
		PaymentSessionID: "mock_session_" + req.OrderID,
		AmountPaise:      req.AmountPaise,
		Currency:         "INR",
		Status:           "ACTIVE",
		ExpiresAt:        &exp,
	}
	m.Orders[req.OrderID] = order
	return order, nil
}

func (m *MockPaymentProvider) CreateUPIOrder(ctx context.Context, orderID string, amountPaise int, customerPhone, note string) (string, *time.Time, error) {
	order, err := m.CreateOrder(ctx, CreateOrderRequest{
		OrderID:       orderID,
		AmountPaise:   int64(amountPaise),
		CustomerPhone: customerPhone,
		Note:          note,
	})
	if err != nil {
		return "", nil, err
	}
	return order.PaymentSessionID, order.ExpiresAt, nil
}

func (m *MockPaymentProvider) FetchPayment(ctx context.Context, orderID string) (*ProviderPayment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FetchPayErr != nil {
		return nil, m.FetchPayErr
	}
	p, ok := m.Payments[orderID]
	if !ok {
		return nil, fmt.Errorf("mock: payment not found for order %s", orderID)
	}
	return p, nil
}

func (m *MockPaymentProvider) FetchRefund(ctx context.Context, orderID, refundID string) (*ProviderRefund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FetchRefErr != nil {
		return nil, m.FetchRefErr
	}
	r, ok := m.Refunds[refundID]
	if !ok {
		return nil, fmt.Errorf("mock: refund not found for id %s", refundID)
	}
	return r, nil
}

func (m *MockPaymentProvider) VerifyWebhook(headers http.Header, rawBody []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.VerifyErr
}

func (m *MockPaymentProvider) ParseWebhook(headers http.Header, rawBody []byte) (*NormalizedWebhookEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ParseEvent != nil {
		return m.ParseEvent, nil
	}
	return &NormalizedWebhookEvent{
		Provider:    m.name,
		EventType:   EventTypePaymentSuccess,
		EventStatus: "SUCCESS",
		AmountPaise: 550000,
		RawPayload:  rawBody,
		EventTime:   time.Now(),
	}, nil
}
