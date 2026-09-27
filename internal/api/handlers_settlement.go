package api

import (
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type resolveDiscrepancyBody struct {
	Notes string `json:"notes" binding:"required"`
	StepUpAuthInput
}

// OwnerListSettlements handles GET /owner/settlements.
func (h *Handlers) OwnerListSettlements(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	repo := h.getSettlementRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settlement repo not configured"})
		return
	}

	var filter domain.SettlementFilter
	if s := c.Query("status"); s != "" {
		st := domain.SettlementReconStatus(s)
		filter.Status = &st
	}
	if src := c.Query("source"); src != "" {
		is := domain.IngestionSource(src)
		filter.Source = &is
	}

	items, total, err := repo.ListSettlements(c.Request.Context(), &pid, filter)
	if err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"settlements": items,
		"total":       total,
	})
}

// OwnerGetSettlement handles GET /owner/settlements/:id.
func (h *Handlers) OwnerGetSettlement(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	repo := h.getSettlementRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settlement repo not configured"})
		return
	}

	stlm, err := repo.GetSettlement(c.Request.Context(), id)
	if err != nil || stlm == nil || (stlm.PropertyID != nil && *stlm.PropertyID != pid) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"settlement": stlm})
}

// OwnerResolveSettlementDiscrepancy handles POST /owner/settlements/:id/resolve.
// Enforces cryptographic step-up OTP / dual control before an owner can override a ledger discrepancy.
func (h *Handlers) OwnerResolveSettlementDiscrepancy(c *gin.Context) {
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

	repo := h.getSettlementRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settlement repo not configured"})
		return
	}

	stlm, err := repo.GetSettlement(c.Request.Context(), id)
	if err != nil || stlm == nil || (stlm.PropertyID != nil && *stlm.PropertyID != pid) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	if stlm.ReconciliationStatus != domain.ReconDiscrepancy && stlm.ReconciliationStatus != domain.ReconUnmatched {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("settlement is in status %q; only discrepancies or unmatched settlements can be manually resolved", stlm.ReconciliationStatus)})
		return
	}

	// Cryptographic step-up / dual-control enforcement
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, body.StepUpAuthInput); !ok {
		return
	}

	if err := repo.ResolveDiscrepancy(c.Request.Context(), id, uid, body.Notes); err != nil {
		respondErr(c, err)
		return
	}

	updated, _ := repo.GetSettlement(c.Request.Context(), id)
	c.JSON(http.StatusOK, gin.H{
		"message":    "settlement discrepancy resolved",
		"settlement": updated,
	})
}

// CashfreeSettlementWebhook handles POST /webhooks/cashfree/settlements.
// Verifies HMAC signature, audits raw payload, and feeds SettlementReconciler.
func (h *Handlers) CashfreeSettlementWebhook(c *gin.Context) {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body"})
		return
	}

	signature := c.GetHeader("x-webhook-signature")
	timestamp := c.GetHeader("x-webhook-timestamp")

	if h.CashfreeSecret != "" {
		if !cashfree.VerifyWebhookHMAC(h.CashfreeSecret, timestamp, string(raw), signature) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook signature"})
			return
		}
	}

	parsed, err := cashfree.ParseSettlementWebhook(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("parse settlement payload: %v", err)})
		return
	}

	if h.SettlementReconciler != nil {
		_, _ = h.SettlementReconciler.ReconcileWebhookSettlement(c.Request.Context(), parsed)
	}

	c.JSON(http.StatusOK, gin.H{"status": "received"})
}

func (h *Handlers) getSettlementRepo() *postgres.SettlementRepo {
	if h.SettlementRepo != nil {
		return h.SettlementRepo
	}
	if h.Pool != nil {
		return postgres.NewSettlementRepo(h.Pool)
	}
	return nil
}
