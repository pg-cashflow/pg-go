package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestGetPaymentRefundHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)

	propID := uuid.New()
	paymentID := uuid.New()
	tenantID := uuid.New()
	ownerID := uuid.New()

	basePayment := &domain.Payment{
		ID:         paymentID,
		PropertyID: &propID,
		TenantID:   tenantID,
		Amount:     5000,
	}

	refRef1 := "rf_test_history_1"
	refRef2 := "rf_test_history_2"
	now := time.Now().UTC()

	refund1 := &domain.GatewayRefund{
		ID:              uuid.New(),
		PaymentID:       paymentID,
		PropertyID:      propID,
		RefundReference: &refRef1,
		AmountPaise:     1500,
		Status:          domain.RefundStatusSucceeded,
		Reason:          "Deposit refund",
		CreatedAt:       now.Add(-2 * time.Hour),
		UpdatedAt:       now.Add(-2 * time.Hour),
	}

	refund2 := &domain.GatewayRefund{
		ID:              uuid.New(),
		PaymentID:       paymentID,
		PropertyID:      propID,
		RefundReference: &refRef2,
		AmountPaise:     1000,
		Status:          domain.RefundStatusPending,
		Reason:          "Maintenance credit",
		CreatedAt:       now.Add(-1 * time.Hour),
		UpdatedAt:       now.Add(-1 * time.Hour),
	}

	repo := newMockRefundPaymentRepo()
	repo.payments[paymentID] = basePayment
	require.NoError(t, repo.CreateOrUpdateRefund(context.Background(), refund1))
	require.NoError(t, repo.CreateOrUpdateRefund(context.Background(), refund2))

	tenantStore := &mockRefundTenantStore{
		t: &domain.Tenant{
			ID:         tenantID,
			PropertyID: propID,
		},
	}

	h := &Handlers{
		Deps: Deps{
			GatewayPaymentRepo: repo,
			TenantStore:        tenantStore,
		},
	}

	router := gin.New()
	ownerGroup := router.Group("/api/owner")
	ownerGroup.Use(func(c *gin.Context) {
		tokenHeader := c.GetHeader("X-Test-Role")
		if tokenHeader == "other_owner" {
			otherProp := uuid.New()
			c.Set(auth.ContextClaimsKey, &auth.Claims{
				UserID:     uuid.New(),
				PropertyID: &otherProp,
				Role:       domain.RoleOwner,
			})
		} else {
			c.Set(auth.ContextClaimsKey, &auth.Claims{
				UserID:     ownerID,
				PropertyID: &propID,
				Role:       domain.RoleOwner,
			})
		}
		c.Next()
	})
	ownerGroup.GET("/payments/:id/refunds", h.GetPaymentRefundHistory)

	t.Run("Authorized owner retrieves refund history and remaining refundable balance", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/"+paymentID.String()+"/refunds", nil)
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		var resp struct {
			PaymentID                uuid.UUID `json:"payment_id"`
			OriginalAmountPaise      int64     `json:"original_amount_paise"`
			RefundedAmountPaise      int64     `json:"refunded_amount_paise"`
			ReservedAmountPaise      int64     `json:"reserved_amount_paise"`
			RemainingRefundablePaise int64     `json:"remaining_refundable_paise"`
			Refunds                  []struct {
				ID          uuid.UUID `json:"id"`
				AmountPaise int64     `json:"amount_paise"`
				Status      string    `json:"status"`
				Reason      string    `json:"reason"`
			} `json:"refunds"`
			PendingOperation *struct {
				ID          uuid.UUID `json:"id"`
				AmountPaise int64     `json:"amount_paise"`
				Status      string    `json:"status"`
			} `json:"pending_operation"`
		}
		err := json.Unmarshal(w.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, paymentID, resp.PaymentID)
		assert.Equal(t, int64(5000), resp.OriginalAmountPaise)
		assert.Equal(t, int64(1500), resp.RefundedAmountPaise)
		assert.Equal(t, int64(1000), resp.ReservedAmountPaise)
		assert.Equal(t, int64(2500), resp.RemainingRefundablePaise) // 5000 - (1500 + 1000)
		assert.Len(t, resp.Refunds, 2)
		require.NotNil(t, resp.PendingOperation)
		assert.Equal(t, refund2.ID, resp.PendingOperation.ID)
		assert.Equal(t, int64(1000), resp.PendingOperation.AmountPaise)
	})

	t.Run("Owner from another property is rejected (cross-property isolation)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/"+paymentID.String()+"/refunds", nil)
		req.Header.Set("X-Test-Role", "other_owner")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("Non-existent payment returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/owner/payments/"+uuid.New().String()+"/refunds", nil)
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}
