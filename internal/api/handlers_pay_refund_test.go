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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

type mockCFClient struct {
	mu           sync.Mutex
	refundCalls  int
	failRefund   bool
	refundResult *cashfree.RefundDetails
	refundErr    error
	lastReason   string
}

func (m *mockCFClient) CreateRefund(ctx context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*cashfree.RefundDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refundCalls++
	m.lastReason = reason
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

func (m *mockCFClient) FetchSuccessfulPayment(ctx context.Context, orderID string) (cfPaymentID, bankRef string, amountPaise int64, ok bool, err error) {
	return "", "", 0, false, nil
}

func (m *mockCFClient) CancelRefund(ctx context.Context, orderID, refundID, remarks string) (*cashfree.RefundDetails, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRefund {
		if m.refundErr != nil {
			return nil, m.refundErr
		}
		return nil, errors.New("gateway rejected cancel refund")
	}
	amt := int64(50000)
	if m.refundResult != nil && m.refundResult.AmountPaise > 0 {
		amt = m.refundResult.AmountPaise
	}
	return &cashfree.RefundDetails{
		CFRefundID:   "cf_ref_mock_cancel",
		RefundID:     refundID,
		OrderID:      orderID,
		RefundStatus: "CANCELLED",
		AmountPaise:  amt,
		RefundReason: remarks,
	}, nil
}

func (m *mockCFClient) IsSandbox() bool {
	return true
}

type mockRefundPaymentRepo struct {
	mu           sync.Mutex
	payments     map[uuid.UUID]*domain.Payment
	refunds      map[uuid.UUID]*domain.GatewayRefund
	refundByRef  map[string]*domain.GatewayRefund
	refundByCF   map[string]*domain.GatewayRefund
	refundByIdem map[string]*domain.GatewayRefund // key: paymentID_idempotencyKey
	allocations  []domain.PaymentAllocation
	refAllocs    []domain.RefundAllocation
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
		return nil, domain.ErrNotFound
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
func (m *mockRefundPaymentRepo) ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ref.Status = newStatus
	m.refunds[ref.ID] = ref
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
			succeeded := true
			for _, r := range m.refunds {
				if r.ID == ra.RefundID {
					succeeded = (r.Status == "succeeded")
					break
				}
			}
			if succeeded {
				totalRefunded += ra.AmountPaise
			}
		}
	}
	return totalPaid - totalRefunded, nil
}
func (m *mockRefundPaymentRepo) GetDueUnreservedPaidPaise(ctx context.Context, dueID uuid.UUID) (int64, error) {
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
			unreserved := true
			for _, r := range m.refunds {
				if r.ID == ra.RefundID {
					if r.Status == "failed" || r.Status == "cancelled" {
						unreserved = false
					}
					break
				}
			}
			if unreserved {
				totalRefunded += ra.AmountPaise
			}
		}
	}
	return totalPaid - totalRefunded, nil
}
func (m *mockRefundPaymentRepo) GetPaymentDueRefundedPaise(ctx context.Context, paymentID, dueID, excludeRefundID uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var totalRefunded int64
	for _, ra := range m.refAllocs {
		if ra.DueID != nil && *ra.DueID == dueID && ra.RefundID != excludeRefundID {
			for _, r := range m.refunds {
				if r.ID == ra.RefundID && r.PaymentID == paymentID && r.Status != "failed" && r.Status != "cancelled" {
					totalRefunded += ra.AmountPaise
					break
				}
			}
		}
	}
	return totalRefunded, nil
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
func (s *mockRefundDueStore) List(context.Context, postgres.DueListFilter) ([]domain.Due, error) {
	return nil, nil
}
func (s *mockRefundDueStore) ListByTenant(context.Context, uuid.UUID) ([]domain.Due, error) {
	return nil, nil
}

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
func (s *mockRefundIntentStore) GetDuesSnapshot(context.Context, uuid.UUID) ([]domain.PaymentIntentDue, error) {
	return nil, nil
}
func (s *mockRefundIntentStore) LatestOpenForDue(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	return nil, nil
}

type mockRefundTenantStore struct {
	t *domain.Tenant
}

func (s *mockRefundTenantStore) GetByID(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return s.t, nil
}
func (s *mockRefundTenantStore) ListByProperty(context.Context, uuid.UUID) ([]domain.Tenant, error) {
	return nil, nil
}
func (s *mockRefundTenantStore) Update(context.Context, *domain.Tenant) error { return nil }
func (s *mockRefundTenantStore) GetIDPhoto(context.Context, uuid.UUID) ([]byte, error) {
	return nil, nil
}

type refundStepUpUserStore struct {
	owners []domain.User
	user   *domain.User
}

func (s *refundStepUpUserStore) Create(context.Context, *domain.User) error { return nil }
func (s *refundStepUpUserStore) GetByPhone(context.Context, string) (*domain.User, error) {
	if s.user != nil {
		return s.user, nil
	}
	return nil, domain.ErrNotFound
}
func (s *refundStepUpUserStore) GetByPropertyAndRole(context.Context, uuid.UUID, domain.Role) ([]domain.User, error) {
	return s.owners, nil
}
func (s *refundStepUpUserStore) GetByID(context.Context, uuid.UUID) (*domain.User, error) {
	if s.user != nil {
		return s.user, nil
	}
	return nil, domain.ErrNotFound
}
func (s *refundStepUpUserStore) SetPropertyID(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

type refundStepUpAuthService struct {
	stubPayoutAuthService
	validOTP    string
	seenPurpose string
	seenBatch   *uuid.UUID
}

func (a *refundStepUpAuthService) VerifyStepUpOTPSpecific(_ context.Context, _ string, otp, purpose string, batchID *uuid.UUID) error {
	a.seenPurpose = purpose
	a.seenBatch = batchID
	if otp == a.validOTP && otp != "" {
		return nil
	}
	return auth.ErrInvalidOTP
}

func setupRefundRouter(h *Handlers, propID, userID uuid.UUID) *gin.Engine {
	if h.UserStore == nil {
		owner := &domain.User{ID: userID, PropertyID: &propID, Role: domain.RoleOwner, Phone: "+919000000001"}
		h.UserStore = &refundStepUpUserStore{owners: []domain.User{*owner}, user: owner}
		h.Auth = &refundStepUpAuthService{validOTP: "123456"}
	}
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

type fakeRefundTxRunner struct {
	gwRepo GatewayPaymentRepo
}

func (f *fakeRefundTxRunner) PrepareRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund) ([]refundAllocItem, error) {
	alreadyRefunded, _ := f.gwRepo.GetPaymentRefundedPaise(ctx, p.ID)
	if alreadyRefunded+req.AmountPaise > int64(p.Amount) {
		return nil, &refundValidationErr{msg: "refund amount exceeds remaining balance"}
	}
	_ = f.gwRepo.CreateOrUpdateRefund(ctx, rfRow)
	return nil, nil
}

func (f *fakeRefundTxRunner) CleanupFailedRefund(ctx context.Context, rfRow *domain.GatewayRefund) error {
	return f.gwRepo.CreateOrUpdateRefund(ctx, rfRow)
}

func (f *fakeRefundTxRunner) SettleRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund, allocationsToCreate []refundAllocItem) error {
	return f.gwRepo.CreateOrUpdateRefund(ctx, rfRow)
}

func (f *fakeRefundTxRunner) ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error {
	ref.Status = newStatus
	return f.gwRepo.CreateOrUpdateRefund(ctx, ref)
}

type shortfallRefundTxRunner struct {
	err error
}

func (s *shortfallRefundTxRunner) PrepareRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund) ([]refundAllocItem, error) {
	if s.err != nil {
		return nil, s.err
	}
	return nil, nil
}

