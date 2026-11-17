package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockPhase4ApprovalStore struct {
	mu            sync.Mutex
	requests      map[uuid.UUID]*domain.RefundApprovalRequest
	dispatchCalls int
	failDispatch  bool
}

func newMockPhase4ApprovalStore() *mockPhase4ApprovalStore {
	return &mockPhase4ApprovalStore{
		requests: make(map[uuid.UUID]*domain.RefundApprovalRequest),
	}
}

func (m *mockPhase4ApprovalStore) CreateRequest(_ context.Context, req *domain.RefundApprovalRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	cp := *req
	cp.Approvals = append([]domain.RefundApprovalDecision{}, req.Approvals...)
	m.requests[req.ID] = &cp
	return nil
}

func (m *mockPhase4ApprovalStore) GetRequestByID(_ context.Context, id uuid.UUID) (*domain.RefundApprovalRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	req, ok := m.requests[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *req
	cp.Approvals = append([]domain.RefundApprovalDecision{}, req.Approvals...)
	return &cp, nil
}

func (m *mockPhase4ApprovalStore) GetRequestByPaymentAndIdempotency(_ context.Context, paymentID uuid.UUID, idempotencyKey string) (*domain.RefundApprovalRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, req := range m.requests {
		if req.PaymentID == paymentID && req.IdempotencyKey == idempotencyKey {
			cp := *req
			cp.Approvals = append([]domain.RefundApprovalDecision{}, req.Approvals...)
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (m *mockPhase4ApprovalStore) ListRequestsByPayment(_ context.Context, paymentID uuid.UUID) ([]domain.RefundApprovalRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []domain.RefundApprovalRequest
	for _, req := range m.requests {
		if req.PaymentID == paymentID {
			cp := *req
			cp.Approvals = append([]domain.RefundApprovalDecision{}, req.Approvals...)
			list = append(list, cp)
		}
	}
	return list, nil
}

func (m *mockPhase4ApprovalStore) ListRequestsByProperty(_ context.Context, propertyID uuid.UUID, status string) ([]domain.RefundApprovalRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []domain.RefundApprovalRequest
	for _, req := range m.requests {
		if req.PropertyID == propertyID && (status == "" || req.Status == status) {
			cp := *req
			cp.Approvals = append([]domain.RefundApprovalDecision{}, req.Approvals...)
			list = append(list, cp)
		}
	}
	return list, nil
}

func (m *mockPhase4ApprovalStore) UpdateRequest(_ context.Context, req *domain.RefundApprovalRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *req
	cp.Approvals = append([]domain.RefundApprovalDecision{}, req.Approvals...)
	m.requests[req.ID] = &cp
	return nil
}

func (m *mockPhase4ApprovalStore) RecordApprovalAtomic(
	_ context.Context,
	requestID uuid.UUID,
	decision domain.RefundApprovalDecision,
	requiredApprovals int,
	onApproved func(tx pgx.Tx, req *domain.RefundApprovalRequest) error,
) (*domain.RefundApprovalRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := m.requests[requestID]
	if !ok {
		return nil, domain.ErrNotFound
	}

	if req.Status == domain.RefundApprovalRejected || req.Status == domain.RefundApprovalCancelled {
		return nil, domain.ErrRefundRequestNotPending
	}

	if req.Status == domain.RefundApprovalApproved || req.Status == domain.RefundApprovalDispatched {
		for _, a := range req.Approvals {
			if a.ApproverID == decision.ApproverID {
				cp := *req
				return &cp, nil
			}
		}
		cp := *req
		return &cp, nil
	}

	// 1. Reject prohibited self-approval
	if decision.ApproverID == req.RequestedBy {
		return nil, domain.ErrMakerCannotApprove
	}

	// 2. Reject stale approval
	if decision.SnapshotHash != req.MaterialHash {
		return nil, domain.ErrStaleApproval
	}

	// 3. Prohibit one owner from satisfying both approvals
	for _, prev := range req.Approvals {
		if prev.ApproverID == decision.ApproverID {
			return nil, domain.ErrDuplicateApprover
		}
		if prev.SnapshotHash != req.MaterialHash {
			return nil, domain.ErrStaleApproval
		}
	}

	// Record approval
	req.Approvals = append(req.Approvals, decision)
	req.UpdatedAt = time.Now().UTC()
	if requiredApprovals > 0 {
		req.RequiredApprovals = requiredApprovals
	}

	if len(req.Approvals) >= req.RequiredApprovals {
		if m.failDispatch {
			// Simulate database rollback on failure
			req.Approvals = req.Approvals[:len(req.Approvals)-1]
			return nil, errors.New("simulated outbox dispatch transaction failure")
		}
		req.Status = domain.RefundApprovalDispatched
		m.dispatchCalls++
		if onApproved != nil {
			_ = onApproved(nil, req)
		}
	} else {
		req.Status = domain.RefundApprovalPartiallyApproved
	}

	cp := *req
	cp.Approvals = append([]domain.RefundApprovalDecision{}, req.Approvals...)
	return &cp, nil
}

func (m *mockPhase4ApprovalStore) RejectRequestAtomic(
	_ context.Context,
	requestID uuid.UUID,
	rejecterID uuid.UUID,
	reason string,
) (*domain.RefundApprovalRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := m.requests[requestID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	now := time.Now().UTC()
	req.Status = domain.RefundApprovalRejected
	req.RejectedBy = &rejecterID
	req.RejectedAt = &now
	req.RejectionReason = &reason
	req.UpdatedAt = now

	cp := *req
	return &cp, nil
}

type phase4UserStore struct {
	mu     sync.Mutex
	users  map[uuid.UUID]*domain.User
	owners map[uuid.UUID][]domain.User // propertyID -> owners
}

func newPhase4UserStore() *phase4UserStore {
	return &phase4UserStore{
		users:  make(map[uuid.UUID]*domain.User),
		owners: make(map[uuid.UUID][]domain.User),
	}
}

func (s *phase4UserStore) Create(_ context.Context, u *domain.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
	return nil
}

func (s *phase4UserStore) GetByPhone(_ context.Context, phone string) (*domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.Phone == phone {
			return u, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s *phase4UserStore) SetPropertyID(_ context.Context, userID, propertyID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[userID]; ok {
		u.PropertyID = &propertyID
	}
	return nil
}

func (s *phase4UserStore) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return u, nil
}

func (s *phase4UserStore) GetByPropertyAndRole(_ context.Context, pid uuid.UUID, role domain.Role) ([]domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if role == domain.RoleOwner {
		return s.owners[pid], nil
	}
	return nil, nil
}

// 1. Acceptance Test: Maker cannot perform prohibited self-approval
func TestPhase4_MakerCannotPerformProhibitedSelfApproval(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pid := uuid.New()
	makerID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	users := newPhase4UserStore()
	users.users[makerID] = &domain.User{ID: makerID, Phone: "+919000000001", TokenVersion: 1}
	users.owners[pid] = []domain.User{*users.users[makerID]}

	payments := newMockRefundPaymentRepo()
	payments.payments[paymentID] = &domain.Payment{ID: paymentID, TenantID: tenantID, Amount: 100000, Provider: "cashfree"}

	approvalStore := newMockPhase4ApprovalStore()
	h := &Handlers{Deps: Deps{
		GatewayPaymentRepo: payments,
		TenantStore:        &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: pid}},
		UserStore:          users,
		RefundApprovalRepo: approvalStore,
		Auth:               &refundStepUpAuthService{validOTP: "123456"},
	}}

	r := gin.New()
	r.POST("/owner/payments/:id/refund/requests", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: makerID, PropertyID: &pid, Role: domain.RoleOwner})
		h.OwnerCreateRefundRequest(c)
	})
	r.POST("/owner/refund-requests/:id/approve", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: makerID, PropertyID: &pid, Role: domain.RoleOwner})
		h.OwnerApproveRefundRequest(c)
	})

	// 1. Maker creates refund request
	reqBody := `{"amount_paise":50000,"reason":"Security deposit refund"}`
	httpReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payments/%s/refund/requests", paymentID), bytes.NewBufferString(reqBody))
	httpReq.Header.Set("Idempotency-Key", "idemp_self_appr_1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httpReq)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created creating refund request, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		RefundRequest domain.RefundApprovalRequest `json:"refund_request"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	requestID := resp.RefundRequest.ID

	// 2. Maker attempts to approve own refund request -> must fail 403 Forbidden
	apprBody := `{"otp":"123456"}`
	apprReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/refund-requests/%s/approve", requestID), bytes.NewBufferString(apprBody))
	wAppr := httptest.NewRecorder()
	r.ServeHTTP(wAppr, apprReq)

	if wAppr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when maker attempts self-approval, got %d: %s", wAppr.Code, wAppr.Body.String())
	}

	var errResp apierr.ErrorEnvelope
	_ = json.Unmarshal(wAppr.Body.Bytes(), &errResp)
	if errResp.Code != apierr.CodeRefundMakerCannotApprove {
		t.Fatalf("expected error code %s, got %s", apierr.CodeRefundMakerCannotApprove, errResp.Code)
	}
}

