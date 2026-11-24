package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockPlatformPayoutStore struct {
	exceptions       []domain.PayoutExceptionItem
	batches          []domain.PayoutBatch
	itemsByBatch     map[uuid.UUID][]domain.PayoutItem
	batchByID        map[uuid.UUID]*domain.PayoutBatch
	reconciledItem   *domain.PayoutItem
	reconcileErr     error
	lastStatusFilter *domain.PayoutBatchStatus
	lastLimit        int
	lastOffset       int
	lastReconcileIn  struct {
		itemID        uuid.UUID
		newStatus     domain.PayoutItemStatus
		cfTransferID  *string
		utr           *string
		settledAt     *time.Time
		failureReason *string
		operatorID    uuid.UUID
		notes         string
	}
	casesByID        map[uuid.UUID]*domain.PlatformPayoutReconciliationCase
	casesList        []domain.PlatformPayoutReconciliationCase
	createCaseErr    error
	approveCaseErr   error
	rejectCaseErr    error
	lastCreateCaseIn domain.CreatePayoutReconciliationCaseParams
}

func (m *mockPlatformPayoutStore) ListPayoutExceptions(ctx context.Context, limit, offset int) ([]domain.PayoutExceptionItem, error) {
	m.lastLimit = limit
	m.lastOffset = offset
	return m.exceptions, nil
}

func (m *mockPlatformPayoutStore) ListAllPayoutBatches(ctx context.Context, status *domain.PayoutBatchStatus, limit, offset int) ([]domain.PayoutBatch, error) {
	m.lastStatusFilter = status
	m.lastLimit = limit
	m.lastOffset = offset
	return m.batches, nil
}

func (m *mockPlatformPayoutStore) GetBatchByIDCrossProperty(ctx context.Context, id uuid.UUID) (*domain.PayoutBatch, error) {
	if b, ok := m.batchByID[id]; ok {
		return b, nil
	}
	return nil, domain.ErrNotFound
}

func (m *mockPlatformPayoutStore) ListPayoutItemsByBatchCrossProperty(ctx context.Context, batchID uuid.UUID) ([]domain.PayoutItem, error) {
	if items, ok := m.itemsByBatch[batchID]; ok {
		return items, nil
	}
	return []domain.PayoutItem{}, nil
}

func (m *mockPlatformPayoutStore) ReconcilePayoutItemManual(
	ctx context.Context,
	itemID uuid.UUID,
	newStatus domain.PayoutItemStatus,
	cfTransferID, utr *string,
	settledAt *time.Time,
	failureReason *string,
	operatorID uuid.UUID,
	notes string,
) (*domain.PayoutItem, error) {
	m.lastReconcileIn.itemID = itemID
	m.lastReconcileIn.newStatus = newStatus
	m.lastReconcileIn.cfTransferID = cfTransferID
	m.lastReconcileIn.utr = utr
	m.lastReconcileIn.settledAt = settledAt
	m.lastReconcileIn.failureReason = failureReason
	m.lastReconcileIn.operatorID = operatorID
	m.lastReconcileIn.notes = notes

	if m.reconcileErr != nil {
		return nil, m.reconcileErr
	}
	if m.reconciledItem != nil {
		return m.reconciledItem, nil
	}
	return &domain.PayoutItem{
		ID:            itemID,
		Status:        newStatus,
		UTR:           utr,
		CFTransferID:  cfTransferID,
		SettledAt:     settledAt,
		FailureReason: failureReason,
		UpdatedAt:     time.Now().UTC(),
	}, nil
}

