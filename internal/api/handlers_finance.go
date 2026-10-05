package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/intelligence"
	"github.com/pg-cashflow/pg-go/internal/roi"
)

func (h *Handlers) financeReady(c *gin.Context) bool {
	if h.Finance == nil || !h.FinanceEnabled {
		respondErr(c, financeClientErr(finance.ErrDisabled))
		return false
	}
	return true
}

func requireIdem(c *gin.Context) (string, bool) {
	k := c.GetHeader("Idempotency-Key")
	if k == "" {
		respondErr(c, financeClientErr(finance.ErrIdempotencyRequired))
		return "", false
	}
	return k, true
}

func (h *Handlers) FinanceSummary(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	period := c.DefaultQuery("period", time.Now().UTC().Format("2006-01"))
	recon, err := h.Payments.BuildSummary(c.Request.Context(), pid, period)
	if err != nil {
		respondErr(c, err)
		return
	}
	sum, err := h.Finance.OperatingSummary(c.Request.Context(), pid, period, recon.RentCollected)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"period":          period,
		"collections":     recon,
		"operating":       sum,
		"query_keys":      []string{"owner", "finance", "summary", period},
		"invalidate_with": []string{"owner.reconciliation", "owner.finance.summary"},
	})
}

func (h *Handlers) FinanceLedger(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	from, to, err := finance.PeriodBounds(c.DefaultQuery("period", time.Now().UTC().Format("2006-01")))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "period must be YYYY-MM"})
		return
	}
	if f := c.Query("from"); f != "" {
		if t, e := time.Parse("2006-01-02", f); e == nil {
			from = t
		}
	}
	if tq := c.Query("to"); tq != "" {
		if t, e := time.Parse("2006-01-02", tq); e == nil {
			to = t
		}
	}
	lines, err := h.Finance.Store.ListJournal(c.Request.Context(), pid, from, to, c.Query("account"))
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": lines})
}

func (h *Handlers) PostCapital(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	idem, ok := requireIdem(c)
	if !ok {
		return
	}
	var body struct {
		Kind        domain.CapitalKind `json:"kind"`
		AmountPaise int64              `json:"amount_paise"`
		Purpose     string             `json:"purpose"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	tx, err := h.Finance.AddCapital(c.Request.Context(), pid, uid, body.Kind, body.AmountPaise, body.Purpose, idem)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusCreated, gin.H{"capital": tx})
}

func (h *Handlers) ListCapital(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	list, err := h.Finance.Store.ListCapital(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"capital": list})
}

func (h *Handlers) PostExpense(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	idem, ok := requireIdem(c)
	if !ok {
		return
	}
	claims, _ := auth.ClaimsFromContext(c)
	var body struct {
		CategoryCode string     `json:"category_code"`
		VendorName   string     `json:"vendor_name"`
		Description  string     `json:"description"`
		AmountPaise  int64      `json:"amount_paise"`
		Emergency    bool       `json:"emergency"`
		IsRecurring  bool       `json:"is_recurring"`
		RoomID       *uuid.UUID `json:"room_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if body.AmountPaise <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount_paise must be greater than zero"})
		return
	}
	if body.CategoryCode == "" {
		body.CategoryCode = "vendor"
	}
	e, appr, err := h.Finance.CreateExpense(c.Request.Context(), finance.CreateExpenseInput{
		PropertyID: pid, ActorID: uid, ActorRole: string(claims.Role),
		CategoryCode: body.CategoryCode, VendorName: body.VendorName, Description: body.Description,
		AmountPaise: body.AmountPaise, Emergency: body.Emergency, IsRecurring: body.IsRecurring, RoomID: body.RoomID, IdempotencyKey: idem,
	})
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusCreated, gin.H{"expense": e, "approval": appr})
}

func (h *Handlers) ListExpenses(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	list, err := h.Finance.Store.ListExpenses(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"expenses": list})
}