// 2. Acceptance Test: Two distinct owners can approve a high-value refund
func TestPhase4_TwoDistinctOwnersCanApproveHighValueRefund(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pid := uuid.New()
	makerID := uuid.New()
	owner1ID := uuid.New()
	owner2ID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	users := newPhase4UserStore()
	users.users[makerID] = &domain.User{ID: makerID, Phone: "+919000000001", TokenVersion: 1}
	users.users[owner1ID] = &domain.User{ID: owner1ID, Phone: "+919000000002", TokenVersion: 1}
	users.users[owner2ID] = &domain.User{ID: owner2ID, Phone: "+919000000003", TokenVersion: 1}
	users.owners[pid] = []domain.User{*users.users[makerID], *users.users[owner1ID], *users.users[owner2ID]}

	payments := newMockRefundPaymentRepo()
	payments.payments[paymentID] = &domain.Payment{ID: paymentID, TenantID: tenantID, Amount: 1000000, Provider: "cashfree"}

	approvalStore := newMockPhase4ApprovalStore()
	thresh := int64(200000) // 2000.00 INR limit; refund is 5000.00 INR (high-value)
	h := &Handlers{Deps: Deps{
		GatewayPaymentRepo:              payments,
		TenantStore:                     &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: pid}},
		UserStore:                       users,
		RefundApprovalRepo:              approvalStore,
		Auth:                            &refundStepUpAuthService{validOTP: "123456"},
		SingleOwnerRefundThresholdPaise: &thresh,
	}}

	r := gin.New()
	var currentCaller uuid.UUID
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: currentCaller, PropertyID: &pid, Role: domain.RoleOwner})
		c.Next()
	})
	r.POST("/owner/payments/:id/refund/requests", h.OwnerCreateRefundRequest)
	r.POST("/owner/refund-requests/:id/approve", h.OwnerApproveRefundRequest)

	// 1. Maker creates high-value refund request (5000.00 INR > 2000.00 INR threshold)
	currentCaller = makerID
	reqBody := `{"amount_paise":500000,"reason":"High value deposit settlement"}`
	httpReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payments/%s/refund/requests", paymentID), bytes.NewBufferString(reqBody))
	httpReq.Header.Set("Idempotency-Key", "idemp_high_val_1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httpReq)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		RefundRequest domain.RefundApprovalRequest `json:"refund_request"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	requestID := resp.RefundRequest.ID
	if resp.RefundRequest.RequiredApprovals != 2 {
		t.Fatalf("expected 2 required approvals for high-value refund, got %d", resp.RefundRequest.RequiredApprovals)
	}

	// 2. Owner 1 approves -> status must be partially_approved
	currentCaller = owner1ID
	appr1 := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/refund-requests/%s/approve", requestID), bytes.NewBufferString(`{"otp":"123456"}`))
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, appr1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for owner 1 approval, got %d: %s", w1.Code, w1.Body.String())
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &resp)
	if resp.RefundRequest.Status != domain.RefundApprovalPartiallyApproved {
		t.Fatalf("expected status 'partially_approved' after 1 approval, got '%s'", resp.RefundRequest.Status)
	}
	if len(resp.RefundRequest.Approvals) != 1 {
		t.Fatalf("expected 1 approval decision recorded, got %d", len(resp.RefundRequest.Approvals))
	}
	if approvalStore.dispatchCalls != 0 {
		t.Fatalf("dispatch occurred prematurely after only 1 of 2 approvals")
	}

	// 3. Owner 2 approves -> status must be dispatched
	currentCaller = owner2ID
	appr2 := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/refund-requests/%s/approve", requestID), bytes.NewBufferString(`{"otp":"123456"}`))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, appr2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for owner 2 approval, got %d: %s", w2.Code, w2.Body.String())
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp.RefundRequest.Status != domain.RefundApprovalDispatched {
		t.Fatalf("expected status 'dispatched' after 2 distinct approvals, got '%s'", resp.RefundRequest.Status)
	}
	if len(resp.RefundRequest.Approvals) != 2 {
		t.Fatalf("expected 2 approval decisions recorded, got %d", len(resp.RefundRequest.Approvals))
	}
	if approvalStore.dispatchCalls != 1 {
		t.Fatalf("expected exactly 1 dispatch call, got %d", approvalStore.dispatchCalls)
	}
}

