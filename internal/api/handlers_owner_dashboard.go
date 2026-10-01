package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// OwnerOccupancy handles GET /owner/occupancy.
// Returns aggregated bed capacity, occupancy counts, vacant beds, and basis points.
func (h *Handlers) OwnerOccupancy(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	occ := h.occupancy(c, pid)
	vacant := occ.CapacityBeds - occ.OccupiedBeds
	if vacant < 0 {
		vacant = 0
	}
	bps := finance.OccupancyBPS(occ)
	pct := float64(bps) / 100.0

	c.JSON(http.StatusOK, gin.H{
		"property_id":        pid,
		"capacity_beds":      occ.CapacityBeds,
		"occupied_beds":      occ.OccupiedBeds,
		"vacant_beds":        vacant,
		"beds_at_risk":       occ.BedsAtRisk,
		"occupancy_rate_bps": bps,
		"occupancy_rate_pct": pct,
		"as_of":              time.Now().UTC(),
	})
}

type BulkMarkPaidPreviewRequest struct {
	DueIDs []uuid.UUID `json:"due_ids" binding:"required"`
}

type EligibleDueItem struct {
	DueID       uuid.UUID `json:"due_id"`
	DueCode     string    `json:"due_code"`
	TenantID    uuid.UUID `json:"tenant_id"`
	TenantName  string    `json:"tenant_name,omitempty"`
	AmountPaise int       `json:"amount_paise"`
	DueDate     string    `json:"due_date"`
}

type SkippedDueItem struct {
	DueID  uuid.UUID `json:"due_id"`
	Reason string    `json:"reason"`
}

// BulkMarkCashPaidPreview handles POST /owner/dues/bulk-mark-paid/preview.
// Computes eligible pending dues and total amount, filtering out already paid or external dues.
func (h *Handlers) BulkMarkCashPaidPreview(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	var req BulkMarkPaidPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due_ids required"})
		return
	}
	if len(req.DueIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due_ids cannot be empty"})
		return
	}

	ctx := c.Request.Context()
	var eligible []EligibleDueItem
	var skipped []SkippedDueItem
	var totalPaise int64

	for _, id := range req.DueIDs {
		due, err := h.DueStore.GetByID(ctx, id)
		if err != nil || due.PropertyID != pid {
			skipped = append(skipped, SkippedDueItem{DueID: id, Reason: "not_found"})
			continue
		}
		if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
			skipped = append(skipped, SkippedDueItem{DueID: id, Reason: string(due.Status)})
			continue
		}

		tenantName := ""
		if h.TenantStore != nil {
			if t, err := h.TenantStore.GetByID(ctx, due.TenantID); err == nil && t != nil {
				tenantName = t.Name
			}
		}

		eligible = append(eligible, EligibleDueItem{
			DueID:       due.ID,
			DueCode:     due.DueCode,
			TenantID:    due.TenantID,
			TenantName:  tenantName,
			AmountPaise: due.Amount,
			DueDate:     due.DueDate.Format("2006-01-02"),
		})
		totalPaise += int64(due.Amount)
	}

	c.JSON(http.StatusOK, gin.H{
		"eligible_count":      len(eligible),
		"total_amount_paise":  totalPaise,
		"total_amount_rupees": float64(totalPaise) / 100.0,
		"eligible_dues":       eligible,
		"skipped_dues":        skipped,
	})
}

type BulkMarkPaidConfirmRequest struct {
	DueIDs              []uuid.UUID `json:"due_ids" binding:"required"`
	ConfirmedTotalPaise int64       `json:"confirmed_total_paise" binding:"required"`
	Note                string      `json:"note"`
}