func (h *Handlers) PostExpensePayment(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	idem, ok := requireIdem(c)
	if !ok {
		return
	}
	eid, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	claims, _ := auth.ClaimsFromContext(c)
	var body struct {
		AmountPaise int64  `json:"amount_paise"`
		Method      string `json:"method"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	role := domain.PayerOwner
	if claims.Role == domain.RoleManager {
		role = domain.PayerManager
	}
	p, err := h.Finance.PayExpense(c.Request.Context(), finance.PayExpenseInput{
		ExpenseID: eid, PropertyID: pid, ActorID: uid, ActorRole: role,
		AmountPaise: body.AmountPaise, Method: body.Method, IdempotencyKey: idem,
	})
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusCreated, gin.H{"payment": p})
}

func (h *Handlers) ListAdvances(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	list, err := h.Finance.Store.ListAdvances(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	out, _ := h.Finance.Store.AdvanceOutstanding(c.Request.Context(), pid)
	c.JSON(http.StatusOK, gin.H{"advances": list, "outstanding_paise": out})
}

func (h *Handlers) PostReimbursement(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	idem, ok := requireIdem(c)
	if !ok {
		return
	}
	var body struct {
		ManagerUserID uuid.UUID `json:"manager_user_id"`
		AmountPaise   int64     `json:"amount_paise"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	r, err := h.Finance.ReimburseManager(c.Request.Context(), pid, uid, body.ManagerUserID, body.AmountPaise, idem)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusCreated, gin.H{"reimbursement": r})
}

func (h *Handlers) ListBudgets(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	list, err := h.Finance.Store.ListBudgets(c.Request.Context(), pid, c.Query("period"))
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"budgets": list})
}

func (h *Handlers) PostBudget(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var b domain.Budget
	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	b.PropertyID = pid
	if err := h.Finance.SaveBudget(c.Request.Context(), &b); err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusCreated, gin.H{"budget": b})
}

func (h *Handlers) PatchBudget(c *gin.Context) {
	h.PostBudget(c)
}

