package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type mockCFClient struct {
	mu           sync.Mutex
	refundCalls  int
	failRefund   bool
	refundResult *cashfree.RefundDetails
	refundErr    error
}

func (m *mockCFClient) CreateRefund(ctx context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*cashfree.RefundDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refundCalls++
	if m.failRefund {
		if m.refundErr != nil {
			return nil, m.refundErr
		}
		return nil, errors.New("gateway rejected refund: insufficient balance")
	}
	if m.refundResult != nil {
		return m.refundResult, nil
	}
	return &cashfree.RefundDetails{
		CFRefundID:   "cf_ref_mock_" + uuid.New().String()[:8],
		RefundID:     refundID,
		OrderID:      orderID,
		RefundStatus: "SUCCESS",
		AmountPaise:  amountPaise,
		RefundReason: reason,
	}, nil
}

func (m *mockCFClient) FetchRefundStatus(ctx context.Context, orderID, refundID string) (*cashfree.RefundDetails, error) {
	return nil, nil
}

type mockRefundPaymentRepo struct {
	mu          sync.Mutex
	payments    map[uuid.UUID]*domain.Payment
	refunds     map[uuid.UUID]*domain.GatewayRefund
	refundByRef map[string]*domain.GatewayRefund
	refundByCF  map[string]*domain.GatewayRefund
	refundByIdem map[string]*domain.GatewayRefund // key: paymentID_idempotencyKey
	allocations []domain.PaymentAllocation
	refAllocs   []domain.RefundAllocation
}

func newMockRefundPaymentRepo() *mockRefundPaymentRepo {
	return &mockRefundPaymentRepo{
		payments:     make(map[uuid.UUID]*domain.Payment),
		refunds:      make(map[uuid.UUID]*domain.GatewayRefund),
		refundByRef:  make(map[string]*domain.GatewayRefund),
		refundByCF:   make(map[string]*domain.GatewayRefund),
		refundByIdem: make(map[string]*domain.GatewayRefund),
	}
}

