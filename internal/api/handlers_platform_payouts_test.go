package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
			if !bytes.Contains(w.Body.Bytes(), []byte(tc.wantSub)) {
				t.Errorf("expected body to contain %q, got %s", tc.wantSub, w.Body.String())
			}
		})
	}
}

func TestPlatformReconcilePayoutItem_Execution(t *testing.T) {
	store := &mockPlatformPayoutStore{}
	r, token, opsUser := setupPlatformPayoutTest(t, store)
	itemID := uuid.New()

	// 1. Success transition
	nowStr := time.Now().UTC().Format(time.RFC3339)
	body := map[string]any{
		"status":         "succeeded",
		"utr":            "UTR-BANK-123456",
		"cf_transfer_id": "cf_tr_9999",
		"settled_at":     nowStr,
		"notes":          "Reconciled against HDFC bank statement #20261101",
	}
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/platform/payouts/items/"+itemID.String()+"/reconcile", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if store.lastReconcileIn.itemID != itemID {
		t.Errorf("expected itemID %s, got %s", itemID, store.lastReconcileIn.itemID)
	}
	if store.lastReconcileIn.newStatus != domain.PayoutSucceeded {
		t.Errorf("expected newStatus succeeded, got %s", store.lastReconcileIn.newStatus)
	}
	if store.lastReconcileIn.utr == nil || *store.lastReconcileIn.utr != "UTR-BANK-123456" {
		t.Errorf("unexpected utr: %v", store.lastReconcileIn.utr)
	}
	if store.lastReconcileIn.operatorID != opsUser.ID {
		t.Errorf("expected operatorID %s, got %s", opsUser.ID, store.lastReconcileIn.operatorID)
	}
	if store.lastReconcileIn.notes != "Reconciled against HDFC bank statement #20261101" {
		t.Errorf("unexpected notes: %s", store.lastReconcileIn.notes)
	}

	// 2. Not found
	store.reconcileErr = domain.ErrNotFound
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/platform/payouts/items/"+uuid.New().String()+"/reconcile", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 on ErrNotFound, got %d", w.Code)
	}

	// 3. Transition error
	store.reconcileErr = errors.New("cannot transition payout item in terminal succeeded status to failed")
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/platform/payouts/items/"+itemID.String()+"/reconcile", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on invalid transition, got %d", w.Code)
	}
}