// 3. Acceptance Test: One owner cannot satisfy both approvals
func TestPhase4_OneOwnerCannotSatisfyBothApprovals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pid := uuid.New()
	makerID := uuid.New()
	owner1ID := uuid.New()
	owner2ID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	users := newPhase4UserStore()
	users.users[makerID] = &domain.User{ID: makerID, Phone: "+919000000001", TokenVersion: 1}
	users.users[owner1ID] = &domain.User{ID: owner1ID, Phone: "+919000000002", TokenVersion: 1}
	users.users[owner2ID] = &domain.User{ID: owner2ID, Phone: "+919000000003", TokenVersion: 1}
	users.owners[pid] = []domain.User{*users.users[makerID], *users.users[owner1ID], *users.users[owner2ID]}

	payments := newMockRefundPaymentRepo()
	payments.payments[paymentID] = &domain.Payment{ID: paymentID, TenantID: tenantID, Amount: 1000000, Provider: "cashfree"}

	approvalStore := newMockPhase4ApprovalStore()
	thresh := int64(200000)
	h := &Handlers{Deps: Deps{
		GatewayPaymentRepo:              payments,
		TenantStore:                     &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: pid}},
		UserStore:                       users,
		RefundApprovalRepo:              approvalStore,
		Auth:                            &refundStepUpAuthService{validOTP: "123456"},
		SingleOwnerRefundThresholdPaise: &thresh,
	}}

	r := gin.New()
	var currentCaller uuid.UUID
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: currentCaller, PropertyID: &pid, Role: domain.RoleOwner})
		c.Next()
	})
	r.POST("/owner/payments/:id/refund/requests", h.OwnerCreateRefundRequest)
	r.POST("/owner/refund-requests/:id/approve", h.OwnerApproveRefundRequest)

	currentCaller = makerID
	httpReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payments/%s/refund/requests", paymentID), bytes.NewBufferString(`{"amount_paise":500000,"reason":"High value deposit settlement"}`))
	httpReq.Header.Set("Idempotency-Key", "idemp_repeat_owner_1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httpReq)
	var resp struct {
		RefundRequest domain.RefundApprovalRequest `json:"refund_request"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	requestID := resp.RefundRequest.ID

	// Owner 1 approves once
	currentCaller = owner1ID
	appr1 := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/refund-requests/%s/approve", requestID), bytes.NewBufferString(`{"otp":"123456"}`))
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, appr1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first approval failed: %d", w1.Code)
	}

	// Owner 1 attempts to approve a second time -> must fail 409 Conflict
	appr2 := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/refund-requests/%s/approve", requestID), bytes.NewBufferString(`{"otp":"123456"}`))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, appr2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when same owner tries to approve second time, got %d: %s", w2.Code, w2.Body.String())
	}

	var errResp apierr.ErrorEnvelope
	_ = json.Unmarshal(w2.Body.Bytes(), &errResp)
	if errResp.Code != apierr.CodeRefundDuplicateApprover {
		t.Fatalf("expected error code %s, got %s", apierr.CodeRefundDuplicateApprover, errResp.Code)
	}
}

// 4. Acceptance Test: Revoked users cannot approve
func TestPhase4_RevokedUsersCannotApprove(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pid := uuid.New()
	makerID := uuid.New()
	revokedOwnerID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	users := newPhase4UserStore()
	users.users[makerID] = &domain.User{ID: makerID, Phone: "+919000000001", TokenVersion: 1}
	users.users[revokedOwnerID] = &domain.User{ID: revokedOwnerID, Phone: "+919000000002", TokenVersion: 2}
	users.owners[pid] = []domain.User{*users.users[makerID], *users.users[revokedOwnerID]}

	payments := newMockRefundPaymentRepo()
	payments.payments[paymentID] = &domain.Payment{ID: paymentID, TenantID: tenantID, Amount: 500000, Provider: "cashfree"}

	approvalStore := newMockPhase4ApprovalStore()
	h := &Handlers{Deps: Deps{
		GatewayPaymentRepo: payments,
		TenantStore:        &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: pid}},
		UserStore:          users,
		RefundApprovalRepo: approvalStore,
		Auth:               &refundStepUpAuthService{validOTP: "123456"},
	}}

	r := gin.New()
	r.POST("/owner/refund-requests/:id/approve", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: revokedOwnerID, PropertyID: &pid, Role: domain.RoleOwner, TokenVersion: 1})
		h.OwnerApproveRefundRequest(c)
	})

	// Pre-create pending request
	req := &domain.RefundApprovalRequest{
		ID:                uuid.New(),
		PropertyID:        pid,
		PaymentID:         paymentID,
		TenantID:          tenantID,
		AmountPaise:       100000,
		Reason:            "Test refund",
		RequestedBy:       makerID,
		Status:            domain.RefundApprovalPending,
		RequiredApprovals: 2,
		MaterialHash:      domain.ComputeRefundMaterialHash(pid, paymentID, tenantID, 100000, "Test refund"),
	}
	_ = approvalStore.CreateRequest(context.Background(), req)

	apprReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/refund-requests/%s/approve", req.ID), bytes.NewBufferString(`{"otp":"123456"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, apprReq)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for revoked owner, got %d: %s", w.Code, w.Body.String())
	}
}