func (m *mockRefundPaymentRepo) Create(ctx context.Context, p *domain.Payment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.payments[p.ID] = p
	return nil
}
func (m *mockRefundPaymentRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.payments[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return p, nil
}
func (m *mockRefundPaymentRepo) GetByUPITxnID(ctx context.Context, txnID string) (*domain.Payment, error) {
	return nil, errors.New("not found")
}
func (m *mockRefundPaymentRepo) GetByCFPaymentID(ctx context.Context, cfID string) (*domain.Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.payments {
		if p.CFPaymentID != nil && *p.CFPaymentID == cfID {
			return p, nil
		}
	}
	return nil, errors.New("not found")
}
func (m *mockRefundPaymentRepo) RecordProcessedEvent(ctx context.Context, provider, eventType, providerRefID, eventStatus string) (bool, error) {
	return true, nil
}
func (m *mockRefundPaymentRepo) CreateAllocation(ctx context.Context, paymentID, dueID uuid.UUID, amountPaise int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allocations = append(m.allocations, domain.PaymentAllocation{
		ID:          uuid.New(),
		PaymentID:   paymentID,
		DueID:       dueID,
		AmountPaise: amountPaise,
		CreatedAt:   time.Now(),
	})
	return nil
}
func (m *mockRefundPaymentRepo) ListAllocationsByPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.PaymentAllocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.PaymentAllocation
	for _, a := range m.allocations {
		if a.PaymentID == paymentID {
			out = append(out, a)
		}
	}
	return out, nil
}
func (m *mockRefundPaymentRepo) CreateWebhookEvent(ctx context.Context, evt *domain.WebhookEvent) error {
	return nil
}
func (m *mockRefundPaymentRepo) UpdateWebhookEventStatus(ctx context.Context, id uuid.UUID, status string, errMsg *string) error {
	return nil
}
func (m *mockRefundPaymentRepo) RecordUnmatchedReceipt(ctx context.Context, orderID, cfPaymentID string, intentID *uuid.UUID, amountPaise int64, failureReason string, payload []byte) error {
	return nil
}
func (m *mockRefundPaymentRepo) GetRefundByID(ctx context.Context, id uuid.UUID) (*domain.GatewayRefund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refunds[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return r, nil
}
func (m *mockRefundPaymentRepo) GetRefundByCFRefundID(ctx context.Context, cfRefundID string) (*domain.GatewayRefund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refundByCF[cfRefundID]
	if !ok {
		return nil, errors.New("not found")
	}
	return r, nil
}
func (m *mockRefundPaymentRepo) GetRefundByReference(ctx context.Context, ref string) (*domain.GatewayRefund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refundByRef[ref]
	if !ok {
		return nil, errors.New("not found")
	}
	return r, nil
}
func (m *mockRefundPaymentRepo) GetRefundByPaymentAndIdempotency(ctx context.Context, paymentID uuid.UUID, idempotencyKey string) (*domain.GatewayRefund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s_%s", paymentID, idempotencyKey)
	r, ok := m.refundByIdem[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return r, nil
}
func (m *mockRefundPaymentRepo) GetPaymentRefundedPaise(ctx context.Context, paymentID uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for _, r := range m.refunds {
		if r.PaymentID == paymentID && r.Status != "failed" && r.Status != "cancelled" {
			total += r.AmountPaise
		}
	}
	return total, nil
}
func (m *mockRefundPaymentRepo) ListRefundsByPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.GatewayRefund, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.GatewayRefund
	for _, r := range m.refunds {
		if r.PaymentID == paymentID {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (m *mockRefundPaymentRepo) CreateOrUpdateRefund(ctx context.Context, ref *domain.GatewayRefund) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refunds[ref.ID] = ref
	if ref.RefundReference != nil {
		m.refundByRef[*ref.RefundReference] = ref
	}
	if ref.CFRefundID != nil {
		m.refundByCF[*ref.CFRefundID] = ref
	}
	if ref.IdempotencyKey != nil {
		key := fmt.Sprintf("%s_%s", ref.PaymentID, *ref.IdempotencyKey)
		m.refundByIdem[key] = ref
	}
	return nil
}
func (m *mockRefundPaymentRepo) CreateRefundAllocation(ctx context.Context, alloc *domain.RefundAllocation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refAllocs = append(m.refAllocs, *alloc)
	return nil
}
func (m *mockRefundPaymentRepo) GetDueNetPaidPaise(ctx context.Context, dueID uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var totalPaid int64
	for _, a := range m.allocations {
		if a.DueID == dueID {
			totalPaid += a.AmountPaise
		}
	}
	var totalRefunded int64
	for _, ra := range m.refAllocs {
		if ra.DueID != nil && *ra.DueID == dueID {
			totalRefunded += ra.AmountPaise
		}
	}
	return totalPaid - totalRefunded, nil
}
func (m *mockRefundPaymentRepo) ListStaleNonTerminalRefunds(ctx context.Context, olderThan time.Time) ([]domain.GatewayRefund, error) {
	return nil, nil
}

type mockRefundDueStore struct {
	mu   sync.Mutex
	dues map[uuid.UUID]*domain.Due
}

func (s *mockRefundDueStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.dues[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return d, nil
}
func (s *mockRefundDueStore) List(context.Context, postgres.DueListFilter) ([]domain.Due, error) { return nil, nil }
func (s *mockRefundDueStore) ListByTenant(context.Context, uuid.UUID) ([]domain.Due, error) { return nil, nil }

type mockRefundIntentStore struct {
	mu      sync.Mutex
	byCF    map[string]*domain.PaymentIntent
	byOrder map[string]*domain.PaymentIntent
}

func (s *mockRefundIntentStore) GetByOrderID(_ context.Context, orderID string) (*domain.PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byOrder[orderID], nil
}
func (s *mockRefundIntentStore) GetByCFPaymentID(_ context.Context, cfID string) (*domain.PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byCF[cfID], nil
}
func (s *mockRefundIntentStore) MarkPaid(context.Context, uuid.UUID, string) error { return nil }
func (s *mockRefundIntentStore) GetDuesSnapshot(context.Context, uuid.UUID) ([]domain.PaymentIntentDue, error) { return nil, nil }

type mockRefundTenantStore struct {
	t *domain.Tenant
}

func (s *mockRefundTenantStore) GetByID(context.Context, uuid.UUID) (*domain.Tenant, error) { return s.t, nil }
func (s *mockRefundTenantStore) ListByProperty(context.Context, uuid.UUID) ([]domain.Tenant, error) { return nil, nil }
func (s *mockRefundTenantStore) Update(context.Context, *domain.Tenant) error { return nil }
func (s *mockRefundTenantStore) GetIDPhoto(context.Context, uuid.UUID) ([]byte, error) { return nil, nil }

func setupRefundRouter(h *Handlers, propID, userID uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/owner/payments/:id/refund", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     userID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		h.OwnerRefundPayment(c)
	})
	return r
}

func TestOwnerRefundPayment_UnitScenarios(t *testing.T) {
	propID := uuid.New()
	userID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()

	cfPayID := "cf_pay_test123"
	cfOrderID := "order_test123"

	// 1. Success: Partial and Full Refund
	t.Run("successful full refund", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{}
		tenantStore := &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}}
		dueStore := &mockRefundDueStore{dues: map[uuid.UUID]*domain.Due{
			dueID: {ID: dueID, PropertyID: propID, TenantID: tenantID, Amount: 0, OriginalAmount: 550000, Status: domain.DueStatusPaid},
		}}
		intentStore := &mockRefundIntentStore{byCF: map[string]*domain.PaymentIntent{
			cfPayID: {ProviderOrderID: cfOrderID, CFPaymentID: &cfPayID},
		}}

		basePayment := &domain.Payment{
			ID:                uuid.New(),
			DueID:             dueID,
			TenantID:          tenantID,
			Amount:            550000,
			MatchedBy:         domain.MatchedByCashfree,
			Provider:          "cashfree",
			CFPaymentID:       &cfPayID,
			ProviderPaymentID: &cfPayID,
			CreatedAt:         time.Now(),
		}
		_ = gwRepo.Create(context.Background(), basePayment)
		_ = gwRepo.CreateAllocation(context.Background(), basePayment.ID, dueID, 550000)

		h := &Handlers{Deps: Deps{
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"amount_paise": 550000,
			"reason":       "Tenant cancelled booking before checkin",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Idempotency-Key", "idem_test_001")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp domain.GatewayRefund
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal refund response: %v", err)
		}
		if resp.AmountPaise != 550000 {
			t.Errorf("expected amount 550000 paise, got %d", resp.AmountPaise)
		}
		if resp.Status != "succeeded" {
			t.Errorf("expected status 'succeeded', got %s", resp.Status)
		}
		if resp.RefundReference == nil || len(*resp.RefundReference) != 35 {
			t.Errorf("expected 35-character refund reference, got %v", resp.RefundReference)
		}
		if cfClient.refundCalls != 1 {
			t.Errorf("expected 1 call to Cashfree, got %d", cfClient.refundCalls)
		}
	})

	// 2. Reject: Exceeds Payment Amount
	t.Run("refund exceeds payment amount", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{}
		tenantStore := &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}}
		dueStore := &mockRefundDueStore{dues: map[uuid.UUID]*domain.Due{
			dueID: {ID: dueID, PropertyID: propID, TenantID: tenantID, Amount: 0, OriginalAmount: 550000, Status: domain.DueStatusPaid},
		}}

		basePayment := &domain.Payment{
			ID:                uuid.New(),
			DueID:             dueID,
			TenantID:          tenantID,
			Amount:            550000,
			MatchedBy:         domain.MatchedByCashfree,
			Provider:          "cashfree",
			CFPaymentID:       &cfPayID,
			ProviderPaymentID: &cfPayID,
			CreatedAt:         time.Now(),
		}
		_ = gwRepo.Create(context.Background(), basePayment)

		h := &Handlers{Deps: Deps{
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"amount_paise": 600000, // exceeds 550,000
			"reason":       "Over refund",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on over-refund, got %d", rec.Code)
		}
		if cfClient.refundCalls != 0 {
			t.Errorf("expected 0 calls to Cashfree on validation failure, got %d", cfClient.refundCalls)
		}
	})

	// 3. Reject: Older Than 180 Days
	t.Run("refund older than 180 days", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{}
		tenantStore := &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}}

		oldPayment := &domain.Payment{
			ID:                uuid.New(),
			DueID:             dueID,
			TenantID:          tenantID,
			Amount:            550000,
			MatchedBy:         domain.MatchedByCashfree,
			Provider:          "cashfree",
			CFPaymentID:       &cfPayID,
			ProviderPaymentID: &cfPayID,
			CreatedAt:         time.Now().Add(-185 * 24 * time.Hour), // 185 days ago
		}
		_ = gwRepo.Create(context.Background(), oldPayment)

		h := &Handlers{Deps: Deps{
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"amount_paise": 550000,
			"reason":       "Old refund",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+oldPayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on 180-day expired payment, got %d", rec.Code)
		}
	})

	// 4. Idempotency Replay
	t.Run("idempotency replay returns existing without second gateway call", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{}
		tenantStore := &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}}
		dueStore := &mockRefundDueStore{dues: map[uuid.UUID]*domain.Due{
			dueID: {ID: dueID, PropertyID: propID, TenantID: tenantID, Amount: 0, OriginalAmount: 550000, Status: domain.DueStatusPaid},
		}}

		basePayment := &domain.Payment{
			ID:                uuid.New(),
			DueID:             dueID,
			TenantID:          tenantID,
			Amount:            550000,
			MatchedBy:         domain.MatchedByCashfree,
			Provider:          "cashfree",
			CFPaymentID:       &cfPayID,
			ProviderPaymentID: &cfPayID,
			CreatedAt:         time.Now(),
		}
		_ = gwRepo.Create(context.Background(), basePayment)
		_ = gwRepo.CreateAllocation(context.Background(), basePayment.ID, dueID, 550000)

		h := &Handlers{Deps: Deps{
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"amount_paise": 200000,
			"reason":       "Idempotent partial refund",
		}
		buf, _ := json.Marshal(body)

		// First call
		req1 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req1.Header.Set("Content-Type", "application/json")
		req1.Header.Set("X-Idempotency-Key", "idem_replay_key_999")
		rec1 := httptest.NewRecorder()
		r.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("first call failed: %d", rec1.Code)
		}

		// Second call with same idempotency key
		req2 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("X-Idempotency-Key", "idem_replay_key_999")
		rec2 := httptest.NewRecorder()
		r.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusOK {
			t.Fatalf("second call failed: %d", rec2.Code)
		}

		// Exact same gateway calls count -> Cashfree was NOT called a second time
		if cfClient.refundCalls != 1 {
			t.Fatalf("expected exactly 1 call to Cashfree, got %d", cfClient.refundCalls)
		}
	})

	// 5. Gateway Error Fallback
	t.Run("gateway error marks refund failed and returns 502", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{failRefund: true}
		tenantStore := &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}}
		dueStore := &mockRefundDueStore{dues: map[uuid.UUID]*domain.Due{
			dueID: {ID: dueID, PropertyID: propID, TenantID: tenantID, Amount: 0, OriginalAmount: 550000, Status: domain.DueStatusPaid},
		}}

		basePayment := &domain.Payment{
			ID:                uuid.New(),
			DueID:             dueID,
			TenantID:          tenantID,
			Amount:            550000,
			MatchedBy:         domain.MatchedByCashfree,
			Provider:          "cashfree",
			CFPaymentID:       &cfPayID,
			ProviderPaymentID: &cfPayID,
			CreatedAt:         time.Now(),
		}
		_ = gwRepo.Create(context.Background(), basePayment)
		_ = gwRepo.CreateAllocation(context.Background(), basePayment.ID, dueID, 550000)

		h := &Handlers{Deps: Deps{
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"amount_paise": 200000,
			"reason":       "Failing refund",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("expected 502 Bad Gateway on gateway failure, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// Live Postgres Integration Test with Universal Lock Hierarchy
// ---------------------------------------------------------------------------

func TestLivePostgres_OwnerRefundPayment_LockHierarchy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live postgres test in short mode")
	}
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	// Apply migration 020
	migrationPath := filepath.Join("..", "..", "migrations", "020_gateway_unmatched_and_refunds.sql")
	migrationBytes, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("failed to read migration 020: %v", err)
	}
	if _, err := pool.Exec(ctx, string(migrationBytes)); err != nil {
		t.Fatalf("failed to apply migration 020: %v", err)
	}

	propID := uuid.New()
	userID := uuid.New()
	tenantID := uuid.New()
	dueID1 := uuid.New()
	dueID2 := uuid.New()
	paymentID := uuid.New()

	// 1. Seed Property, User, Tenant, Dues, and Payment
	inviteCode := fmt.Sprintf("P%s", uuid.New().String()[:7])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Refund Test Prop', 'Address', '+919999977777', 'prop@upi', 'Owner', 'o@refund.com', $2)
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("seed property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	userPhone := fmt.Sprintf("+9197%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role)
		VALUES ($1, $2, 'owner')
		ON CONFLICT (phone) DO UPDATE SET role = 'owner'
	`, userID, userPhone)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	}()

	tenantPhone := fmt.Sprintf("+9198%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Refund Tenant', $3, '102', 1100000, 5, 'active')
	`, tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	dueCode1 := uuid.New().String()[:8]
	dueCode2 := uuid.New().String()[:8]

	// Cycle 1 Due: Jan 1
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $2, $3, $4, 0, 550000, 'paid', '2026-01-05', 'rent', '2026-01-01', '2026-02-01')
	`, dueID1, dueCode1, propID, tenantID)
	if err != nil {
		t.Fatalf("seed due 1: %v", err)
	}

	// Cycle 2 Due: Feb 1
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $2, $3, $4, 0, 550000, 'paid', '2026-02-05', 'rent', '2026-02-01', '2026-03-01')
	`, dueID2, dueCode2, propID, tenantID)
	if err != nil {
		t.Fatalf("seed due 2: %v", err)
	}

	cfPaymentID := "cf_pay_live_" + uuid.New().String()[:8]
	cfOrderID := "order_live_" + uuid.New().String()[:8]

	// Seed Payment of ₹11,000 covering both cycles
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, tenant_id, amount, matched_by, provider, provider_payment_id, cf_payment_id, created_at)
		VALUES ($1, $2, 1100000, 'cashfree', 'cashfree', $3, $3, NOW())
	`, paymentID, tenantID, cfPaymentID)
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	// Multi-due payment allocations
	_, err = pool.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, due_id, amount_paise)
		VALUES ($1, $2, 550000), ($1, $3, 550000)
	`, paymentID, dueID1, dueID2)
	if err != nil {
		t.Fatalf("seed payment allocations: %v", err)
	}

	// Seed Payment Intent
	_, err = pool.Exec(ctx, `
		INSERT INTO payment_intents (id, provider, provider_order_id, amount_paise, status, cf_payment_id)
		VALUES ($1, 'cashfree', $2, 1100000, 'paid', $3)
	`, uuid.New(), cfOrderID, cfPaymentID)
	if err != nil {
		t.Fatalf("seed payment intent: %v", err)
	}

	payRepo := postgres.NewPaymentRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	intentRepo := postgres.NewPaymentIntentRepo(pool)
	cfClient := &mockCFClient{}

	h := &Handlers{Deps: Deps{
		Pool:               pool,
		GatewayPaymentRepo: payRepo,
		DueStore:           dueRepo,
		TenantStore:        tenantRepo,
		IntentStore:        intentRepo,
		CashfreeClient:     cfClient,
	}}

	r := setupRefundRouter(h, propID, userID)

	// Round 1: Partial Refund of ₹3,000
	// LIFO rule: Newest due is Cycle 2 (Feb 5).
	// Cycle 2 should receive the ₹3,000 refund allocation.
	// Cycle 2 net paid drops from ₹5,500 to ₹2,500 -> status becomes 'partial', amount becomes ₹2,500.
	// Cycle 1 net paid remains ₹5,500 -> status remains 'paid', amount remains 0.
	body1 := map[string]any{
		"amount_paise": 300000,
		"reason":       "Partial refund round 1",
	}
	buf1, _ := json.Marshal(body1)
	req1 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+paymentID.String()+"/refund", bytes.NewReader(buf1))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("Round 1 refund failed with %d: %s", rec1.Code, rec1.Body.String())
	}

	// Check Due 2 status in Postgres
	var due2Status string
	var due2Amount int
	err = pool.QueryRow(ctx, `SELECT status, amount FROM dues WHERE id = $1`, dueID2).Scan(&due2Status, &due2Amount)
	if err != nil {
		t.Fatalf("query due 2: %v", err)
	}
	if due2Status != "partial" || due2Amount != 300000 {
		t.Fatalf("expected Due 2 to be status='partial' and amount=300000 paise (250000 net paid), got status=%s amount=%d", due2Status, due2Amount)
	}

	// Check Due 1 status in Postgres
	var due1Status string
	var due1Amount int
	err = pool.QueryRow(ctx, `SELECT status, amount FROM dues WHERE id = $1`, dueID1).Scan(&due1Status, &due1Amount)
	if err != nil {
		t.Fatalf("query due 1: %v", err)
	}
	if due1Status != "paid" || due1Amount != 0 {
		t.Fatalf("expected Due 1 to remain status='paid' and amount=0, got status=%s amount=%d", due1Status, due1Amount)
	}

	// Round 2: Second Refund of ₹3,500
	// Remaining on Due 2 is ₹2,500 -> Due 2 receives ₹2,500 allocation -> net paid becomes 0 -> status becomes 'pending', amount becomes ₹5,500.
	// Remaining ₹1,000 spills over to Due 1 -> Due 1 net paid drops to ₹4,500 -> status becomes 'partial', amount becomes ₹1,000.
	body2 := map[string]any{
		"amount_paise": 350000,
		"reason":       "Partial refund round 2",
	}
	buf2, _ := json.Marshal(body2)
	req2 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+paymentID.String()+"/refund", bytes.NewReader(buf2))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("Round 2 refund failed with %d: %s", rec2.Code, rec2.Body.String())
	}

	err = pool.QueryRow(ctx, `SELECT status, amount FROM dues WHERE id = $1`, dueID2).Scan(&due2Status, &due2Amount)
	if err != nil {
		t.Fatalf("query due 2 after round 2: %v", err)
	}
	if due2Status != "pending" || due2Amount != 550000 {
		t.Fatalf("expected Due 2 to become status='pending' and amount=550000, got status=%s amount=%d", due2Status, due2Amount)
	}

	err = pool.QueryRow(ctx, `SELECT status, amount FROM dues WHERE id = $1`, dueID1).Scan(&due1Status, &due1Amount)
	if err != nil {
		t.Fatalf("query due 1 after round 2: %v", err)
	}
	if due1Status != "partial" || due1Amount != 100000 {
		t.Fatalf("expected Due 1 to become status='partial' and amount=100000, got status=%s amount=%d", due1Status, due1Amount)
	}

	// Round 3: Over-refund attempt
	// Total refunded so far: ₹3,000 + ₹3,500 = ₹6,500. Remaining balance: ₹4,500.
	// Requesting ₹5,000 must be rejected with 400!
	body3 := map[string]any{
		"amount_paise": 500000, // exceeds remaining 4,500
		"reason":       "Attempting to exceed remaining refundable balance",
	}
	buf3, _ := json.Marshal(body3)
	req3 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+paymentID.String()+"/refund", bytes.NewReader(buf3))
	req3.Header.Set("Content-Type", "application/json")
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on over-refund, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