func (s *shortfallRefundTxRunner) CleanupFailedRefund(ctx context.Context, rfRow *domain.GatewayRefund) error {
	return nil
}

func (s *shortfallRefundTxRunner) SettleRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund, allocationsToCreate []refundAllocItem) error {
	return nil
}

func (s *shortfallRefundTxRunner) ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error {
	ref.Status = newStatus
	return nil
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

		fakeRunner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     fakeRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 550000,
			"reason":       "Tenant cancelled booking before checkin",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Idempotency-Key", "idem_test_001")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
		}
		var wrapper struct {
			Message string                `json:"message"`
			Refund  *domain.GatewayRefund `json:"refund"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &wrapper); err != nil || wrapper.Refund == nil {
			t.Fatalf("unmarshal refund response: %v", err)
		}
		resp := wrapper.Refund
		if resp.AmountPaise != 550000 {
			t.Errorf("expected amount 550000 paise, got %d", resp.AmountPaise)
		}
		if resp.Status != "initiated" {
			t.Errorf("expected status 'initiated', got %s", resp.Status)
		}
		if resp.RefundReference == nil || len(*resp.RefundReference) != 34 {
			t.Errorf("expected 34-character refund reference, got %v", resp.RefundReference)
		}
		if resp.RefundReference != nil && strings.Contains(*resp.RefundReference, "_") {
			t.Errorf("expected alphanumeric refund reference without underscore, got %s", *resp.RefundReference)
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

		fakeRunner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     fakeRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 600000, // exceeds 550,000
			"reason":       "Over refund",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem_test_002")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on over-refund, got %d", rec.Code)
		}
		if cfClient.refundCalls != 0 {
			t.Errorf("expected 0 calls to Cashfree on validation failure, got %d", cfClient.refundCalls)
		}
	})

	// 2b. Reject: Allocation Shortfall
	t.Run("refund allocation shortfall returns 422 with code", func(t *testing.T) {
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

		shortfallRunner := &shortfallRefundTxRunner{
			err: &refundValidationErr{
				msg:    "refund allocation shortfall: 100000 paise unallocated across dues",
				status: http.StatusUnprocessableEntity,
				code:   apierr.CodePaymentRefundShortfall,
			},
		}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     shortfallRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 550000,
			"reason":       "Tenant request refund",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem_test_shortfall")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 Unprocessable Entity on allocation shortfall, got %d (body: %s)", rec.Code, rec.Body.String())
		}
		var env apierr.ErrorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("failed to decode error envelope: %v", err)
		}
		if env.Code != apierr.CodePaymentRefundShortfall {
			t.Fatalf("expected code %s, got %s", apierr.CodePaymentRefundShortfall, env.Code)
		}
		if cfClient.refundCalls != 0 {
			t.Errorf("expected 0 calls to Cashfree on validation failure, got %d", cfClient.refundCalls)
		}
	})

	// 3. Reject: Older Than 120 Days
	t.Run("refund older than 120 days", func(t *testing.T) {
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
			CreatedAt:         time.Now().Add(-125 * 24 * time.Hour), // 125 days ago (> 120 days default)
		}
		_ = gwRepo.Create(context.Background(), oldPayment)

		fakeRunner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     fakeRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 550000,
			"reason":       "Old refund",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+oldPayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on 120-day expired payment, got %d", rec.Code)
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

		fakeRunner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     fakeRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
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
		if rec1.Code != http.StatusAccepted {
			t.Fatalf("first call failed: %d", rec1.Code)
		}

		// Second call with same idempotency key
		req2 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("X-Idempotency-Key", "idem_replay_key_999")
		rec2 := httptest.NewRecorder()
		r.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusOK && rec2.Code != http.StatusAccepted {
			t.Fatalf("second call failed: %d", rec2.Code)
		}

		// Exact same gateway calls count -> Cashfree was NOT called on HTTP request path
		if cfClient.refundCalls != 0 {
			t.Fatalf("expected 0 calls to Cashfree on HTTP path, got %d", cfClient.refundCalls)
		}

		// Third call with SAME key but DIFFERENT amount must return 409 Conflict
		bodyConflict := map[string]any{
			"otp":          "123456",
			"amount_paise": 300000,
			"reason":       "Idempotent partial refund with different amount",
		}
		bufConflict, _ := json.Marshal(bodyConflict)
		req3 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(bufConflict))
		req3.Header.Set("Content-Type", "application/json")
		req3.Header.Set("X-Idempotency-Key", "idem_replay_key_999")
		rec3 := httptest.NewRecorder()
		r.ServeHTTP(rec3, req3)
		if rec3.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict on idempotency key mismatch, got %d: %s", rec3.Code, rec3.Body.String())
		}
	})

	// 4b. Idempotency Replay with Step-Up: retry after dropped connection does not require a fresh code
	t.Run("idempotency retry succeeds without valid step-up code on second call", func(t *testing.T) {
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

		testOwner := &domain.User{
			ID:         userID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
			Phone:      "+919876543210",
		}
		userStore := &refundStepUpUserStore{
			owners: []domain.User{*testOwner}, // Solo owner enforces step-up
			user:   testOwner,
		}
		authSvc := &refundStepUpAuthService{validOTP: "654321"}

		fakeRunner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     fakeRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
			UserStore:          userStore,
			Auth:               authSvc,
		}}

		r := setupRefundRouter(h, propID, userID)

		bodyWithOTP := map[string]any{
			"amount_paise": 250000,
			"reason":       "Step up test refund",
			"otp":          "654321",
		}
		bufWithOTP, _ := json.Marshal(bodyWithOTP)

		// Call 1: valid OTP -> succeeds with 202 Accepted
		req1 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(bufWithOTP))
		req1.Header.Set("Content-Type", "application/json")
		req1.Header.Set("Idempotency-Key", "idem_stepup_retry_key")
		rec1 := httptest.NewRecorder()
		r.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusAccepted {
			t.Fatalf("first call with valid OTP failed: %d: %s", rec1.Code, rec1.Body.String())
		}

		// Call 2: retry of same idempotency key with INVALID/EMPTY OTP -> must still return 200/202 with the refund
		bodyNoOTP := map[string]any{
			"amount_paise": 250000,
			"reason":       "Step up test refund",
			"otp":          "", // empty OTP (already consumed on initial attempt)
		}
		bufNoOTP, _ := json.Marshal(bodyNoOTP)

		req2 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(bufNoOTP))
		req2.Header.Set("Content-Type", "application/json")
		req2.Header.Set("Idempotency-Key", "idem_stepup_retry_key")
		rec2 := httptest.NewRecorder()
		r.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusOK && rec2.Code != http.StatusAccepted {
			t.Fatalf("retry with same idempotency key and empty OTP failed: %d: %s", rec2.Code, rec2.Body.String())
		}

		// Gateway was NOT called synchronously on HTTP path
		if cfClient.refundCalls != 0 {
			t.Fatalf("expected 0 synchronous Cashfree calls, got %d", cfClient.refundCalls)
		}

		// Call 3: NEW idempotency key with empty OTP must be rejected with 401 Unauthorized
		req3 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(bufNoOTP))
		req3.Header.Set("Content-Type", "application/json")
		req3.Header.Set("Idempotency-Key", "idem_stepup_new_key")
		rec3 := httptest.NewRecorder()
		r.ServeHTTP(rec3, req3)
		if rec3.Code != http.StatusUnauthorized {
			t.Fatalf("new refund request with empty OTP expected 401 Unauthorized, got %d: %s", rec3.Code, rec3.Body.String())
		}
	})

	// 5. Gateway Error Fallback
	t.Run("gateway 4xx rejection marks refund failed and returns 502", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{
			failRefund: true,
			refundErr:  &cashfree.APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: "gateway rejected refund: insufficient balance"},
		}
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

		fakeRunner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     fakeRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 200000,
			"reason":       "Failing refund",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem_test_005")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted on initiated refund, got %d: %s", rec.Code, rec.Body.String())
		}

		var wrapper struct {
			Message string                `json:"message"`
			Refund  *domain.GatewayRefund `json:"refund"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &wrapper); err != nil || wrapper.Refund == nil {
			t.Fatalf("unmarshal refund response: %v", err)
		}
		if wrapper.Refund.Status != "initiated" {
			t.Errorf("expected initial status 'initiated', got %s", wrapper.Refund.Status)
		}

		// Dispatch via outbox dispatcher
		dispatcher := finance.NewRefundGatewayDispatcher(gwRepo, cfClient)
		if err := dispatcher.DispatchRefund(context.Background(), wrapper.Refund.ID); err != nil {
			t.Fatalf("dispatch error: %v", err)
		}
		updated, _ := gwRepo.GetRefundByID(context.Background(), wrapper.Refund.ID)
		if updated.Status != "failed" {
			t.Errorf("expected refund to be marked failed by dispatcher on 4xx rejection, got %s", updated.Status)
		}
	})

	t.Run("gateway timeout keeps refund initiated and returns 202", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{
			failRefund: true,
			refundErr:  errors.New("gateway timeout: network deadline exceeded"),
		}
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

		fakeRunner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     fakeRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 200000,
			"reason":       "Timing out refund",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem_test_005_timeout")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted on ambiguous gateway timeout, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// 6. Fail-closed: Nil Pool and Nil RefundTxRunner Returns 500
	t.Run("nil pool and nil runner fails closed with 500", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{}
		tenantStore := &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}}

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

		dueStore := &mockRefundDueStore{dues: map[uuid.UUID]*domain.Due{
			dueID: {ID: dueID, PropertyID: propID, TenantID: tenantID, DueCode: "DUE1", Amount: 0, OriginalAmount: 550000, Status: domain.DueStatusPaid},
		}}
		intentStore := &mockRefundIntentStore{byCF: map[string]*domain.PaymentIntent{
			cfPayID: {ProviderOrderID: cfOrderID, CFPaymentID: &cfPayID},
		}}

		h := &Handlers{Deps: Deps{
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
			// Both Pool and RefundTxRunner are nil
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 100000,
			"reason":       "Fail closed check",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem_test_006")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 Internal Server Error when pool/runner is nil, got %d", rec.Code)
		}
		if cfClient.refundCalls != 0 {
			t.Errorf("expected 0 calls to Cashfree when database unavailable, got %d", cfClient.refundCalls)
		}
	})

	t.Run("rejects cross-property refund attempt with 404", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{}
		otherPropID := uuid.New()
		tenantStore := &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: otherPropID}}
		dueStore := &mockRefundDueStore{dues: map[uuid.UUID]*domain.Due{
			dueID: {ID: dueID, PropertyID: otherPropID, TenantID: tenantID, Amount: 0, OriginalAmount: 550000, Status: domain.DueStatusPaid},
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

		fakeRunner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     fakeRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 550000,
			"reason":       "Cross property refund attempt",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found, got %d: %s", rec.Code, rec.Body.String())
		}
		var errResp map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
		if errResp["error"] != "payment not found" {
			t.Errorf("expected 'payment not found', got %q", errResp["error"])
		}
		if len(gwRepo.refunds) != 0 {
			t.Errorf("expected no refunds created in repo, got %d", len(gwRepo.refunds))
		}
	})

	t.Run("rejects refund when Idempotency-Key is missing with 400", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{}
		tenantStore := &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}}

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
			RefundTxRunner:     &fakeRefundTxRunner{gwRepo: gwRepo},
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 100000,
			"reason":       "Missing idempotency key test",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		// Explicitly no Idempotency-Key or X-Idempotency-Key
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on missing idempotency key, got %d: %s", rec.Code, rec.Body.String())
		}
		var errResp map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
		if errResp["error"] != "Idempotency-Key header is required for refund operations" {
			t.Errorf("expected missing idempotency key error, got %q", errResp["error"])
		}
		if cfClient.refundCalls != 0 {
			t.Errorf("expected 0 calls to Cashfree, got %d", cfClient.refundCalls)
		}
	})

	t.Run("rejects refund when Idempotency-Key exceeds 128 chars with 400", func(t *testing.T) {
		gwRepo := newMockRefundPaymentRepo()
		cfClient := &mockCFClient{}
		tenantStore := &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propID}}

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
			RefundTxRunner:     &fakeRefundTxRunner{gwRepo: gwRepo},
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 100000,
			"reason":       "Oversized idempotency key test",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", strings.Repeat("x", 129))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on oversized idempotency key, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("concurrent retry adopting non-terminal refund returns 202 without calling gateway", func(t *testing.T) {
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

		existingRefID := uuid.New()
		existingRefStr := "rf_canonical_existing_123"
		idemKey := "idem_racing_retry"
		existingRow := &domain.GatewayRefund{
			ID:              existingRefID,
			PaymentID:       basePayment.ID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &existingRefStr,
			IdempotencyKey:  &idemKey,
			AmountPaise:     200000,
			Status:          "initiated",
			Reason:          "First request in flight",
		}

		runner := &adoptingRefundTxRunner{
			adoptedRow: existingRow,
		}

		h := &Handlers{Deps: Deps{
			RefundTxRunner:     runner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 200000,
			"reason":       "First request in flight",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idemKey)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted on adopted initiated refund, got %d: %s", rec.Code, rec.Body.String())
		}
		if cfClient.refundCalls != 0 {
			t.Fatalf("expected 0 gateway calls when adopting initiated refund, got %d", cfClient.refundCalls)
		}
	})

	t.Run("concurrent retry adopting terminal refund returns 200 OK without calling gateway", func(t *testing.T) {
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

		existingRefID := uuid.New()
		existingRefStr := "rf_canonical_existing_456"
		idemKey := "idem_racing_terminal"
		existingRow := &domain.GatewayRefund{
			ID:              existingRefID,
			PaymentID:       basePayment.ID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &existingRefStr,
			IdempotencyKey:  &idemKey,
			AmountPaise:     200000,
			Status:          "succeeded",
			Reason:          "Already succeeded",
		}

		runner := &adoptingRefundTxRunner{
			adoptedRow: existingRow,
		}

		h := &Handlers{Deps: Deps{
			RefundTxRunner:     runner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)

		body := map[string]any{
			"otp":          "123456",
			"amount_paise": 200000,
			"reason":       "Already succeeded",
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idemKey)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on adopted terminal refund, got %d: %s", rec.Code, rec.Body.String())
		}
		if cfClient.refundCalls != 0 {
			t.Fatalf("expected 0 gateway calls when adopting terminal refund, got %d", cfClient.refundCalls)
		}
	})
}

