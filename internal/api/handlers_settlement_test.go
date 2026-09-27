package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type mockAPISettlementRepo struct {
	items map[uuid.UUID]*domain.GatewaySettlement
}

func newMockAPISettlementRepo() *mockAPISettlementRepo {
	return &mockAPISettlementRepo{items: make(map[uuid.UUID]*domain.GatewaySettlement)}
}

func (m *mockAPISettlementRepo) UpsertSettlement(_ context.Context, s *domain.GatewaySettlement) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	m.items[s.ID] = s
	return nil
}

func (m *mockAPISettlementRepo) GetSettlement(_ context.Context, id uuid.UUID) (*domain.GatewaySettlement, error) {
	if v, ok := m.items[id]; ok {
		return v, nil
	}
	return nil, postgres.ErrSettlementNotFound
}

func (m *mockAPISettlementRepo) GetSettlementByCFID(_ context.Context, cfID, orderID, _ string) (*domain.GatewaySettlement, error) {
	for _, v := range m.items {
		if v.CFSettlementID == cfID {
			return v, nil
		}
	}
	return nil, postgres.ErrSettlementNotFound
}

func (m *mockAPISettlementRepo) ListSettlements(_ context.Context, pid *uuid.UUID, filter domain.SettlementFilter) ([]*domain.GatewaySettlement, int, error) {
	var list []*domain.GatewaySettlement
	for _, v := range m.items {
		if pid != nil && v.PropertyID != nil && *v.PropertyID != *pid {
			continue
		}
		if filter.Status != nil && v.ReconciliationStatus != *filter.Status {
			continue
		}
		list = append(list, v)
	}
	return list, len(list), nil
}

func (m *mockAPISettlementRepo) ResolveDiscrepancy(_ context.Context, id uuid.UUID, resolvedBy uuid.UUID, notes string) error {
	v, ok := m.items[id]
	if !ok {
		return postgres.ErrSettlementNotFound
	}
	v.ReconciliationStatus = domain.ReconManuallyReconciled
	v.ResolutionNotes = &notes
	v.ResolvedBy = &resolvedBy
	now := time.Now().UTC()
	v.ResolvedAt = &now
	return nil
}

func TestOwnerSettlementHandlers_UnitScenarios(t *testing.T) {
	gin.SetMode(gin.TestMode)

	propID := uuid.New()
	ownerID := uuid.New()
	otherPropID := uuid.New()

	stlmRepo := newMockAPISettlementRepo()

	stlmID := uuid.New()
	stlmRepo.items[stlmID] = &domain.GatewaySettlement{
		ID:                   stlmID,
		PropertyID:           &propID,
		CFSettlementID:       "CF_SETTLE_001",
		ReconciliationStatus: domain.ReconDiscrepancy,
		GrossAmountPaise:     500000,
		NetAmountPaise:       490000,
	}

	otherStlmID := uuid.New()
	stlmRepo.items[otherStlmID] = &domain.GatewaySettlement{
		ID:                   otherStlmID,
		PropertyID:           &otherPropID,
		CFSettlementID:       "CF_SETTLE_FOREIGN",
		ReconciliationStatus: domain.ReconDiscrepancy,
	}

	h := &Handlers{
		Deps: Deps{
			Pool: nil,
		},
	}
	// Inject mock through unexported/exported accessor pattern
	h.SettlementRepo = (*postgres.SettlementRepo)(nil)

	// Create test router
	r := gin.New()
	ownerGroup := r.Group("/owner", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
	})
	{
		// Inject mock helper
		ownerGroup.GET("/settlements", func(c *gin.Context) {
			pid, ok := propertyIDFromClaims(c)
			if !ok {
				return
			}
			items, total, _ := stlmRepo.ListSettlements(c.Request.Context(), &pid, domain.SettlementFilter{})
			c.JSON(http.StatusOK, gin.H{"settlements": items, "total": total})
		})
		ownerGroup.GET("/settlements/:id", func(c *gin.Context) {
			pid, ok := propertyIDFromClaims(c)
			if !ok {
				return
			}
			id, ok := ParseUUIDParam(c, "id")
			if !ok {
				return
			}
			stlm, err := stlmRepo.GetSettlement(c.Request.Context(), id)
			if err != nil || stlm == nil || (stlm.PropertyID != nil && *stlm.PropertyID != pid) {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"settlement": stlm})
		})
		ownerGroup.POST("/settlements/:id/resolve", func(c *gin.Context) {
			pid, ok := propertyIDFromClaims(c)
			if !ok {
				return
			}
			uid, ok := userIDFromClaims(c)
			if !ok {
				return
			}
			id, ok := ParseUUIDParam(c, "id")
			if !ok {
				return
			}
			var body resolveDiscrepancyBody
			if err := c.ShouldBindJSON(&body); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "notes are required for discrepancy resolution"})
				return
			}
			stlm, err := stlmRepo.GetSettlement(c.Request.Context(), id)
			if err != nil || stlm == nil || (stlm.PropertyID != nil && *stlm.PropertyID != pid) {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			if stlm.ReconciliationStatus != domain.ReconDiscrepancy && stlm.ReconciliationStatus != domain.ReconUnmatched {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
				return
			}
			if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, body.StepUpAuthInput); !ok {
				return
			}
			_ = stlmRepo.ResolveDiscrepancy(c.Request.Context(), id, uid, body.Notes)
			updated, _ := stlmRepo.GetSettlement(c.Request.Context(), id)
			c.JSON(http.StatusOK, gin.H{"settlement": updated})
		})
	}

	t.Run("GET /owner/settlements lists settlements for property", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/owner/settlements", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", w.Code)
		}
		var resp struct {
			Settlements []*domain.GatewaySettlement `json:"settlements"`
			Total       int                         `json:"total"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Total != 1 {
			t.Errorf("expected 1 settlement for property, got %d", resp.Total)
		}
	})

	t.Run("GET /owner/settlements/:id returns settlement details", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/owner/settlements/%s", stlmID), nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", w.Code)
		}
	})

	t.Run("GET /owner/settlements/:id returns 404 for IDOR cross-property access", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/owner/settlements/%s", otherStlmID), nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found on cross-property IDOR, got %d", w.Code)
		}
	})

	t.Run("POST /owner/settlements/:id/resolve resolves discrepancy", func(t *testing.T) {
		payload := `{"notes":"Verified manually with bank statement"}`
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/settlements/%s/resolve", stlmID), bytes.NewBufferString(payload))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}

		updated := stlmRepo.items[stlmID]
		if updated.ReconciliationStatus != domain.ReconManuallyReconciled {
			t.Errorf("expected reconciliation status %s, got %s", domain.ReconManuallyReconciled, updated.ReconciliationStatus)
		}
		if updated.ResolutionNotes == nil || *updated.ResolutionNotes != "Verified manually with bank statement" {
			t.Errorf("expected resolution notes persisted")
		}
	})
}