func TestLivePostgres_OwnerRefundPayment_GuardA_Rejection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live postgres test in short mode")
	}
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	// Ensure migration 022 is applied
	m22Path := filepath.Join("..", "..", "migrations", "022_payout_batches_and_payees.sql")
	m22Bytes, err := os.ReadFile(m22Path)
	if err == nil {
		_, _ = pool.Exec(ctx, string(m22Bytes))
	}

	propID := uuid.New()
	userID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()
	paymentID := uuid.New()

	inviteCode := fmt.Sprintf("P%s", uuid.New().String()[:7])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Guard A Prop', 'Address', '+919999966666', 'guard@upi', 'Owner', 'guard@test.com', $2)
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("seed property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	userPhone := fmt.Sprintf("+9196%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role)
		VALUES ($1, $2, 'owner')
		ON CONFLICT (phone) DO UPDATE SET role = 'owner'
	`, userID, userPhone)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	}()

	tenantPhone := fmt.Sprintf("+9195%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Departed Tenant', $3, '103', 550000, 5, 'active')
	`, tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	dueCode := uuid.New().String()[:8]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $2, $3, $4, 0, 550000, 'paid', '2026-03-05', 'rent', '2026-03-01', '2026-04-01')
	`, dueID, dueCode, propID, tenantID)
	if err != nil {
		t.Fatalf("seed due: %v", err)
	}

	cfPaymentID := "cf_pay_guard_" + uuid.New().String()[:8]
	cfOrderID := "order_guard_" + uuid.New().String()[:8]

	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, tenant_id, amount, matched_by, provider, provider_payment_id, cf_payment_id, created_at)
		VALUES ($1, $2, 550000, 'cashfree', 'cashfree', $3, $3, NOW())
	`, paymentID, tenantID, cfPaymentID)
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, due_id, amount_paise)
		VALUES ($1, $2, 550000)
	`, paymentID, dueID)
	if err != nil {
		t.Fatalf("seed payment allocations: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO payment_intents (id, provider, provider_order_id, amount_paise, status, cf_payment_id)
		VALUES ($1, 'cashfree', $2, 550000, 'paid', $3)
	`, uuid.New(), cfOrderID, cfPaymentID)
	if err != nil {
		t.Fatalf("seed payment intent: %v", err)
	}

	// Create approved departure for tenant
	depID := uuid.New()
	vacateDate := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenant_departures (id, tenant_id, property_id, notice_given_at, planned_vacate_date, deposit_amount_paise, net_refund_paise, status)
		VALUES ($1, $2, $3, NOW(), $4, 1000000, 725000, 'approved')
	`, depID, tenantID, propID, vacateDate)
	if err != nil {
		t.Fatalf("seed departure: %v", err)
	}

	payRepo := postgres.NewPaymentRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	intentRepo := postgres.NewPaymentIntentRepo(pool)
	cfClient := &mockCFClient{}

	h := &Handlers{Deps: Deps{
		Pool:               pool,
		GatewayPaymentRepo: payRepo,
		DueStore:           dueRepo,
		TenantStore:        tenantRepo,
		IntentStore:        intentRepo,
		CashfreeClient:     cfClient,
	}}

	r := setupRefundRouter(h, propID, userID)

	// Case 1: Tenant has approved departure -> MUST return 400 Bad Request
	body := map[string]any{
		"amount_paise": 100000,
		"reason":       "Attempting refund on departed tenant",
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+paymentID.String()+"/refund", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request from Guard A (approved departure), got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	expectedErrMsg := "Cannot issue gateway refund against payment tied to a settled departure. Deposit and rent adjustments must be resolved via departure disbursement/payout ledger."
	if resp["error"] != expectedErrMsg {
		t.Fatalf("expected error message %q, got %q", expectedErrMsg, resp["error"])
	}

	// Case 2: Departure deleted/cancelled, but linked due has departure_due_adjustments -> MUST still return 400 Bad Request
	_, err = pool.Exec(ctx, `UPDATE tenant_departures SET status = 'cancelled' WHERE id = $1`, depID)
	if err != nil {
		t.Fatalf("update departure to cancelled: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO departure_due_adjustments (departure_id, due_id, amount_paise, adjustment_type, created_at)
		VALUES ($1, $2, 275000, 'unused_rent_reversal', NOW())
	`, depID, dueID)
	if err != nil {
		t.Fatalf("seed departure due adjustment: %v", err)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+paymentID.String()+"/refund", bytes.NewReader(buf))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request from Guard A (departure due adjustments), got %d: %s", rec2.Code, rec2.Body.String())
	}
	var resp2 map[string]string
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if resp2["error"] != expectedErrMsg {
		t.Fatalf("expected error message %q, got %q", expectedErrMsg, resp2["error"])
	}
}