// BulkMarkCashPaidConfirm handles POST /owner/dues/bulk-mark-paid/confirm.
// Enforces that confirmed_total_paise matches the eligible dues total exactly,
// then applies cash payments atomically under a shared batch_ref.
func (h *Handlers) BulkMarkCashPaidConfirm(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}

	var req BulkMarkPaidConfirmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due_ids and confirmed_total_paise required"})
		return
	}
	if len(req.DueIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due_ids cannot be empty"})
		return
	}

	ctx := c.Request.Context()
	var eligibleDues []*domain.Due
	var computedTotal int64

	for _, id := range req.DueIDs {
		due, err := h.DueStore.GetByID(ctx, id)
		if err != nil || due.PropertyID != pid {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("due %s not found or property mismatch", id)})
			return
		}
		if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("due %s is already settled (%s)", id, due.Status)})
			return
		}
		eligibleDues = append(eligibleDues, due)
		computedTotal += int64(due.Amount)
	}

	// Re-entered total check: strict protection against accidental click or data inflation
	if computedTotal != req.ConfirmedTotalPaise {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("total amount mismatch: computed %d paise, confirmed %d paise", computedTotal, req.ConfirmedTotalPaise),
		})
		return
	}

	batchRef := fmt.Sprintf("bulk_cash_%s_%s", time.Now().UTC().Format("20060102150405"), uuid.New().String()[:8])
	var settledPayments []*domain.Payment

	for _, due := range eligibleDues {
		note := req.Note
		if note == "" {
			note = fmt.Sprintf("Bulk cash collection (batch: %s)", batchRef)
		} else {
			note = fmt.Sprintf("%s (batch: %s)", note, batchRef)
		}

		p, err := h.Payments.MarkCashPaid(ctx, due.ID, due.Amount, uid, note)
		if err != nil {
			respondErr(c, paymentClientErr(err))
			return
		}
		settledPayments = append(settledPayments, p)
	}

	c.JSON(http.StatusOK, gin.H{
		"batch_ref":           batchRef,
		"settled_count":       len(settledPayments),
		"total_settled_paise": computedTotal,
		"payments":            settledPayments,
	})
}

// Generate calendar token scoped to property
func generateCalendarToken(propertyID uuid.UUID, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("calendar_feed:" + propertyID.String()))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s.%s", propertyID.String(), sig[:32])
}

// Verify calendar token scoped to property
func verifyCalendarToken(token, secret string) (uuid.UUID, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return uuid.Nil, false
	}
	propID, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, false
	}
	expected := generateCalendarToken(propID, secret)
	if !hmac.Equal([]byte(token), []byte(expected)) {
		return uuid.Nil, false
	}
	return propID, true
}

// OwnerGetCalendarToken handles GET /owner/calendar/token.
// Returns the signed subscription URL for external calendar readers.
func (h *Handlers) OwnerGetCalendarToken(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	tok := generateCalendarToken(pid, h.JWTSecret)
	baseURL := h.MagicLinkBaseURL
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	calendarURL := fmt.Sprintf("%s/api/owner/calendar.ics?token=%s", strings.TrimRight(baseURL, "/"), tok)

	c.JSON(http.StatusOK, gin.H{
		"property_id":  pid,
		"token":        tok,
		"calendar_url": calendarURL,
	})
}

