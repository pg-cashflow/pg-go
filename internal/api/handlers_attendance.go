package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// ----------------------------------------------------------------------------
// Staff Profile Handlers
// ----------------------------------------------------------------------------

type createStaffBody struct {
	PayeeID              string  `json:"payee_id" binding:"required"`
	Name                 string  `json:"name" binding:"required"`
	Role                 string  `json:"role" binding:"required"`
	Phone                *string `json:"phone"`
	BaseMonthlyWagePaise int64   `json:"base_monthly_wage_paise" binding:"required"`
	EffectiveFrom        string  `json:"effective_from" binding:"required"` // "YYYY-MM-DD"
}

func (h *Handlers) OwnerCreateStaffProfile(c *gin.Context) {
	if h.AttendanceRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attendance service unavailable"})
		return
	}

	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	var body createStaffBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	payeeID, err := uuid.Parse(strings.TrimSpace(body.PayeeID))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payee_id"})
		return
	}

	effectiveFrom, err := time.Parse("2006-01-02", strings.TrimSpace(body.EffectiveFrom))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_from format (must be YYYY-MM-DD)"})
		return
	}

	if body.BaseMonthlyWagePaise <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "base_monthly_wage_paise must be positive"})
		return
	}

	staff := domain.StaffProfile{
		PropertyID:           pid,
		PayeeID:              payeeID,
		Name:                 strings.TrimSpace(body.Name),
		Role:                 strings.TrimSpace(body.Role),
		Phone:                body.Phone,
		BaseMonthlyWagePaise: body.BaseMonthlyWagePaise,
		EffectiveFrom:        effectiveFrom,
		Status:               domain.StaffActive,
	}

	created, err := h.AttendanceRepo.CreateStaffProfile(c.Request.Context(), staff)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create staff profile"})
		return
	}

	c.JSON(http.StatusCreated, created)
}

func (h *Handlers) OwnerListStaffProfiles(c *gin.Context) {
	if h.AttendanceRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attendance service unavailable"})
		return
	}

	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	onlyActive := c.Query("active_only") != "false"
	list, err := h.AttendanceRepo.ListStaffProfiles(c.Request.Context(), pid, onlyActive)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list staff profiles"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"staff": list})
}

type updateStaffStatusBody struct {
	Status      string  `json:"status" binding:"required"` // "active" | "inactive"
	EffectiveTo *string `json:"effective_to"`              // "YYYY-MM-DD"
}

func (h *Handlers) OwnerUpdateStaffProfileStatus(c *gin.Context) {
	if h.AttendanceRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attendance service unavailable"})
		return
	}

	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	staffID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid staff id"})
		return
	}

	var body updateStaffStatusBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	status := domain.StaffProfileStatus(strings.TrimSpace(body.Status))
	if status != domain.StaffActive && status != domain.StaffInactive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 'active' or 'inactive'"})
		return
	}

	var effectiveTo *time.Time
	if body.EffectiveTo != nil && *body.EffectiveTo != "" {
		parsed, err := time.Parse("2006-01-02", *body.EffectiveTo)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid effective_to format (must be YYYY-MM-DD)"})
			return
		}
		effectiveTo = &parsed
	}

	err = h.AttendanceRepo.UpdateStaffProfileStatus(c.Request.Context(), pid, staffID, status, effectiveTo)
	if errors.Is(err, postgres.ErrStaffNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "staff profile not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update staff status"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "staff status updated"})
}

// ----------------------------------------------------------------------------
// Leave Policy Handlers
// ----------------------------------------------------------------------------

func (h *Handlers) OwnerGetLeavePolicy(c *gin.Context) {
	if h.AttendanceRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attendance service unavailable"})
		return
	}

	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	policy, err := h.AttendanceRepo.GetLeavePolicy(c.Request.Context(), pid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get leave policy"})
		return
	}

	c.JSON(http.StatusOK, policy)
}

func (h *Handlers) OwnerUpdateLeavePolicy(c *gin.Context) {
	if h.AttendanceRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attendance service unavailable"})
		return
	}

	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	var body domain.LeavePolicyUpdateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	if body.MonthlyFreeLeaveDays < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "monthly_free_leave_days cannot be negative"})
		return
	}

	basis := body.WorkingDaysBasis
	if basis == "" {
		basis = domain.WorkingDaysBasisCalendarDays
	}
	if basis != domain.WorkingDaysBasisCalendarDays &&
		basis != domain.WorkingDaysBasisFixed30 &&
		basis != domain.WorkingDaysBasisExcludingSundays {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid working_days_basis"})
		return
	}

	policy := domain.LeavePolicy{
		PropertyID:           pid,
		MonthlyFreeLeaveDays: body.MonthlyFreeLeaveDays,
		PaidHolidays:         body.PaidHolidays,
		WorkingDaysBasis:     basis,
	}

	saved, err := h.AttendanceRepo.UpsertLeavePolicy(c.Request.Context(), policy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update leave policy"})
		return
	}

	c.JSON(http.StatusOK, saved)
}

// ----------------------------------------------------------------------------
// Daily Attendance Handlers
// ----------------------------------------------------------------------------

func (h *Handlers) OwnerMarkDailyAttendance(c *gin.Context) {
	if h.AttendanceSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attendance service unavailable"})
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

	var req domain.MarkDailyAttendanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	if err := h.AttendanceSvc.MarkDailyAttendance(c.Request.Context(), pid, uid, req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "attendance recorded successfully"})
}

func (h *Handlers) OwnerListMonthlyAttendance(c *gin.Context) {
	if h.AttendanceRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attendance service unavailable"})
		return
	}

	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	month := strings.TrimSpace(c.Query("month"))
	if month == "" {
		month = time.Now().UTC().Format("2006-01")
	}

	if _, err := time.Parse("2006-01", month); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid month format (must be YYYY-MM)"})
		return
	}

	records, err := h.AttendanceRepo.ListAttendanceForMonth(c.Request.Context(), pid, month)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list attendance"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"month":   month,
		"records": records,
	})
}

// ----------------------------------------------------------------------------
// Payroll Cycle Handlers (Preview & Finalize)
// ----------------------------------------------------------------------------

type payrollCycleBody struct {
	CycleMonth string `json:"cycle_month" binding:"required"` // "YYYY-MM"
}

func (h *Handlers) OwnerPreviewPayroll(c *gin.Context) {
	if h.AttendanceSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attendance service unavailable"})
		return
	}

	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	var body payrollCycleBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	cycleMonth := strings.TrimSpace(body.CycleMonth)
	if _, err := time.Parse("2006-01", cycleMonth); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cycle_month format (must be YYYY-MM)"})
		return
	}

	previews, err := h.AttendanceSvc.PreviewCyclePayroll(c.Request.Context(), pid, cycleMonth)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to preview payroll: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"cycle_month":  cycleMonth,
		"calculations": previews,
	})
}

func (h *Handlers) OwnerFinalizePayroll(c *gin.Context) {
	if h.AttendanceSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "attendance service unavailable"})
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

	var body payrollCycleBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}

	cycleMonth := strings.TrimSpace(body.CycleMonth)
	if _, err := time.Parse("2006-01", cycleMonth); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cycle_month format (must be YYYY-MM)"})
		return
	}

	finalized, err := h.AttendanceSvc.FinalizePayrollCycleTx(c.Request.Context(), pid, uid, cycleMonth)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize payroll: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"cycle_month": cycleMonth,
		"finalized":   finalized,
	})
}