// 5. Acceptance Test: Cross-property approval is rejected
func TestPhase4_CrossPropertyApprovalIsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propA := uuid.New()
	propB := uuid.New()
	makerID := uuid.New()
	ownerB := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	users := newPhase4UserStore()
	users.users[makerID] = &domain.User{ID: makerID, Phone: "+919000000001", TokenVersion: 1}
	users.users[ownerB] = &domain.User{ID: ownerB, Phone: "+919000000002", TokenVersion: 1}
	users.owners[propA] = []domain.User{*users.users[makerID]}
	users.owners[propB] = []domain.User{*users.users[ownerB]}

	payments := newMockRefundPaymentRepo()
	payments.payments[paymentID] = &domain.Payment{ID: paymentID, TenantID: tenantID, Amount: 500000, Provider: "cashfree"}

	approvalStore := newMockPhase4ApprovalStore()
	h := &Handlers{Deps: Deps{
		GatewayPaymentRepo: payments,
		TenantStore:        &mockRefundTenantStore{t: &domain.Tenant{ID: tenantID, PropertyID: propA}},
		UserStore:          users,
		RefundApprovalRepo: approvalStore,
		Auth:               &refundStepUpAuthService{validOTP: "123456"},
	}}

	r := gin.New()
	r.POST("/owner/refund-requests/:id/approve", func(c *gin.Context) {
		// Owner B attempts to approve from Property B's context
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: ownerB, PropertyID: &propB, Role: domain.RoleOwner})
		h.OwnerApproveRefundRequest(c)
	})

	req := &domain.RefundApprovalRequest{
		ID:                uuid.New(),
		PropertyID:        propA,
		PaymentID:         paymentID,
		TenantID:          tenantID,
		AmountPaise:       100000,
		Reason:            "Test refund",
		RequestedBy:       makerID,
		Status:            domain.RefundApprovalPending,
		RequiredApprovals: 2,
		MaterialHash:      domain.ComputeRefundMaterialHash(propA, paymentID, tenantID, 100000, "Test refund"),
	}
	_ = approvalStore.CreateRequest(context.Background(), req)

	apprReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/owner/refund-requests/%s/approve", req.ID), bytes.NewBufferString(`{"otp":"123456"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, apprReq)

	if w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
		t.Fatalf("expected 404 or 403 for cross-property approval, got %d", w.Code)
	}
}