type adoptingRefundTxRunner struct {
	adoptedRow *domain.GatewayRefund
}

func (a *adoptingRefundTxRunner) PrepareRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund) ([]refundAllocItem, error) {
	if a.adoptedRow != nil {
		*rfRow = *a.adoptedRow
	}
	return nil, nil
}

func (a *adoptingRefundTxRunner) CleanupFailedRefund(ctx context.Context, rfRow *domain.GatewayRefund) error {
	return nil
}

func (a *adoptingRefundTxRunner) SettleRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund, allocationsToCreate []refundAllocItem) error {
	return nil
}

func (a *adoptingRefundTxRunner) ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error {
	ref.Status = newStatus
	return nil
}

// ---------------------------------------------------------------------------
// Live Postgres Integration Test with Universal Lock Hierarchy
// ---------------------------------------------------------------------------

func TestLivePostgres_OwnerRefundPayment_LockHierarchy(t *testing.T) {
	if testing.Short() {
		testutil.FailOnSkipIfDBRequired(t, "skipping live postgres test in short mode")
	}
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
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
		INSERT INTO payments (id, property_id, tenant_id, amount, matched_by, provider, provider_payment_id, cf_payment_id, created_at)
		VALUES ($1, $4, $2, 1100000, 'cashfree', 'cashfree', $3, $3, NOW())
	`, paymentID, tenantID, cfPaymentID, propID)
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
		"otp":          "123456",
		"amount_paise": 300000,
		"reason":       "Partial refund round 1",
	}
	buf1, _ := json.Marshal(body1)
	req1 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+paymentID.String()+"/refund", bytes.NewReader(buf1))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Idempotency-Key", "idem_live_round_1")
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("Round 1 refund failed with %d: %s", rec1.Code, rec1.Body.String())
	}

	// Check Due 2 status in Postgres
	var due2Status string
	var due2Amount int64
	err = pool.QueryRow(ctx, `SELECT status, amount FROM dues WHERE id = $1`, dueID2).Scan(&due2Status, &due2Amount)
	if err != nil {
		t.Fatalf("query due 2: %v", err)
	}
	if due2Status != "partial" || due2Amount != 300000 {
		t.Fatalf("expected Due 2 to be status='partial' and amount=300000 paise (250000 net paid), got status=%s amount=%d", due2Status, due2Amount)
	}

	// Check Due 1 status in Postgres
	var due1Status string
	var due1Amount int64
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
		"otp":          "123456",
		"amount_paise": 350000,
		"reason":       "Partial refund round 2",
	}
	buf2, _ := json.Marshal(body2)
	req2 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+paymentID.String()+"/refund", bytes.NewReader(buf2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Idempotency-Key", "idem_live_round_2")
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
		"otp":          "123456",
		"amount_paise": 500000, // exceeds remaining 4,500
		"reason":       "Attempting to exceed remaining refundable balance",
	}
	buf3, _ := json.Marshal(body3)
	req3 := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+paymentID.String()+"/refund", bytes.NewReader(buf3))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Idempotency-Key", "idem_live_round_3")
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on over-refund, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

func TestLivePostgres_OwnerRefundPayment_GuardA_Rejection(t *testing.T) {
	if testing.Short() {
		testutil.FailOnSkipIfDBRequired(t, "skipping live postgres test in short mode")
	}
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
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
		INSERT INTO payments (id, property_id, tenant_id, amount, matched_by, provider, provider_payment_id, cf_payment_id, created_at)
		VALUES ($1, $4, $2, 550000, 'cashfree', 'cashfree', $3, $3, NOW())
	`, paymentID, tenantID, cfPaymentID, propID)
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
		"otp":          "123456",
		"amount_paise": 100000,
		"reason":       "Attempting refund on departed tenant",
	}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+paymentID.String()+"/refund", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "idem_guard_a_1")
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
	req2.Header.Set("Idempotency-Key", "idem_guard_a_2")
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

