package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
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
// Enforces fail-closed HMAC signature check, timestamp tolerance, audit logging in webhook_events,
// and ensures reconciler errors propagate to trigger gateway retry.
func (h *Handlers) CashfreeSettlementWebhook(c *gin.Context) {
	// Cap settlement webhook payload read to 256 KB
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 256*1024))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body"})
		return
	}

	// 1. Fail closed if CashfreeSecret is unconfigured
	if h.CashfreeSecret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment gateway not configured"})
		return
	}

	signature := c.GetHeader("x-webhook-signature")
	timestamp := c.GetHeader("x-webhook-timestamp")

	// 2. Cryptographic HMAC verification
	if !cashfree.VerifyWebhookHMAC(h.CashfreeSecret, timestamp, string(raw), signature) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook signature"})
		return
	}

	// 3. Timestamp drift / replay check
	tolerance := h.WebhookToleranceSec
	if tolerance <= 0 {
		tolerance = 300
	}
	if err := cashfree.VerifyWebhookTimestamp(timestamp, tolerance, time.Now().UTC()); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp tolerance exceeded"})
		return
	}

	// 4. Audit raw payload into webhook_events
	if h.GatewayPaymentRepo != nil {
		if aerr := h.GatewayPaymentRepo.CreateWebhookEvent(c.Request.Context(), &domain.WebhookEvent{
			Provider:         "cashfree",
			Signature:        &signature,
			TimestampHeader:  &timestamp,
			RawPayload:       raw,
			ProcessingStatus: "received",
		}); aerr != nil {
			slog.Warn("failed to persist settlement webhook audit event", "error", aerr)
		}
	}

	// 5. Parse payload with sanitized error
	parsed, err := cashfree.ParseSettlementWebhook(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid settlement payload"})
		return
	}

	// 6. Feed SettlementReconciler with fail-closed retry propagation
	if h.SettlementReconciler != nil {
		if _, err := h.SettlementReconciler.ReconcileWebhookSettlement(c.Request.Context(), parsed); err != nil {
			slog.Error("failed to reconcile settlement webhook", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "settlement reconciliation failure"})
			return
		}
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

type runEODBalanceBody struct {
	Date string `json:"date"`
}

func (h *Handlers) getSettlementBalancer() *finance.SettlementBalancer {
	if h.SettlementBalancer != nil {
		return h.SettlementBalancer
	}
	if h.SettlementBalancerRepo != nil {
		return finance.NewSettlementBalancer(h.SettlementBalancerRepo)
	}
	if h.Pool != nil {
		return finance.NewSettlementBalancer(postgres.NewSettlementBalancerRepo(h.Pool))
	}
	return nil
}

// OwnerGetEODBalance handles GET /owner/settlements/eod-balance?date=YYYY-MM-DD
func (h *Handlers) OwnerGetEODBalance(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	balancer := h.getSettlementBalancer()
	if balancer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settlement balancer not configured"})
		return
	}

	dateStr := c.Query("date")
	reconDate := time.Now().UTC()
	if dateStr != "" {
		parsed, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date format, expected YYYY-MM-DD"})
			return
		}
		reconDate = parsed
	}

	bal, err := balancer.GetDailyBalance(c.Request.Context(), pid, reconDate)
	if err != nil {
		if errors.Is(err, postgres.ErrDailyBalanceNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "eod balance not found for specified date"})
			return
		}
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, bal)
}

// OwnerRunEODBalance handles POST /owner/settlements/eod-balance/run
func (h *Handlers) OwnerRunEODBalance(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	balancer := h.getSettlementBalancer()
	if balancer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settlement balancer not configured"})
		return
	}

	var body runEODBalanceBody
	_ = c.ShouldBindJSON(&body)

	reconDate := time.Now().UTC()
	if body.Date != "" {
		parsed, err := time.Parse("2006-01-02", body.Date)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid date format, expected YYYY-MM-DD"})
			return
		}
		reconDate = parsed
	}

	bal, err := balancer.RunDailyBalance(c.Request.Context(), pid, reconDate)
	if err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, bal)
}

// OwnerListEODBalances handles GET /owner/settlements/eod-balance/history
func (h *Handlers) OwnerListEODBalances(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	balancer := h.getSettlementBalancer()
	if balancer == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settlement balancer not configured"})
		return
	}

	limit := 30
	offset := 0
	if l := c.Query("limit"); l != "" {
		if val, err := strconv.Atoi(l); err == nil && val > 0 {
			limit = val
		}
	}
	if o := c.Query("offset"); o != "" {
		if val, err := strconv.Atoi(o); err == nil && val >= 0 {
			offset = val
		}
	}

	list, err := balancer.ListDailyBalances(c.Request.Context(), pid, limit, offset)
	if err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"balances": list,
		"total":    len(list),
	})
}