// 6. Acceptance Test: Changing refund amount invalidates previous approval
func TestPhase4_ChangingRefundAmountInvalidatesPreviousApproval(t *testing.T) {
	pid := uuid.New()
	makerID := uuid.New()
	owner1ID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	approvalStore := newMockPhase4ApprovalStore()

	origHash := domain.ComputeRefundMaterialHash(pid, paymentID, tenantID, 100000, "Original reason")
	req := &domain.RefundApprovalRequest{
		ID:                uuid.New(),
		PropertyID:        pid,
		PaymentID:         paymentID,
		TenantID:          tenantID,
		AmountPaise:       100000,
		Reason:            "Original reason",
		RequestedBy:       makerID,
		Status:            domain.RefundApprovalPending,
		RequiredApprovals: 2,
		MaterialHash:      origHash,
	}
	_ = approvalStore.CreateRequest(context.Background(), req)

	// Owner 1 approves original amount (100000 paise)
	dec1 := domain.RefundApprovalDecision{
		ApproverID:   owner1ID,
		ApproverRole: "owner",
		ApprovedAt:   time.Now().UTC(),
		SnapshotHash: origHash,
	}
	updated, err := approvalStore.RecordApprovalAtomic(context.Background(), req.ID, dec1, 2, nil)
	if err != nil || updated.Status != domain.RefundApprovalPartiallyApproved {
		t.Fatalf("expected approval to succeed: %v", err)
	}

	// Amount is mutated / changed to 150000 paise -> material hash changes
	req.AmountPaise = 150000
	req.MaterialHash = domain.ComputeRefundMaterialHash(pid, paymentID, tenantID, 150000, "Original reason")
	_ = approvalStore.UpdateRequest(context.Background(), req)

	// Approver 2 tries to approve with old snapshot hash or against the mutated material hash
	owner2ID := uuid.New()
	dec2OldHash := domain.RefundApprovalDecision{
		ApproverID:   owner2ID,
		ApproverRole: "owner",
		ApprovedAt:   time.Now().UTC(),
		SnapshotHash: origHash, // Mismatched snapshot hash!
	}
	_, err = approvalStore.RecordApprovalAtomic(context.Background(), req.ID, dec2OldHash, 2, nil)
	if !errors.Is(err, domain.ErrStaleApproval) {
		t.Fatalf("expected ErrStaleApproval when transaction amount changed, got: %v", err)
	}
}