func (h *Handlers) GetFinanceSettings(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	// EnsureDefaults is idempotent seed-on-first-access; a failure here is non-fatal
	// because GetSettings below will still return a usable zero-value row.
	_ = h.Finance.Store.EnsureDefaults(c.Request.Context(), pid)
	st, err := h.Finance.Store.GetSettings(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	pol, err := h.Finance.Store.GetPolicy(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	var gset *domain.PropertyGamificationSettings
	if h.GamificationStore != nil {
		gset, _ = h.GamificationStore.GetSettings(c.Request.Context(), pid)
	}
	c.JSON(http.StatusOK, gin.H{"settings": st, "policy": pol, "loyalty": gset})
}

func (h *Handlers) PatchFinanceSettings(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var body struct {
		Settings *domain.PropertyFinanceSettings      `json:"settings"`
		Policy   *domain.ApprovalPolicy               `json:"policy"`
		Loyalty  *domain.PropertyGamificationSettings `json:"loyalty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	st, pol, loy, err := h.Finance.PatchUnifiedSettings(c.Request.Context(), pid, body.Settings, body.Policy, body.Loyalty)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	res := gin.H{"settings": st, "policy": pol}
	if loy != nil {
		res["loyalty"] = loy
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handlers) ListFinanceApprovals(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	list, err := h.Finance.Store.ListApprovals(c.Request.Context(), pid, c.Query("status"))
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"approvals": list})
}

func (h *Handlers) DecideFinanceApproval(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
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
	approve := c.Param("action") == "approve"
	var body struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&body)
	if err := h.Finance.DecideApproval(c.Request.Context(), pid, uid, id, approve, body.Note); err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) FinanceTieOut(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	period := c.DefaultQuery("period", time.Now().UTC().Format("2006-01"))
	recon, err := h.Payments.BuildSummary(c.Request.Context(), pid, period)
	if err != nil {
		respondErr(c, err)
		return
	}
	t, err := h.Finance.ComputeTieOut(c.Request.Context(), pid, period, recon)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"tie_out": t, "label": "Collections tie-out"})
}

func (h *Handlers) CloseFinanceTieOut(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	period := c.DefaultQuery("period", time.Now().UTC().Format("2006-01"))
	t, err := h.Finance.CloseTieOut(c.Request.Context(), pid, period)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"tie_out": t})
}

type ReopenTieOutRequest struct {
	Period string `json:"period" binding:"required"`
	Reason string `json:"reason" binding:"required"`
	StepUpAuthInput
}

func (h *Handlers) ReopenFinanceTieOut(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	claims, _ := auth.ClaimsFromContext(c)
	uid := claims.UserID

	var req ReopenTieOutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierr.RespondBindErr(c, "period and reason are required", apierr.CodeRequestInvalidBody)
		return
	}
	req.Period = strings.TrimSpace(req.Period)
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Period == "" || req.Reason == "" {
		apierr.RespondBindErr(c, "period and reason cannot be empty", apierr.CodeRequestInvalidBody)
		return
	}

	// Enforce cryptographic step-up OTP / dual control before an owner can reopen a closed period
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, req.StepUpAuthInput); !ok {
		return
	}

	actor := fmt.Sprintf("owner:%s: %s", uid, req.Reason)
	t, err := h.Finance.ReopenTieOut(c.Request.Context(), pid, req.Period, actor, req.Reason)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"tie_out": t,
		"status":  "reopened",
	})
}

func (h *Handlers) OwnerStatementIncome(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	period := c.DefaultQuery("period", time.Now().UTC().Format("2006-01"))
	from, to, err := finance.PeriodBounds(period)
	if err != nil {
		apierr.RespondBindErr(c, "invalid period, expected YYYY-MM", apierr.CodeRequestInvalidBody)
		return
	}
	lines, err := h.Finance.IncomeStatement(c.Request.Context(), pid, from, to)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"period": period,
		"from":   from,
		"to":     to,
		"lines":  lines,
	})
}

func (h *Handlers) OwnerStatementBalanceSheet(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var to time.Time
	if p := c.Query("period"); p != "" {
		_, periodTo, err := finance.PeriodBounds(p)
		if err != nil {
			apierr.RespondBindErr(c, "invalid period, expected YYYY-MM", apierr.CodeRequestInvalidBody)
			return
		}
		to = periodTo
	} else if toStr := c.Query("to"); toStr != "" {
		parsed, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			apierr.RespondBindErr(c, "invalid to timestamp, expected RFC3339", apierr.CodeRequestInvalidBody)
			return
		}
		to = parsed
	} else {
		to = time.Now().UTC()
	}
	lines, err := h.Finance.BalanceSheet(c.Request.Context(), pid, to)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"as_of": to,
		"lines": lines,
	})
}

func (h *Handlers) OwnerStatementCashFlow(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	period := c.DefaultQuery("period", time.Now().UTC().Format("2006-01"))
	from, to, err := finance.PeriodBounds(period)
	if err != nil {
		apierr.RespondBindErr(c, "invalid period, expected YYYY-MM", apierr.CodeRequestInvalidBody)
		return
	}
	lines, err := h.Finance.CashFlow(c.Request.Context(), pid, from, to)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"period": period,
		"from":   from,
		"to":     to,
		"lines":  lines,
	})
}

func (h *Handlers) OwnerStatementTrialBalance(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var to time.Time
	if p := c.Query("period"); p != "" {
		_, periodTo, err := finance.PeriodBounds(p)
		if err != nil {
			apierr.RespondBindErr(c, "invalid period, expected YYYY-MM", apierr.CodeRequestInvalidBody)
			return
		}
		to = periodTo
	} else if toStr := c.Query("to"); toStr != "" {
		parsed, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			apierr.RespondBindErr(c, "invalid to timestamp, expected RFC3339", apierr.CodeRequestInvalidBody)
			return
		}
		to = parsed
	} else {
		to = time.Now().UTC()
	}
	lines, err := h.Finance.TrialBalance(c.Request.Context(), pid, to)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"as_of": to,
		"lines": lines,
	})
}

func (h *Handlers) OwnerReconcilingItems(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var asOf time.Time
	if asOfStr := c.Query("as_of"); asOfStr != "" {
		parsed, err := time.Parse("2006-01-02", asOfStr)
		if err != nil {
			apierr.RespondBindErr(c, "invalid as_of date, expected YYYY-MM-DD", apierr.CodeRequestInvalidBody)
			return
		}
		asOf = parsed
	} else {
		asOf = time.Now().UTC()
	}
	items, err := h.Finance.ReconcilingItems(c.Request.Context(), pid, asOf)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"as_of": asOf.Format("2006-01-02"),
		"items": items,
	})
}

func (h *Handlers) FinanceVarianceBridge(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	period := c.DefaultQuery("period", time.Now().UTC().Format("2006-01"))
	recon, err := h.Payments.BuildSummary(c.Request.Context(), pid, period)
	if err != nil {
		respondErr(c, err)
		return
	}
	occ := h.occupancy(c, pid)
	br, err := h.Finance.VarianceBridge(c.Request.Context(), pid, period, recon, occ, recon.RentCollected+recon.OutstandingRent)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"variance_bridge": br, "label": "Plan vs actual"})
}

func (h *Handlers) occupancy(c *gin.Context, pid uuid.UUID) finance.Occupancy {
	var rooms []domain.Room
	if h.GamificationStore != nil {
		rooms, _ = h.GamificationStore.ListRooms(c.Request.Context(), pid)
	}
	tenants, _ := h.TenantStore.ListByProperty(c.Request.Context(), pid)
	return finance.ComputeOccupancy(rooms, tenants, time.Now().UTC())
}

func (h *Handlers) OwnerROI(c *gin.Context) {
	if !h.financeReady(c) || h.ROI == nil {
		if h.ROI == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "roi unavailable"})
			return
		}
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	period := c.DefaultQuery("period", time.Now().UTC().Format("2006-01"))
	recon, err := h.Payments.BuildSummary(c.Request.Context(), pid, period)
	if err != nil {
		respondErr(c, err)
		return
	}
	var rooms []domain.Room
	if h.GamificationStore != nil {
		rooms, _ = h.GamificationStore.ListRooms(c.Request.Context(), pid)
	}
	tenants, _ := h.TenantStore.ListByProperty(c.Request.Context(), pid)
	rep, err := h.ROI.Report(c.Request.Context(), pid, period, recon, rooms, tenants, 0, 0, 0)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"roi": rep})
}

func (h *Handlers) OwnerROIRecovery(c *gin.Context) {
	h.OwnerROI(c)
}

func (h *Handlers) OwnerROIBreakEven(c *gin.Context) {
	h.OwnerROI(c)
}

func (h *Handlers) OwnerROIScenario(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	var in roi.ScenarioInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"scenario": roi.Simulate(in)})
}

func (h *Handlers) ManagerFinanceToday(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	claims, _ := auth.ClaimsFromContext(c)
	st, _ := h.Finance.Store.GetSettings(c.Request.Context(), pid)
	if claims.Role == domain.RoleManager && !st.ManagerCanViewROI {
		// still return ops digest without capital
	}
	exps, _ := h.Finance.Store.ListExpenses(c.Request.Context(), pid)
	appr, _ := h.Finance.Store.ListApprovals(c.Request.Context(), pid, "pending")
	var head any
	if h.Gamification != nil {
		head, _ = h.Gamification.GetHeadcountReport(c.Request.Context(), pid, time.Now().UTC())
	}
	c.JSON(http.StatusOK, gin.H{
		"expenses":          exps,
		"pending_approvals": appr,
		"kitchen_headcount": head,
		"hide_capital":      claims.Role == domain.RoleManager && !st.ManagerCanViewCapital,
	})
}

func (h *Handlers) InsightsSummary(c *gin.Context) {
	if h.Intelligence == nil || !h.IntelligenceEnabled {
		c.JSON(http.StatusOK, gin.H{"insights": []any{}, "status": "not_enabled"})
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	leaks, _ := h.Finance.Store.ListLeakage(c.Request.Context(), pid)
	recs, _ := h.Finance.Store.ListRecommendations(c.Request.Context(), pid)
	var total int64
	for _, l := range leaks {
		total += l.EstimatedPaise
	}
	c.JSON(http.StatusOK, gin.H{
		"leakage":         leaks,
		"recommendations": recs,
		"coi_6m_paise":    intelligence.COI(total, 6),
		"query_keys":      []string{"owner", "insights"},
	})
}

func (h *Handlers) ListLeakage(c *gin.Context) {
	h.InsightsSummary(c)
}

func (h *Handlers) GetLeakage(c *gin.Context) {
	if h.Finance == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unavailable"})
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	e, err := h.Finance.Store.GetLeakage(c.Request.Context(), id)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	if e.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"leakage": e})
}

func (h *Handlers) ListRecommendations(c *gin.Context) {
	if h.Finance == nil {
		c.JSON(http.StatusOK, gin.H{"recommendations": []any{}})
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	list, err := h.Finance.Store.ListRecommendations(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"recommendations": list})
}

func (h *Handlers) RecommendationAction(c *gin.Context) {
	if h.Finance == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unavailable"})
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	rec, err := h.Finance.Store.GetRecommendation(c.Request.Context(), id)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	if rec.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	now := time.Now().UTC()
	switch c.Param("action") {
	case "accept":
		rec.Status = "accepted"
		rec.AcceptedAt = &now
	case "reject":
		rec.Status = "rejected"
	case "complete":
		rec.Status = "completed"
		rec.CompletedAt = &now
		var body struct {
			RealizedPaise int64 `json:"realized_savings_paise"`
		}
		_ = c.ShouldBindJSON(&body)
		if body.RealizedPaise != 0 {
			rec.RealizedSavingsPaise = &body.RealizedPaise
		}
	}
	if err := h.Finance.Store.UpdateRecommendation(c.Request.Context(), rec); err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"recommendation": rec})
}

func (h *Handlers) OwnerCOI(c *gin.Context) {
	h.InsightsSummary(c)
}

func (h *Handlers) OwnerForecast(c *gin.Context) {
	if h.Finance == nil {
		c.JSON(http.StatusOK, gin.H{"forecast": []int64{}})
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	period := time.Now().UTC().Format("2006-01")
	recon, err := h.Payments.BuildSummary(c.Request.Context(), pid, period)
	if err != nil {
		respondErr(c, err)
		return
	}
	sum, err := h.Finance.OperatingSummary(c.Request.Context(), pid, period, recon.RentCollected)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	series := intelligence.ForecastOCF(sum.OCFPaise, 200, 12)
	payload, _ := json.Marshal(series)
	// InsertForecast is a best-effort cache write; a failure here must not block
	// the forecast response already computed in memory.
	_ = h.Finance.Store.InsertForecast(c.Request.Context(), &domain.ForecastSnapshot{
		PropertyID: pid, HorizonDays: 365, AsOf: time.Now().UTC(), Payload: payload,
	})
	c.JSON(http.StatusOK, gin.H{"forecast_ocf_paise": series, "horizon_months": 12})
}

func (h *Handlers) ListExpenseImports(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	list, err := h.Finance.Store.ListImportSuggestions(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"suggestions": list})
}

func (h *Handlers) PostMealPrep(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var m domain.MealPrepActual
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	m.PropertyID = pid
	if err := h.Finance.Store.UpsertMealPrep(c.Request.Context(), &m); err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"prep": m})
}

// OwnerClearingDriftReport handles GET /owner/finance/reports/clearing-drift.
func (h *Handlers) OwnerClearingDriftReport(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	rep, err := h.Finance.GetClearingDriftReport(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, rep)
}

// OwnerDepositReserveReport handles GET /owner/finance/reports/deposit-reserve.
func (h *Handlers) OwnerDepositReserveReport(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	rep, err := h.Finance.GetDepositReserveReport(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, rep)
}

// OwnerCapitalPaybackReport handles GET /owner/finance/reports/capital-payback.
func (h *Handlers) OwnerCapitalPaybackReport(c *gin.Context) {
	if !h.financeReady(c) {
		return
	}
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	rep, err := h.Finance.GetCapitalPaybackReport(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, financeClientErr(err))
		return
	}
	c.JSON(http.StatusOK, rep)
}