func (m *mockPlatformPayoutStore) CreatePayoutReconciliationCase(ctx context.Context, params domain.CreatePayoutReconciliationCaseParams) (*domain.PlatformPayoutReconciliationCase, error) {
	m.lastCreateCaseIn = params
	if m.createCaseErr != nil {
		return nil, m.createCaseErr
	}
	c := &domain.PlatformPayoutReconciliationCase{
		ID:             uuid.New(),
		PayoutItemID:   params.PayoutItemID,
		ProposedStatus: params.ProposedStatus,
		UTR:            params.UTR,
		CFTransferID:   params.CFTransferID,
		SettledAt:      params.SettledAt,
		FailureReason:  params.FailureReason,
		Notes:          params.Notes,
		EvidenceRef:    params.EvidenceRef,
		EvidenceHash:   params.EvidenceHash,
		MakerID:        params.MakerID,
		Status:         domain.ReconciliationCasePending,
		ItemSnapshot:   `{"id":"` + params.PayoutItemID.String() + `","status":"processing"}`,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	if m.casesByID == nil {
		m.casesByID = make(map[uuid.UUID]*domain.PlatformPayoutReconciliationCase)
	}
	m.casesByID[c.ID] = c
	return c, nil
}

func (m *mockPlatformPayoutStore) GetPayoutReconciliationCase(ctx context.Context, caseID uuid.UUID) (*domain.PlatformPayoutReconciliationCase, error) {
	if c, ok := m.casesByID[caseID]; ok {
		return c, nil
	}
	return nil, domain.ErrNotFound
}

func (m *mockPlatformPayoutStore) ListPayoutReconciliationCases(ctx context.Context, status *domain.PayoutReconciliationCaseStatus, limit, offset int) ([]domain.PlatformPayoutReconciliationCase, error) {
	return m.casesList, nil
}

func (m *mockPlatformPayoutStore) ApprovePayoutReconciliationCase(ctx context.Context, caseID uuid.UUID, checkerID uuid.UUID, checkerNotes string) (*domain.PayoutItem, error) {
	if m.approveCaseErr != nil {
		return nil, m.approveCaseErr
	}
	c, ok := m.casesByID[caseID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if checkerID == c.MakerID {
		return nil, errors.New("maker cannot approve own reconciliation case; independent checker required")
	}
	c.Status = domain.ReconciliationCaseApproved
	c.CheckerID = &checkerID
	return &domain.PayoutItem{
		ID:            c.PayoutItemID,
		Status:        c.ProposedStatus,
		UTR:           c.UTR,
		CFTransferID:  c.CFTransferID,
		SettledAt:     c.SettledAt,
		FailureReason: c.FailureReason,
		UpdatedAt:     time.Now().UTC(),
	}, nil
}

func (m *mockPlatformPayoutStore) RejectPayoutReconciliationCase(ctx context.Context, caseID uuid.UUID, checkerID uuid.UUID, rejectionReason string) (*domain.PlatformPayoutReconciliationCase, error) {
	if m.rejectCaseErr != nil {
		return nil, m.rejectCaseErr
	}
	c, ok := m.casesByID[caseID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if checkerID == c.MakerID {
		return nil, errors.New("maker cannot reject own reconciliation case; independent checker required")
	}
	c.Status = domain.ReconciliationCaseRejected
	c.CheckerID = &checkerID
	c.RejectionReason = &rejectionReason
	return c, nil
}

func setupPlatformPayoutTest(t *testing.T, store *mockPlatformPayoutStore) (*gin.Engine, string, *domain.User) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	const secret = "platform-ops-test-secret-32-chars-ok"

	opsUser := &domain.User{
		ID:    uuid.New(),
		Role:  domain.RolePlatformOps,
		Phone: "+919876543210",
	}

	deps := Deps{
		JWTSecret:          secret,
		PayoutPlatformRepo: store,
	}
	r := NewRouter(deps)
	token, err := auth.IssueAccessToken(secret, opsUser)
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}

	return r, token, opsUser
}

func TestPlatformPayouts_RBAC(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "platform-ops-test-secret-32-chars-ok"
	store := &mockPlatformPayoutStore{}
	deps := Deps{
		JWTSecret:          secret,
		PayoutPlatformRepo: store,
	}
	r := NewRouter(deps)

	// 1. Unauthenticated -> 401
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/platform/payouts/exceptions", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: expected 401, got %d", w.Code)
	}

	// 2. Role Owner -> 403
	propID := uuid.New()
	owner := &domain.User{ID: uuid.New(), Role: domain.RoleOwner, Phone: "+919000000001", PropertyID: &propID}
	ownerTok, _ := auth.IssueAccessToken(secret, owner)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/platform/payouts/exceptions", nil)
	req.Header.Set("Authorization", "Bearer "+ownerTok)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("owner: expected 403, got %d", w.Code)
	}

	// 3. Role Manager -> 403
	mgr := &domain.User{ID: uuid.New(), Role: domain.RoleManager, Phone: "+919000000002", PropertyID: &propID}
	mgrTok, _ := auth.IssueAccessToken(secret, mgr)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/platform/payouts/exceptions", nil)
	req.Header.Set("Authorization", "Bearer "+mgrTok)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("manager: expected 403, got %d", w.Code)
	}

	// 4. Role Tenant -> 403
	tenantID := uuid.New()
	tenant := &domain.User{ID: uuid.New(), Role: domain.RoleTenant, Phone: "+919000000003", PropertyID: &propID, TenantID: &tenantID}
	tenantTok, _ := auth.IssueAccessToken(secret, tenant)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/platform/payouts/exceptions", nil)
	req.Header.Set("Authorization", "Bearer "+tenantTok)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("tenant: expected 403, got %d", w.Code)
	}

	// 5. Role PlatformOps -> 200
	ops := &domain.User{ID: uuid.New(), Role: domain.RolePlatformOps, Phone: "+919000000004"}
	opsTok, _ := auth.IssueAccessToken(secret, ops)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/platform/payouts/exceptions", nil)
	req.Header.Set("Authorization", "Bearer "+opsTok)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("platform_ops: expected 200, got %d", w.Code)
	}
}