// OwnerCalendarICS handles GET /owner/calendar.ics.
// Public feed authenticated via property-scoped token query parameter.
// Outputs standard RFC 5545 iCalendar format with anonymized aggregated dues.
func (h *Handlers) OwnerCalendarICS(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		c.String(http.StatusUnauthorized, "missing token")
		return
	}

	propID, ok := verifyCalendarToken(token, h.JWTSecret)
	if !ok {
		c.String(http.StatusUnauthorized, "invalid calendar token")
		return
	}

	ctx := c.Request.Context()
	prop, err := h.PropertyStore.GetByID(ctx, propID)
	if err != nil {
		c.String(http.StatusNotFound, "property not found")
		return
	}

	// Fetch dues for property
	dues, err := h.DueStore.List(ctx, postgres.DueListFilter{PropertyID: propID})
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to load dues")
		return
	}

	// Group pending/partial dues by date (YYYYMMDD)
	type daySummary struct {
		Date        time.Time
		Count       int
		AmountPaise int64
	}
	summaryByDate := make(map[string]*daySummary)

	for _, d := range dues {
		if d.Status != domain.DueStatusPending && d.Status != domain.DueStatusPartial {
			continue
		}
		dateKey := d.DueDate.Format("20060102")
		if summaryByDate[dateKey] == nil {
			summaryByDate[dateKey] = &daySummary{
				Date: d.DueDate,
			}
		}
		summaryByDate[dateKey].Count++
		summaryByDate[dateKey].AmountPaise += int64(d.Amount)
	}

	// Sort dates for deterministic output
	var dateKeys []string
	for k := range summaryByDate {
		dateKeys = append(dateKeys, k)
	}
	sort.Strings(dateKeys)

	// Build RFC 5545 iCalendar stream
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//PG Cashflow//Rent Calendar//EN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
	b.WriteString("METHOD:PUBLISH\r\n")
	b.WriteString(fmt.Sprintf("X-WR-CALNAME:Rent Dues - %s\r\n", prop.Name))
	b.WriteString("X-WR-TIMEZONE:Asia/Kolkata\r\n")

	nowStamp := time.Now().UTC().Format("20060102T150405Z")

	for _, k := range dateKeys {
		s := summaryByDate[k]
		rupees := float64(s.AmountPaise) / 100.0

		b.WriteString("BEGIN:VEVENT\r\n")
		b.WriteString(fmt.Sprintf("UID:rent-%s-%s@pg-cashflow\r\n", propID.String()[:8], k))
		b.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", nowStamp))
		b.WriteString(fmt.Sprintf("DTSTART;VALUE=DATE:%s\r\n", k))
		b.WriteString(fmt.Sprintf("SUMMARY:Rent Due: %d dues (₹%.0f)\r\n", s.Count, rupees))
		b.WriteString(fmt.Sprintf("DESCRIPTION:Aggregated rent dues for %s: %d pending dues totaling ₹%.2f.\r\n", prop.Name, s.Count, rupees))
		b.WriteString("STATUS:CONFIRMED\r\n")
		b.WriteString("TRANSP:TRANSPARENT\r\n")
		b.WriteString("END:VEVENT\r\n")
	}

	b.WriteString("END:VCALENDAR\r\n")

	c.Header("Content-Type", "text/calendar; charset=utf-8")
	c.Header("Content-Disposition", "inline; filename=\"rent-dues.ics\"")
	c.String(http.StatusOK, b.String())
}

// OwnerGetPropertySettings handles GET /owner/settings.
func (h *Handlers) OwnerGetPropertySettings(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	settings, err := h.getPropertySettings(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": settings})
}

type UpdatePropertySettingsInput struct {
	PayoutAutoDispatch  *bool           `json:"payout_auto_dispatch"`
	ReminderOffsets     []int           `json:"reminder_offsets"`
	ReminderCatchUpDays *int            `json:"reminder_catch_up_days"`
	ActiveModules       map[string]bool `json:"active_modules"`
	AutoApplyCredit     *bool           `json:"auto_apply_credit"`
}

// OwnerUpdatePropertySettings handles PATCH /owner/settings.
func (h *Handlers) OwnerUpdatePropertySettings(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	var in UpdatePropertySettingsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid request body"))
		return
	}

	current, err := h.getPropertySettings(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}

	if in.PayoutAutoDispatch != nil {
		current.PayoutAutoDispatch = *in.PayoutAutoDispatch
	}
	if in.ReminderOffsets != nil {
		current.ReminderOffsets = in.ReminderOffsets
	}
	if in.ReminderCatchUpDays != nil {
		current.ReminderCatchUpDays = *in.ReminderCatchUpDays
	}
	if in.ActiveModules != nil {
		current.ActiveModules = in.ActiveModules
	}
	if in.AutoApplyCredit != nil {
		current.AutoApplyCredit = *in.AutoApplyCredit
	}

	if pss, ok := h.PropertyStore.(interface {
		UpsertSettings(context.Context, *domain.PropertySettings) error
	}); ok {
		if err := pss.UpsertSettings(c.Request.Context(), current); err != nil {
			respondErr(c, err)
			return
		}
	} else if h.Pool != nil {
		repo := postgres.NewPropertyRepo(h.Pool)
		if err := repo.UpsertSettings(c.Request.Context(), current); err != nil {
			respondErr(c, err)
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"settings": current})
}