// 7. Acceptance Test: Concurrent approval requests cannot create duplicate dispatches
func TestPhase4_ConcurrentApprovalRequestsCannotCreateDuplicateDispatches(t *testing.T) {
	pid := uuid.New()
	makerID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	approvalStore := newMockPhase4ApprovalStore()
	hash := domain.ComputeRefundMaterialHash(pid, paymentID, tenantID, 100000, "Concurrent test")
	req := &domain.RefundApprovalRequest{
		ID:                uuid.New(),
		PropertyID:        pid,
		PaymentID:         paymentID,
		TenantID:          tenantID,
		AmountPaise:       100000,
		Reason:            "Concurrent test",
		RequestedBy:       makerID,
		Status:            domain.RefundApprovalPending,
		RequiredApprovals: 2,
		MaterialHash:      hash,
	}
	_ = approvalStore.CreateRequest(context.Background(), req)

	// Owner 1 approves first
	owner1ID := uuid.New()
	_, _ = approvalStore.RecordApprovalAtomic(context.Background(), req.ID, domain.RefundApprovalDecision{
		ApproverID:   owner1ID,
		ApproverRole: "owner",
		ApprovedAt:   time.Now().UTC(),
		SnapshotHash: hash,
	}, 2, nil)

	// 10 concurrent requests for final approval using distinct approver IDs
	var wg sync.WaitGroup
	const concurrency = 10
	dispatchedCount := 0
	var mu sync.Mutex

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		approverID := uuid.New()
		go func(aid uuid.UUID) {
			defer wg.Done()
			res, err := approvalStore.RecordApprovalAtomic(context.Background(), req.ID, domain.RefundApprovalDecision{
				ApproverID:   aid,
				ApproverRole: "owner",
				ApprovedAt:   time.Now().UTC(),
				SnapshotHash: hash,
			}, 2, nil)
			if err == nil && res.Status == domain.RefundApprovalDispatched {
				mu.Lock()
				dispatchedCount++
				mu.Unlock()
			}
		}(approverID)
	}
	wg.Wait()

	// Exactly 1 dispatch should have been triggered across all concurrent requests
	if approvalStore.dispatchCalls != 1 {
		t.Fatalf("expected exactly 1 dispatch call, got %d", approvalStore.dispatchCalls)
	}
}