func TestRecomputeDueStatus_DepartureScenarios(t *testing.T) {
	// Tests §1.5 Departure Scenarios against RecomputeDueStatusMath
	// Contractual ceiling for mid-cycle Day 15 departure: prorated owed = ₹2,750 (275000 paise)

	// Scenario 1: Unpaid rent at departure
	// Billed: ₹5,500. Prorated: ₹2,750. Tenant paid ₹0.
	// Netting payment from deposit = ₹2,750.
	// payment_allocations = 275000, refund_allocations = 0, departure_adjustments = 0.
	// Net Paid = 275000.
	status1, remAmt1 := RecomputeDueStatusMath(275000, 275000)
	if status1 != domain.DueStatusPaid || remAmt1 != 0 {
		t.Fatalf("Scenario 1 failed: expected status='paid', amount=0, got status=%s amount=%d", status1, remAmt1)
	}

	// Scenario 2: Advance rent paid in full
	// Billed: ₹5,500. Prorated: ₹2,750. Tenant paid ₹5,500 on Day 1.
	// payment_allocations = 550000, refund_allocations = 0, departure_adjustments = 275000 (unused rent reversal).
	// Net Paid = 550000 - 275000 = 275000.
	netPaid2 := int64(550000 - 275000)
	status2, remAmt2 := RecomputeDueStatusMath(netPaid2, 275000)
	if status2 != domain.DueStatusPaid || remAmt2 != 0 {
		t.Fatalf("Scenario 2 failed: expected status='paid', amount=0, got status=%s amount=%d", status2, remAmt2)
	}

	// Scenario 3: Partial payment with excess above prorated owed
	// Billed: ₹5,500. Prorated: ₹2,750. Tenant paid ₹3,000 on Day 1.
	// payment_allocations = 300000, refund_allocations = 0, departure_adjustments = 25000 (reversal of 3,000 - 2,750).
	// Net Paid = 300000 - 25000 = 275000.
	netPaid3 := int64(300000 - 25000)
	status3, remAmt3 := RecomputeDueStatusMath(netPaid3, 275000)
	if status3 != domain.DueStatusPaid || remAmt3 != 0 {
		t.Fatalf("Scenario 3 failed: expected status='paid', amount=0, got status=%s amount=%d", status3, remAmt3)
	}

	// Scenario 3b: Partial underpayment below prorated owed
	// Billed: ₹5,500. Prorated: ₹2,750. Tenant paid only ₹2,000 on Day 1.
	// payment_allocations = 200000, refund_allocations = 0, departure_adjustments = 0 (tenant owes remaining 750).
	// Net Paid = 200000.
	netPaid3b := int64(200000)
	status3b, remAmt3b := RecomputeDueStatusMath(netPaid3b, 275000)
	if status3b != domain.DueStatusPartial || remAmt3b != 75000 {
		t.Fatalf("Scenario 3b failed: expected status='partial', amount=75000 (₹750), got status=%s amount=%d", status3b, remAmt3b)
	}

	// Standard Non-Departure Due: ₹5,500 due, ₹3,000 refund on ₹5,500 payment
	// Net Paid = 550000 - 300000 = 250000 against ceiling 550000.
	statusStd, remAmtStd := RecomputeDueStatusMath(250000, 550000)
	if statusStd != domain.DueStatusPartial || remAmtStd != 300000 {
		t.Fatalf("Standard partial refund failed: expected status='partial', amount=300000, got status=%s amount=%d", statusStd, remAmtStd)
	}
}