func TestPlatformListPayoutExceptions(t *testing.T) {
	store := &mockPlatformPayoutStore{
		exceptions: []domain.PayoutExceptionItem{
			{
				PayoutItem: domain.PayoutItem{
					ID:          uuid.New(),
					AmountPaise: 500000,
					Status:      domain.PayoutFailed,
				},
				PropertyID:   uuid.New(),
				PropertyName: "Sunrise Residency",
				PayeeName:    "Ramesh Kumar",
				PayeeType:    domain.PayeeTypeStaff,
			},
		},
	}
	r, token, _ := setupPlatformPayoutTest(t, store)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/platform/payouts/exceptions?limit=25&offset=10", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.lastLimit != 25 || store.lastOffset != 10 {
		t.Errorf("expected limit=25 offset=10, got limit=%d offset=%d", store.lastLimit, store.lastOffset)
	}

	var res struct {
		Exceptions []domain.PayoutExceptionItem `json:"exceptions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(res.Exceptions) != 1 {
		t.Fatalf("expected 1 exception, got %d", len(res.Exceptions))
	}
	if res.Exceptions[0].PropertyName != "Sunrise Residency" {
		t.Errorf("expected Sunrise Residency, got %s", res.Exceptions[0].PropertyName)
	}
}

func TestPlatformListPayoutBatches(t *testing.T) {
	batchID := uuid.New()
	store := &mockPlatformPayoutStore{
		batches: []domain.PayoutBatch{
			{
				ID:               batchID,
				BatchNumber:      "BATCH-001",
				Status:           domain.BatchProcessing,
				TotalAmountPaise: 1200000,
				ItemCount:        2,
			},
		},
	}
	r, token, _ := setupPlatformPayoutTest(t, store)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/platform/payouts/batches?status=processing&limit=20&offset=5", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.lastStatusFilter == nil || *store.lastStatusFilter != domain.BatchProcessing {
		t.Errorf("expected status filter processing, got %v", store.lastStatusFilter)
	}
	if store.lastLimit != 20 || store.lastOffset != 5 {
		t.Errorf("expected limit=20 offset=5, got limit=%d offset=%d", store.lastLimit, store.lastOffset)
	}

	var res struct {
		Batches []domain.PayoutBatch `json:"batches"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(res.Batches) != 1 || res.Batches[0].BatchNumber != "BATCH-001" {
		t.Errorf("unexpected batches: %v", res.Batches)
	}
}