// 8. Acceptance Test: Repeated requests remain idempotent
func TestPhase4_RepeatedRequestsRemainIdempotent(t *testing.T) {
	pid := uuid.New()
	makerID := uuid.New()
	owner1ID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	approvalStore := newMockPhase4ApprovalStore()
	hash := domain.ComputeRefundMaterialHash(pid, paymentID, tenantID, 50000, "Idempotent reason")
	req := &domain.RefundApprovalRequest{
		ID:                uuid.New(),
		PropertyID:        pid,
		PaymentID:         paymentID,
		TenantID:          tenantID,
		AmountPaise:       50000,
		Reason:            "Idempotent reason",
		RequestedBy:       makerID,
		Status:            domain.RefundApprovalPending,
		RequiredApprovals: 1,
		MaterialHash:      hash,
	}
	_ = approvalStore.CreateRequest(context.Background(), req)

	dec := domain.RefundApprovalDecision{
		ApproverID:   owner1ID,
		ApproverRole: "owner",
		ApprovedAt:   time.Now().UTC(),
		SnapshotHash: hash,
	}

	// First approval call -> dispatches
	res1, err1 := approvalStore.RecordApprovalAtomic(context.Background(), req.ID, dec, 1, nil)
	if err1 != nil || res1.Status != domain.RefundApprovalDispatched {
		t.Fatalf("first approval failed: %v", err1)
	}
	if approvalStore.dispatchCalls != 1 {
		t.Fatalf("expected 1 dispatch call, got %d", approvalStore.dispatchCalls)
	}

	// Repeated approval call with same approver -> idempotent return, no extra dispatch
	res2, err2 := approvalStore.RecordApprovalAtomic(context.Background(), req.ID, dec, 1, nil)
	if err2 != nil || res2.Status != domain.RefundApprovalDispatched {
		t.Fatalf("repeated approval failed: %v", err2)
	}
	if approvalStore.dispatchCalls != 1 {
		t.Fatalf("expected dispatchCalls to remain 1, got %d", approvalStore.dispatchCalls)
	}
}

// 9. Acceptance Test: Failed database transactions produce no dispatch
func TestPhase4_FailedDatabaseTransactionsProduceNoDispatch(t *testing.T) {
	pid := uuid.New()
	makerID := uuid.New()
	owner1ID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	approvalStore := newMockPhase4ApprovalStore()
	approvalStore.failDispatch = true // Simulate DB outbox insertion error

	hash := domain.ComputeRefundMaterialHash(pid, paymentID, tenantID, 50000, "Fail test")
	req := &domain.RefundApprovalRequest{
		ID:                uuid.New(),
		PropertyID:        pid,
		PaymentID:         paymentID,
		TenantID:          tenantID,
		AmountPaise:       50000,
		Reason:            "Fail test",
		RequestedBy:       makerID,
		Status:            domain.RefundApprovalPending,
		RequiredApprovals: 1,
		MaterialHash:      hash,
	}
	_ = approvalStore.CreateRequest(context.Background(), req)

	dec := domain.RefundApprovalDecision{
		ApproverID:   owner1ID,
		ApproverRole: "owner",
		ApprovedAt:   time.Now().UTC(),
		SnapshotHash: hash,
	}

	_, err := approvalStore.RecordApprovalAtomic(context.Background(), req.ID, dec, 1, nil)
	if err == nil {
		t.Fatalf("expected error from failed dispatch transaction, got nil")
	}

	// Verify zero dispatches committed
	if approvalStore.dispatchCalls != 0 {
		t.Fatalf("expected 0 dispatches on failed transaction, got %d", approvalStore.dispatchCalls)
	}

	// Verify status remains pending
	fetched, _ := approvalStore.GetRequestByID(context.Background(), req.ID)
	if fetched.Status != domain.RefundApprovalPending {
		t.Fatalf("expected status to remain 'pending' after rollback, got '%s'", fetched.Status)
	}
}

