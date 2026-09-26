package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
)

func TestFinanceCrossPropertyIDORGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"

	propA := uuid.New()
	propB := uuid.New()

	ownerTokenA, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &propA})
	ownerTokenB, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &propB})

	memStore := finance.NewMemoryStore()
	finSvc := finance.NewService(memStore, nil)

	// Create leakage and recommendation belonging to Property B
	leakageB := &domain.LeakageEvent{
		ID:             uuid.New(),
		PropertyID:     propB,
		Category:       "electricity",
		EstimatedPaise: 50000,
	}
	_ = memStore.InsertLeakage(context.Background(), leakageB)

	recB := &domain.Recommendation{
		ID:                   uuid.New(),
		PropertyID:           propB,
		Issue:                "high electricity bill",
		SuggestedAction:      "replace faulty meter",
		ExpectedSavingsPaise: 25000,
		Status:               "open",
	}
	_ = memStore.InsertRecommendation(context.Background(), recB)

	deps := Deps{
		JWTSecret:      jwtSecret,
		Finance:        finSvc,
		FinanceEnabled: true,
	}
	router := NewRouter(deps)

	// 1. Owner A tries to read Property B's leakage event -> 404 Not Found
	req := httptest.NewRequest(http.MethodGet, "/api/owner/leakage/"+leakageB.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+ownerTokenA)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when Owner A accesses Property B leakage, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 2. Owner B can read Property B's leakage event -> 200 OK
	req2 := httptest.NewRequest(http.MethodGet, "/api/owner/leakage/"+leakageB.ID.String(), nil)
	req2.Header.Set("Authorization", "Bearer "+ownerTokenB)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 when Owner B accesses Property B leakage, got %d", w2.Code)
	}

	// 3. Owner A tries to accept Property B's recommendation -> 404 Not Found
	req3 := httptest.NewRequest(http.MethodPost, "/api/owner/recommendations/"+recB.ID.String()+"/accept", bytes.NewBufferString("{}"))
	req3.Header.Set("Authorization", "Bearer "+ownerTokenA)
	req3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when Owner A mutates Property B recommendation, got %d (body: %s)", w3.Code, w3.Body.String())
	}

	// 4. Owner B can accept Property B's recommendation -> 200 OK
	req4 := httptest.NewRequest(http.MethodPost, "/api/owner/recommendations/"+recB.ID.String()+"/accept", bytes.NewBufferString("{}"))
	req4.Header.Set("Authorization", "Bearer "+ownerTokenB)
	req4.Header.Set("Content-Type", "application/json")
	w4 := httptest.NewRecorder()
	router.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("expected 200 when Owner B mutates Property B recommendation, got %d", w4.Code)
	}
}