func TestPlatformGetPayoutBatch(t *testing.T) {
	batchID := uuid.New()
	store := &mockPlatformPayoutStore{
		batchByID: map[uuid.UUID]*domain.PayoutBatch{
			batchID: {
				ID:          batchID,
				BatchNumber: "BATCH-002",
				Status:      domain.BatchCompleted,
			},
		},
		itemsByBatch: map[uuid.UUID][]domain.PayoutItem{
			batchID: {
				{ID: uuid.New(), BatchID: &batchID, AmountPaise: 50000, Status: domain.PayoutSucceeded},
			},
		},
	}
	r, token, _ := setupPlatformPayoutTest(t, store)

	// 1. Existing batch
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/platform/payouts/batches/"+batchID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		Batch domain.PayoutBatch  `json:"batch"`
		Items []domain.PayoutItem `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if res.Batch.BatchNumber != "BATCH-002" || len(res.Items) != 1 {
		t.Errorf("unexpected batch or items: %v", res)
	}

	// 2. Unknown batch -> 404
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/platform/payouts/batches/"+uuid.New().String(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown batch, got %d", w.Code)
	}
}

func TestPlatformReconcilePayoutItem_Validations(t *testing.T) {
	t.Skip("direct single-operator reconciliation is disabled in favor of maker-checker reconciliation cases")
	store := &mockPlatformPayoutStore{}
	r, token, _ := setupPlatformPayoutTest(t, store)
	itemID := uuid.New()

	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{
			name: "missing notes",
			payload: map[string]any{
				"status": "succeeded",
				"utr":    "UTR123456",
				"notes":  "   ",
			},
			wantSub: "reconciliation notes are required",
		},
		{
			name: "succeeded missing utr",
			payload: map[string]any{
				"status": "succeeded",
				"notes":  "Reconciled from bank statement",
			},
			wantSub: "utr is required",
		},
		{
			name: "failed missing failure_reason",
			payload: map[string]any{
				"status": "failed",
				"notes":  "Rejected by bank",
			},
			wantSub: "failure_reason is required",
		},
		{
			name: "rejected missing failure_reason",
			payload: map[string]any{
				"status": "rejected",
				"notes":  "Beneficiary account closed",
			},
			wantSub: "failure_reason is required",
		},
		{
			name: "invalid status",
			payload: map[string]any{
				"status": "pending",
				"notes":  "Resetting status",
			},
			wantSub: "invalid reconciliation status",
		},
		{
			name: "invalid settled_at format",
			payload: map[string]any{
				"status":     "succeeded",
				"utr":        "UTR99999",
				"settled_at": "not-a-timestamp",
				"notes":      "Reconciled from statement",
			},
			wantSub: "invalid settled_at format",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.payload)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/platform/payouts/items/"+itemID.String()+"/reconcile", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			bodyLower := strings.ToLower(w.Body.String())
			wantLower := strings.ToLower(tc.wantSub)
			if !strings.Contains(bodyLower, wantLower) && !strings.Contains(bodyLower, strings.ReplaceAll(wantLower, "_", "")) {
				t.Errorf("expected body to contain %q, got %s", tc.wantSub, w.Body.String())
			}
		})
	}
}

func TestPlatformReconcilePayoutItem_DirectReconciliationForbidden(t *testing.T) {
	store := &mockPlatformPayoutStore{}
	r, token, _ := setupPlatformPayoutTest(t, store)
	itemID := uuid.New()

	body := map[string]any{
		"status": "succeeded",
		"utr":    "UTR-BANK-123456",
		"notes":  "Single operator direct reconciliation attempt",
	}
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/platform/payouts/items/"+itemID.String()+"/reconcile", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for direct reconciliation, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("maker-checker two-person control is required")) {
		t.Errorf("expected body to mention maker-checker requirement, got %s", w.Body.String())
	}
}

func TestPlatformPayoutReconciliationCase_MakerCheckerLifecycle(t *testing.T) {
	store := &mockPlatformPayoutStore{}
	r, makerToken, makerUser := setupPlatformPayoutTest(t, store)
	itemID := uuid.New()

	// 1. Maker submits reconciliation case -> 201 Created, pending status
	nowStr := time.Now().UTC().Format(time.RFC3339)
	createBody := map[string]any{
		"proposed_status": "succeeded",
		"utr":             "UTR-BANK-778899",
		"cf_transfer_id":  "cf_tr_7788",
		"settled_at":      nowStr,
		"notes":           "Reconciled from bank statement statement_nov.pdf",
		"evidence_ref":    "storage://ops-audit/evidence/statement_nov.pdf",
		"evidence_hash":   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
	b, _ := json.Marshal(createBody)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/platform/payouts/items/"+itemID.String()+"/reconcile-case", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+makerToken)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for maker case submission, got %d: %s", w.Code, w.Body.String())
	}

	var createResp struct {
		Case domain.PlatformPayoutReconciliationCase `json:"case"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("unmarshal create case response: %v", err)
	}
	caseID := createResp.Case.ID
	if caseID == uuid.Nil {
		t.Fatalf("expected non-nil case ID")
	}
	if createResp.Case.Status != domain.ReconciliationCasePending {
		t.Fatalf("expected pending status, got %s", createResp.Case.Status)
	}
	if createResp.Case.MakerID != makerUser.ID {
		t.Fatalf("expected maker ID %s, got %s", makerUser.ID, createResp.Case.MakerID)
	}

	// 2. Maker attempts self-approval -> 403 Forbidden!
	approveBody := map[string]any{
		"notes": "Self approval attempt by maker",
	}
	ab, _ := json.Marshal(approveBody)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/platform/payouts/reconcile-cases/"+caseID.String()+"/approve", bytes.NewReader(ab))
	req.Header.Set("Authorization", "Bearer "+makerToken) // Same maker token!
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for maker self-approval, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("maker cannot approve own reconciliation case")) {
		t.Errorf("expected body to mention maker self-approval forbidden, got %s", w.Body.String())
	}

	// 3. Independent checker approves -> 200 OK
	checkerUser := &domain.User{
		ID:    uuid.New(),
		Role:  domain.RolePlatformOps,
		Phone: "+919876500000",
	}
	checkerToken, err := auth.IssueAccessToken("platform-ops-test-secret-32-chars-ok", checkerUser)
	if err != nil {
		t.Fatalf("issue checker token: %v", err)
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/platform/payouts/reconcile-cases/"+caseID.String()+"/approve", bytes.NewReader(ab))
	req.Header.Set("Authorization", "Bearer "+checkerToken) // Different checker token!
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for independent checker approval, got %d: %s", w.Code, w.Body.String())
	}

	var approveResp struct {
		Item domain.PayoutItem `json:"item"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &approveResp); err != nil {
		t.Fatalf("unmarshal approve response: %v", err)
	}
	if approveResp.Item.Status != domain.PayoutSucceeded {
		t.Fatalf("expected item status succeeded, got %s", approveResp.Item.Status)
	}
	if approveResp.Item.UTR == nil || *approveResp.Item.UTR != "UTR-BANK-778899" {
		t.Fatalf("expected UTR UTR-BANK-778899, got %v", approveResp.Item.UTR)
	}
}

func TestPlatformPayoutReconciliationCase_RejectionFlow(t *testing.T) {
	store := &mockPlatformPayoutStore{}
	r, makerToken, _ := setupPlatformPayoutTest(t, store)
	itemID := uuid.New()

	createBody := map[string]any{
		"proposed_status": "failed",
		"failure_reason":  "UTR not found on bank ledger",
		"notes":           "Check with bank manager",
		"evidence_ref":    "storage://ops-audit/evidence/bank_inquiry.pdf",
		"evidence_hash":   "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
	}
	b, _ := json.Marshal(createBody)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/platform/payouts/items/"+itemID.String()+"/reconcile-case", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+makerToken)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}

	var createResp struct {
		Case domain.PlatformPayoutReconciliationCase `json:"case"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	caseID := createResp.Case.ID

	// Maker attempts self-rejection -> 403 Forbidden
	rejectBody := map[string]any{
		"reason": "Rejected by maker self",
	}
	rb, _ := json.Marshal(rejectBody)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/platform/payouts/reconcile-cases/"+caseID.String()+"/reject", bytes.NewReader(rb))
	req.Header.Set("Authorization", "Bearer "+makerToken)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for maker self-rejection, got %d: %s", w.Code, w.Body.String())
	}

	// Independent checker rejects -> 200 OK
	checkerUser := &domain.User{
		ID:    uuid.New(),
		Role:  domain.RolePlatformOps,
		Phone: "+919876500000",
	}
	checkerToken, _ := auth.IssueAccessToken("platform-ops-test-secret-32-chars-ok", checkerUser)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/platform/payouts/reconcile-cases/"+caseID.String()+"/reject", bytes.NewReader(rb))
	req.Header.Set("Authorization", "Bearer "+checkerToken)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for checker rejection, got %d: %s", w.Code, w.Body.String())
	}
	var rejectResp struct {
		Case domain.PlatformPayoutReconciliationCase `json:"case"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &rejectResp)
	if rejectResp.Case.Status != domain.ReconciliationCaseRejected {
		t.Fatalf("expected rejected status, got %s", rejectResp.Case.Status)
	}
	if rejectResp.Case.CheckerID == nil || *rejectResp.Case.CheckerID != checkerUser.ID {
		t.Fatalf("expected checker ID %s, got %v", checkerUser.ID, rejectResp.Case.CheckerID)
	}
}

func TestPlatformPayoutReconciliationCase_Validations(t *testing.T) {
	store := &mockPlatformPayoutStore{}
	r, token, _ := setupPlatformPayoutTest(t, store)
	itemID := uuid.New()

	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{
			name: "missing evidence_ref",
			payload: map[string]any{
				"proposed_status": "succeeded",
				"utr":             "UTR123",
				"notes":           "Some notes",
				"evidence_hash":   "hash123",
			},
			wantSub: "evidence_ref",
		},
		{
			name: "missing evidence_hash",
			payload: map[string]any{
				"proposed_status": "succeeded",
				"utr":             "UTR123",
				"notes":           "Some notes",
				"evidence_ref":    "ref123",
			},
			wantSub: "evidence_hash",
		},
		{
			name: "succeeded missing utr",
			payload: map[string]any{
				"proposed_status": "succeeded",
				"notes":           "Some notes",
				"evidence_ref":    "ref123",
				"evidence_hash":   "hash123",
			},
			wantSub: "utr is required",
		},
		{
			name: "failed missing failure_reason",
			payload: map[string]any{
				"proposed_status": "failed",
				"notes":           "Some notes",
				"evidence_ref":    "ref123",
				"evidence_hash":   "hash123",
			},
			wantSub: "failure_reason is required",
		},
		{
			name: "invalid proposed status",
			payload: map[string]any{
				"proposed_status": "pending",
				"notes":           "Some notes",
				"evidence_ref":    "ref123",
				"evidence_hash":   "hash123",
			},
			wantSub: "invalid proposed reconciliation status",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.payload)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/platform/payouts/items/"+itemID.String()+"/reconcile-case", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 Bad Request, got %d: %s", w.Code, w.Body.String())
			}
			bodyLower := strings.ToLower(w.Body.String())
			wantLower := strings.ToLower(tc.wantSub)
			if !strings.Contains(bodyLower, wantLower) && !strings.Contains(bodyLower, strings.ReplaceAll(wantLower, "_", "")) {
				t.Errorf("expected body to contain %q, got %s", tc.wantSub, w.Body.String())
			}
		})
	}
}