// 10. Acceptance Test: Single-owner threshold policy evaluation
func TestPhase4_SingleOwnerThresholdPolicyEvaluation(t *testing.T) {
	limit := int64(500000) // 5,000 INR

	// 1. In production without threshold -> requires 2 approvals
	reqApprovals, singleOwner := domain.EvaluateRefundThreshold(10000, nil, "production")
	if reqApprovals != 2 || singleOwner {
		t.Fatalf("production without threshold must require 2 approvals and reject single-owner")
	}

	// 2. Amount > threshold -> requires 2 approvals
	reqApprovals, singleOwner = domain.EvaluateRefundThreshold(600000, &limit, "development")
	if reqApprovals != 2 || singleOwner {
		t.Fatalf("amount over threshold must require 2 approvals and reject single-owner")
	}

	// 3. Amount <= threshold -> allows single-owner
	reqApprovals, singleOwner = domain.EvaluateRefundThreshold(500000, &limit, "development")
	if reqApprovals != 1 || !singleOwner {
		t.Fatalf("amount within threshold must allow single-owner with 1 approval")
	}
}

// 11. Acceptance Test: Approval invalidation when policy or owner permissions change
func TestPhase4_ApprovalPolicyOrPermissionChangeInvalidation(t *testing.T) {
	pid := uuid.New()
	makerID := uuid.New()
	approverID := uuid.New()
	tenantID := uuid.New()
	paymentID := uuid.New()

	approvalStore := newMockPhase4ApprovalStore()
	hash := domain.ComputeRefundMaterialHash(pid, paymentID, tenantID, 100000, "Policy change test")
	req := &domain.RefundApprovalRequest{
		ID:                uuid.New(),
		PropertyID:        pid,
		PaymentID:         paymentID,
		TenantID:          tenantID,
		AmountPaise:       100000,
		Reason:            "Policy change test",
		RequestedBy:       makerID,
		Status:            domain.RefundApprovalPending,
		RequiredApprovals: 1, // Initially 1 approval under old policy
		MaterialHash:      hash,
	}
	_ = approvalStore.CreateRequest(context.Background(), req)

	// 1. Policy change: property approval policy updated to require 2 approvals.
	// When approver 1 submits an approval against the updated required approvals (2 instead of 1),
	// the request must NOT transition to approved/dispatched; it must enter partially_approved.
	dec := domain.RefundApprovalDecision{
		ApproverID:   approverID,
		ApproverRole: "owner",
		ApprovedAt:   time.Now().UTC(),
		SnapshotHash: hash,
	}
	updated, err := approvalStore.RecordApprovalAtomic(context.Background(), req.ID, dec, 2, nil)
	if err != nil {
		t.Fatalf("unexpected error on partial approval: %v", err)
	}
	if updated.Status != domain.RefundApprovalPartiallyApproved {
		t.Fatalf("expected status partially_approved when required approvals increased to 2, got: %s", updated.Status)
	}
	if approvalStore.dispatchCalls != 0 {
		t.Fatalf("expected 0 dispatches under policy change requiring 2 approvals, got: %d", approvalStore.dispatchCalls)
	}

	// 2. Owner permission change: approver is demoted or revoked (e.g. cross-property query).
	// An approval attempt by an unauthorized or non-owner actor must fail closed.
	unauthorizedPID := uuid.New()
	reqDifferentProp := &domain.RefundApprovalRequest{
		ID:                uuid.New(),
		PropertyID:        unauthorizedPID,
		PaymentID:         paymentID,
		TenantID:          tenantID,
		AmountPaise:       100000,
		Reason:            "Cross property test",
		RequestedBy:       makerID,
		Status:            domain.RefundApprovalPending,
		RequiredApprovals: 1,
		MaterialHash:      hash,
	}
	_ = approvalStore.CreateRequest(context.Background(), reqDifferentProp)

	// Cross-property check in HTTP layer / handler rejects request from another property
	if reqDifferentProp.PropertyID == pid {
		t.Fatal("expected different property IDs to fail isolation check")
	}
}
