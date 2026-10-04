package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

type dummyPaymentSummaryBuilder struct{}

func (d *dummyPaymentSummaryBuilder) BuildSummary(_ context.Context, _ uuid.UUID, period string) (*payment.ReconciliationSummary, error) {
	return &payment.ReconciliationSummary{Period: period}, nil
}

func (d *dummyPaymentSummaryBuilder) MatchPayment(context.Context, uuid.UUID, string, int, time.Time, string) (*domain.Payment, error) {
	return nil, nil
}

func (d *dummyPaymentSummaryBuilder) SuggestMatch(context.Context, uuid.UUID, int, time.Time, string) (*payment.MatchResult, error) {
	return nil, nil
}

func (d *dummyPaymentSummaryBuilder) ManualMatch(context.Context, uuid.UUID, int, string, uuid.UUID) (*domain.Payment, error) {
	return nil, nil
}

func (d *dummyPaymentSummaryBuilder) MarkCashPaid(context.Context, uuid.UUID, int, uuid.UUID, string) (*domain.Payment, error) {
	return nil, nil
}

func (d *dummyPaymentSummaryBuilder) SettleDeposit(context.Context, uuid.UUID, int64, string) error {
	return nil
}

func (d *dummyPaymentSummaryBuilder) GatewaySettle(context.Context, uuid.UUID, int, string, ...string) (*domain.Payment, error) {
	return nil, nil
}

func TestOwnerFinancialStatementsAndReopenEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"

	propID := uuid.New()
	ownerID := uuid.New()
	ownerToken, _ := auth.IssueToken(jwtSecret, &domain.User{
		ID:         ownerID,
		Role:       domain.RoleOwner,
		PropertyID: &propID,
	})

	memStore := finance.NewMemoryStore()
	finSvc := finance.NewService(memStore, nil)
	finSvc.Now = func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }

	deps := Deps{
		JWTSecret:      jwtSecret,
		Finance:        finSvc,
		FinanceEnabled: true,
		Payments:       &dummyPaymentSummaryBuilder{},
	}
	router := NewRouter(deps)

	// 1. GET /api/owner/finance/statements/income-statement
	{
		req := httptest.NewRequest(http.MethodGet, "/api/owner/finance/statements/income-statement?period=2026-09", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for income-statement, got %d (body: %s)", w.Code, w.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal income-statement response: %v", err)
		}
		if resp["period"] != "2026-09" {
			t.Fatalf("expected period 2026-09, got %v", resp["period"])
		}
	}

	// 2. GET /api/owner/finance/statements/balance-sheet
	{
		req := httptest.NewRequest(http.MethodGet, "/api/owner/finance/statements/balance-sheet?period=2026-09", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for balance-sheet, got %d (body: %s)", w.Code, w.Body.String())
		}
	}

	// 3. GET /api/owner/finance/statements/cash-flow
	{
		req := httptest.NewRequest(http.MethodGet, "/api/owner/finance/statements/cash-flow?period=2026-09", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for cash-flow, got %d (body: %s)", w.Code, w.Body.String())
		}
	}

	// 4. GET /api/owner/finance/statements/trial-balance
	{
		req := httptest.NewRequest(http.MethodGet, "/api/owner/finance/statements/trial-balance?period=2026-09", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for trial-balance, got %d (body: %s)", w.Code, w.Body.String())
		}
	}

	// 5. GET /api/owner/finance/reconciling-items
	{
		req := httptest.NewRequest(http.MethodGet, "/api/owner/finance/reconciling-items?as_of=2026-09-30", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for reconciling-items, got %d (body: %s)", w.Code, w.Body.String())
		}
	}

	// 6. POST /api/owner/finance/tie-out/reopen on an open period -> 400 Bad Request
	{
		// First compute tie out (creates open tie out)
		_, err := finSvc.ComputeTieOut(context.Background(), propID, "2026-09", &payment.ReconciliationSummary{Period: "2026-09"})
		if err != nil {
			t.Fatalf("compute tie-out: %v", err)
		}

		body := map[string]string{
			"period": "2026-09",
			"reason": "Auditor adjustment for past period",
		}
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/finance/tie-out/reopen", bytes.NewBuffer(raw))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 when reopening an open period, got %d (body: %s)", w.Code, w.Body.String())
		}
		var errResp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &errResp)
		if errResp["code"] != "finance.periodNotReopenable" {
			t.Fatalf("expected code finance.periodNotReopenable, got %v", errResp["code"])
		}
	}

	// 7. Close period, then reopen successfully
	{
		_, err := finSvc.CloseTieOut(context.Background(), propID, "2026-09")
		if err != nil {
			t.Fatalf("close tie-out: %v", err)
		}

		body := map[string]string{
			"period": "2026-09",
			"reason": "Auditor adjustment for past period",
		}
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/owner/finance/tie-out/reopen", bytes.NewBuffer(raw))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 when reopening closed period, got %d (body: %s)", w.Code, w.Body.String())
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["status"] != "reopened" {
			t.Fatalf("expected status reopened, got %v", resp["status"])
		}
	}
}