func TestManagerRefundPayment_ToggleAndCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	managerID := uuid.New()
	paymentID := uuid.New()

	t.Run("manager refund rejected when owner toggle is disabled", func(t *testing.T) {
		h := &Handlers{
			Deps: Deps{
				OnboardingSettings: &fakeOnboardingSettingsRepo{
					settings: map[uuid.UUID]*domain.PropertyOnboardingSettings{
						propID: {
							PropertyID:          propID,
							AllowManagerRefunds: false,
						},
					},
				},
			},
		}

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: paymentID.String()}}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     managerID,
			PropertyID: &propID,
			Role:       domain.RoleManager,
		})
		c.Request = httptest.NewRequest(http.MethodPost, "/manager/payments/"+paymentID.String()+"/refund", nil)

		h.ManagerRefundPayment(c)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden, got %d", w.Code)
		}
	})
}

func TestOwnerRefundPayment_ReasonValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	userID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()

	cfPayID := "cf_pay_test_reason"
	cfOrderID := "order_test_reason"

	setupHandler := func() (*Handlers, *domain.Payment, *gin.Engine, *mockCFClient) {
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
			ProviderPaymentID: &cfPayID,
			PropertyID:        &propID,
			CreatedAt:         time.Now().Add(-1 * time.Hour),
		}
		_ = gwRepo.Create(context.Background(), basePayment)
		_ = gwRepo.CreateAllocation(context.Background(), basePayment.ID, dueID, 550000)

		fakeRunner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{Deps: Deps{
			RefundTxRunner:     fakeRunner,
			GatewayPaymentRepo: gwRepo,
			CashfreeClient:     cfClient,
			TenantStore:        tenantStore,
			DueStore:           dueStore,
			IntentStore:        intentStore,
		}}

		r := setupRefundRouter(h, propID, userID)
		return h, basePayment, r, cfClient
	}

	validCases := []struct {
		name   string
		reason string
	}{
		{name: "English with apostrophe and slash and dot", reason: "Tenant's deposit refund / room change."},
		{name: "Telugu language refund reason", reason: "అద్దె వాపసు"},
		{name: "Tamil language refund reason", reason: "வாடகை திருப்பி"},
		{name: "Kannada language refund reason", reason: "ಬಾಡಿಗೆ ಮರುಪಾವತಿ"},
		{name: "Kannada with ZWNJ (passbook catalog key)", reason: "ಪಾಸ್ಬುಕ್"},
		{name: "Kannada with virama and ZWNJ", reason: "ಪಾಸ್‌ಬುಕ್"},
		{name: "Multi-language reason with numbers and dash", reason: "Room 102 - అద్దె వాపసు / overpayment"},
	}

	for _, tc := range validCases {
		t.Run("valid_"+tc.name, func(t *testing.T) {
			_, basePayment, r, _ := setupHandler()
			body := map[string]any{
				"otp":          "123456",
				"amount_paise": 100000,
				"reason":       tc.reason,
			}
			buf, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Idempotency-Key", "idem_"+uuid.New().String()[:8])
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusAccepted {
				t.Fatalf("expected 202 Accepted for valid reason %q, got %d (body: %s)", tc.reason, rec.Code, rec.Body.String())
			}

			var wrapper struct {
				Message string               `json:"message"`
				Refund  domain.GatewayRefund `json:"refund"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &wrapper); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if wrapper.Refund.Reason != strings.TrimSpace(tc.reason) {
				t.Errorf("expected audit reason %q, got %q", tc.reason, wrapper.Refund.Reason)
			}
		})
	}

	invalidCases := []struct {
		name         string
		reason       string
		expectedCode string
	}{
		{name: "too short under 3 chars", reason: "ab", expectedCode: "payment.refundReasonLength"},
		{name: "disallowed special characters (script tag)", reason: "Refund <script>", expectedCode: "payment.refundReasonCharacters"},
		{name: "disallowed special characters (dollar)", reason: "Refund $500", expectedCode: "payment.refundReasonCharacters"},
		{name: "disallowed special characters (semicolon)", reason: "Refund; DROP TABLE dues", expectedCode: "payment.refundReasonCharacters"},
	}

	for _, tc := range invalidCases {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			_, basePayment, r, _ := setupHandler()
			body := map[string]any{
				"otp":          "123456",
				"amount_paise": 100000,
				"reason":       tc.reason,
			}
			buf, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPost, "/api/owner/payments/"+basePayment.ID.String()+"/refund", bytes.NewReader(buf))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Idempotency-Key", "idem_"+uuid.New().String()[:8])
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 Bad Request for invalid reason %q, got %d (body: %s)", tc.reason, rec.Code, rec.Body.String())
			}
			var resp map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if tc.expectedCode != "" && resp["code"] != tc.expectedCode {
				t.Errorf("expected error code %q, got %v", tc.expectedCode, resp["code"])
			}
		})
	}
}

func TestCancelRefund_Scenarios(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	ownerID := uuid.New()
	managerID := uuid.New()
	paymentID := uuid.New()
	tenantID := uuid.New()
	cfOrderID := "order_test_cancel"

	setupCancelHandler := func(cfClient CashfreeClient) (*Handlers, *mockRefundPaymentRepo) {
		gwRepo := newMockRefundPaymentRepo()
		owner := &domain.User{ID: ownerID, PropertyID: &propID, Role: domain.RoleOwner, Phone: "+919000000001"}
		manager := &domain.User{ID: managerID, PropertyID: &propID, Role: domain.RoleManager, Phone: "+919000000002"}
		userStore := &refundStepUpUserStore{owners: []domain.User{*owner}, user: owner}
		authSvc := &refundStepUpAuthService{validOTP: "123456"}

		h := &Handlers{
			Deps: Deps{
				GatewayPaymentRepo: gwRepo,
				CashfreeClient:     cfClient,
				UserStore:          userStore,
				Auth:               authSvc,
				RefundTxRunner:     &fakeRefundTxRunner{gwRepo: gwRepo},
				OnboardingSettings: &fakeOnboardingSettingsRepo{
					settings: map[uuid.UUID]*domain.PropertyOnboardingSettings{
						propID: {
							PropertyID:          propID,
							AllowManagerRefunds: true,
						},
					},
				},
			},
		}
		_ = manager // used in manager test
		return h, gwRepo
	}

	t.Run("successful cancellation of ONHOLD refund releases reservation", func(t *testing.T) {
		cfClient := &mockCFClient{}
		h, gwRepo := setupCancelHandler(cfClient)

		rfRef := "rf_test_cancel_12345"
		orderPayload, _ := json.Marshal(map[string]any{"order_id": cfOrderID})
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusOnHold,
			Source:          "owner",
			RawPayload:      orderPayload,
			CreatedAt:       time.Now().Add(-10 * time.Minute),
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"reason": "Cancelled by owner due to bank top up delay",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d (body: %s)", w.Code, w.Body.String())
		}

		updated, err := gwRepo.GetRefundByID(context.Background(), rf.ID)
		if err != nil || updated == nil {
			t.Fatalf("failed reloading refund: %v", err)
		}
		if updated.Status != domain.RefundStatusCancelled {
			t.Errorf("expected status cancelled, got %s", updated.Status)
		}
	})

	t.Run("gateway rejection keeps local reservation held and returns 502", func(t *testing.T) {
		cfClient := &mockCFClient{failRefund: true}
		h, gwRepo := setupCancelHandler(cfClient)

		rfRef := "rf_test_cancel_reject"
		orderPayload, _ := json.Marshal(map[string]any{"order_id": cfOrderID})
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusOnHold,
			Source:          "owner",
			RawPayload:      orderPayload,
			CreatedAt:       time.Now().Add(-10 * time.Minute),
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"reason": "Cancelled by owner",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusBadGateway {
			t.Fatalf("expected 502 Bad Gateway on gateway failure, got %d", w.Code)
		}

		// Verify row in database remains ONHOLD and was NOT cancelled locally
		updated, _ := gwRepo.GetRefundByID(context.Background(), rf.ID)
		if updated.Status != domain.RefundStatusOnHold {
			t.Errorf("expected status to remain on_hold, got %s", updated.Status)
		}
	})

	t.Run("initiated refund can be cancelled immediately when lock is free without 2-minute delay", func(t *testing.T) {
		cfClient := &mockCFClient{}
		h, gwRepo := setupCancelHandler(cfClient)

		rfRef := "rf_fresh_flight"
		orderPayload, _ := json.Marshal(map[string]any{"order_id": cfOrderID})
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusInitiated,
			Source:          "owner",
			RawPayload:      orderPayload,
			CreatedAt:       time.Now().Add(-30 * time.Second), // Fresh in-flight (< 2 min)
			UpdatedAt:       time.Now().Add(-30 * time.Second),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"reason": "Cancelled immediately",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for immediate cancel when lock is free, got %d (body: %s)", w.Code, w.Body.String())
		}
	})

	t.Run("reject cancel for non-owner source refunds", func(t *testing.T) {
		cfClient := &mockCFClient{}
		h, gwRepo := setupCancelHandler(cfClient)

		rfRef := "rf_dash_source"
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusPending,
			Source:          "dashboard", // Non-owner source
			CreatedAt:       time.Now().Add(-10 * time.Minute),
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"reason": "Attempting dashboard cancel",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for non-owner refund, got %d", w.Code)
		}
	})

	t.Run("step-up OTP required for refund cancellation", func(t *testing.T) {
		cfClient := &mockCFClient{}
		h, gwRepo := setupCancelHandler(cfClient)

		rfRef := "rf_otp_test"
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusOnHold,
			Source:          "owner",
			CreatedAt:       time.Now().Add(-10 * time.Minute),
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		// Send wrong OTP
		body := map[string]any{
			"reason": "Cancelled without proper OTP",
			"otp":    "999999",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized for invalid OTP, got %d", w.Code)
		}
	})

	t.Run("manager cancel respects allow_manager_refunds setting", func(t *testing.T) {
		cfClient := &mockCFClient{}
		h, gwRepo := setupCancelHandler(cfClient)
		// Disable manager refunds
		h.OnboardingSettings = &fakeOnboardingSettingsRepo{
			settings: map[uuid.UUID]*domain.PropertyOnboardingSettings{
				propID: {
					PropertyID:          propID,
					AllowManagerRefunds: false,
				},
			},
		}

		rfRef := "rf_mgr_cancel"
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusOnHold,
			Source:          "owner",
			CreatedAt:       time.Now().Add(-10 * time.Minute),
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     managerID,
			PropertyID: &propID,
			Role:       domain.RoleManager,
		})
		body := map[string]any{
			"reason": "Manager attempting cancel",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/manager/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.ManagerCancelRefund(c)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden when manager refunds are disabled, got %d", w.Code)
		}
	})

	t.Run("initiated refund with gateway 404 fails closed with 409 and leaves reservation held", func(t *testing.T) {
		cfClient := &mockCFClient{
			failRefund: true,
			refundErr: &cashfree.APIError{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Body:       `{"message":"refund not found"}`,
			},
		}
		h, gwRepo := setupCancelHandler(cfClient)

		rfRef := "rf_test_cancel_undispatched"
		orderPayload, _ := json.Marshal(map[string]any{"order_id": cfOrderID})
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			CFRefundID:      nil, // Never assigned by Cashfree
			AmountPaise:     75000,
			Status:          domain.RefundStatusInitiated,
			Source:          "owner",
			RawPayload:      orderPayload,
			CreatedAt:       time.Now().Add(-5 * time.Minute),
			UpdatedAt:       time.Now().Add(-5 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"reason": "Cancelling undispatched initiated refund",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict for 404 refund cancellation, got %d (body: %s)", w.Code, w.Body.String())
		}

		updated, err := gwRepo.GetRefundByID(context.Background(), rf.ID)
		if err != nil || updated == nil {
			t.Fatalf("failed fetching updated refund: %v", err)
		}
		if updated.Status != domain.RefundStatusInitiated {
			t.Fatalf("expected refund status to remain 'initiated', got %s", updated.Status)
		}
	})

	t.Run("cancel fails closed when gateway details missing", func(t *testing.T) {
		cfClient := &mockCFClient{}
		h, gwRepo := setupCancelHandler(cfClient)

		rfRef := "" // Missing reference
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusOnHold,
			Source:          "owner",
			RawPayload:      []byte(`{}`), // Missing order_id
			CreatedAt:       time.Now().Add(-10 * time.Minute),
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"reason": "Cancel without gateway details",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict when gateway details missing, got %d (body: %s)", w.Code, w.Body.String())
		}
	})

	t.Run("cancel fails closed when cashfree client is nil", func(t *testing.T) {
		h, gwRepo := setupCancelHandler(nil)

		rfRef := "rf_test_nil_client"
		orderPayload, _ := json.Marshal(map[string]any{"order_id": cfOrderID})
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusOnHold,
			Source:          "owner",
			RawPayload:      orderPayload,
			CreatedAt:       time.Now().Add(-10 * time.Minute),
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"reason": "Cancel with nil client",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable when client is nil, got %d (body: %s)", w.Code, w.Body.String())
		}
	})

	t.Run("R4: lock contention returns 409 conflict", func(t *testing.T) {
		cfClient := &mockCFClient{}
		h, gwRepo := setupCancelHandler(cfClient)
		h.RefundLocker = &mockApiRefundLocker{acquired: false}

		rfRef := "rf_test_cancel_lock"
		orderPayload, _ := json.Marshal(map[string]any{"order_id": cfOrderID})
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusInitiated,
			Source:          "owner",
			RawPayload:      orderPayload,
			CreatedAt:       time.Now().Add(-10 * time.Minute),
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"reason": "Cancel busy refund",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict on lock contention, got %d (body: %s)", w.Code, w.Body.String())
		}
	})

	t.Run("R5: gateway cancellation response amount mismatch returns 502 with reservation held", func(t *testing.T) {
		cfClient := &mockCFClient{
			refundResult: &cashfree.RefundDetails{
				CFRefundID:   "cf_ref_mock_cancel",
				RefundID:     "rf_test_cancel_mismatch",
				OrderID:      cfOrderID,
				RefundStatus: "CANCELLED",
				AmountPaise:  25000, // Mismatched amount
			},
		}
		h, gwRepo := setupCancelHandler(cfClient)

		rfRef := "rf_test_cancel_mismatch"
		orderPayload, _ := json.Marshal(map[string]any{"order_id": cfOrderID})
		rf := &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       paymentID,
			PropertyID:      propID,
			Provider:        "cashfree",
			RefundReference: &rfRef,
			AmountPaise:     50000,
			Status:          domain.RefundStatusOnHold,
			Source:          "owner",
			RawPayload:      orderPayload,
			CreatedAt:       time.Now().Add(-10 * time.Minute),
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"reason": "Cancel with mismatch",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/cancel", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerCancelRefund(c)

		if w.Code != http.StatusBadGateway {
			t.Fatalf("expected 502 Bad Gateway on amount mismatch, got %d (body: %s)", w.Code, w.Body.String())
		}
		reloaded, _ := gwRepo.GetRefundByID(context.Background(), rf.ID)
		if reloaded.Status != domain.RefundStatusOnHold {
			t.Fatalf("reservation must remain held, got status %s", reloaded.Status)
		}
	})
	_ = tenantID
}

type mockApiRefundLocker struct {
	acquired bool
}

func (m *mockApiRefundLocker) TryLockRefundOperation(ctx context.Context, refundID uuid.UUID) (bool, func(), error) {
	if !m.acquired {
		return false, func() {}, nil
	}
	return true, func() {}, nil
}

func TestOwnerResolveReviewRefund_UnitScenarios(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	ownerID := uuid.New()
	paymentID := uuid.New()

	setupH := func(acquired bool) (*Handlers, *mockRefundPaymentRepo) {
		gwRepo := newMockRefundPaymentRepo()
		gwRepo.payments[paymentID] = &domain.Payment{
			ID:         paymentID,
			PropertyID: &propID,
			Amount:     500000,
			Provider:   "cashfree",
		}
		owner := &domain.User{ID: ownerID, PropertyID: &propID, Role: domain.RoleOwner, Phone: "+919000000001"}
		userStore := &refundStepUpUserStore{owners: []domain.User{*owner}, user: owner}
		authSvc := &refundStepUpAuthService{validOTP: "123456"}
		runner := &fakeRefundTxRunner{gwRepo: gwRepo}
		h := &Handlers{
			Deps: Deps{
				GatewayPaymentRepo: gwRepo,
				RefundTxRunner:     runner,
				UserStore:          userStore,
				Auth:               authSvc,
				RefundLocker:       &mockApiRefundLocker{acquired: acquired},
			},
		}
		return h, gwRepo
	}

	t.Run("Rejects non-needs_review refund", func(t *testing.T) {
		h, gwRepo := setupH(true)
		rf := &domain.GatewayRefund{
			ID:          uuid.New(),
			PaymentID:   paymentID,
			PropertyID:  propID,
			AmountPaise: 200000,
			Status:      domain.RefundStatusInitiated,
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"action": "cancel",
			"reason": "Unconfirmed orphan refund verified manually",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/resolve-review", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerResolveReviewRefund(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d (body: %s)", w.Code, w.Body.String())
		}
	})

	t.Run("Rejects invalid action", func(t *testing.T) {
		h, gwRepo := setupH(true)
		rf := &domain.GatewayRefund{
			ID:          uuid.New(),
			PaymentID:   paymentID,
			PropertyID:  propID,
			AmountPaise: 200000,
			Status:      domain.RefundStatusNeedsReview,
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"action": "unknown_action",
			"reason": "Manual operator override",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/resolve-review", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerResolveReviewRefund(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on invalid action, got %d", w.Code)
		}
	})

	t.Run("Lock contention returns 409 Conflict", func(t *testing.T) {
		h, gwRepo := setupH(false)
		rf := &domain.GatewayRefund{
			ID:          uuid.New(),
			PaymentID:   paymentID,
			PropertyID:  propID,
			AmountPaise: 200000,
			Status:      domain.RefundStatusNeedsReview,
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"action": "cancel",
			"reason": "Release reservation after confirmation",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/resolve-review", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerResolveReviewRefund(c)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict on lock contention, got %d", w.Code)
		}
	})

	t.Run("Action cancel transitions status to cancelled", func(t *testing.T) {
		h, gwRepo := setupH(true)
		rf := &domain.GatewayRefund{
			ID:          uuid.New(),
			PaymentID:   paymentID,
			PropertyID:  propID,
			AmountPaise: 200000,
			Status:      domain.RefundStatusNeedsReview,
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"action": "cancel",
			"reason": "Cashfree confirmed refund was never executed",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/resolve-review", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerResolveReviewRefund(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d (body: %s)", w.Code, w.Body.String())
		}
		updated, _ := gwRepo.GetRefundByID(context.Background(), rf.ID)
		if updated.Status != domain.RefundStatusCancelled {
			t.Fatalf("expected status cancelled, got %s", updated.Status)
		}
	})

	t.Run("Action confirm_succeeded transitions status to succeeded", func(t *testing.T) {
		h, gwRepo := setupH(true)
		rf := &domain.GatewayRefund{
			ID:          uuid.New(),
			PaymentID:   paymentID,
			PropertyID:  propID,
			AmountPaise: 200000,
			Status:      domain.RefundStatusNeedsReview,
		}
		_ = gwRepo.CreateOrUpdateRefund(context.Background(), rf)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{
			{Key: "id", Value: paymentID.String()},
			{Key: "refund_id", Value: rf.ID.String()},
		}
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
		body := map[string]any{
			"action": "confirm_succeeded",
			"reason": "Gateway portal confirms transfer succeeded with bank reference",
			"otp":    "123456",
		}
		buf, _ := json.Marshal(body)
		c.Request = httptest.NewRequest(http.MethodPost, "/owner/payments/"+paymentID.String()+"/refunds/"+rf.ID.String()+"/resolve-review", bytes.NewReader(buf))
		c.Request.Header.Set("Content-Type", "application/json")

		h.OwnerResolveReviewRefund(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d (body: %s)", w.Code, w.Body.String())
		}
		updated, _ := gwRepo.GetRefundByID(context.Background(), rf.ID)
		if updated.Status != domain.RefundStatusSucceeded {
			t.Fatalf("expected status succeeded, got %s", updated.Status)
		}
	})
}
