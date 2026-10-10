package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/gamification"
)

// --- Tenant Handlers ---

// TenantPoints returns live balance, expiring points, streak, and recent ledger entries.
func (h *Handlers) TenantPoints(c *gin.Context) {
	tenant, ok := c.Get(auth.ContextTenantKey)
	if !ok {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return
	}
	t := tenant.(*domain.Tenant)

	balance, expiringSoon, earliest, err := h.Gamification.GetBalance(c.Request.Context(), t.ID)
	if err != nil {
		respondErr(c, err)
		return
	}

	streak, _ := h.GamificationStore.GetStreak(c.Request.Context(), t.ID)
	ledger, _ := h.GamificationStore.ListLedgerByTenant(c.Request.Context(), t.ID, 20)

	c.JSON(http.StatusOK, gin.H{
		"balance":         balance,
		"expiring_soon":   expiringSoon,
		"earliest_expiry": earliest,
		"on_time_months":  streak.OnTimeMonths,
		"freezes_left":    streak.FreezesAvailable,
		"ledger":          ledger,
	})
}

// TenantRewards lists catalog perks and shows if tenant meets tenure requirement.
func (h *Handlers) TenantRewards(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	catalog, err := h.GamificationStore.ListRewardsCatalog(c.Request.Context(), tenant.PropertyID)
	if err != nil {
		respondErr(c, err)
		return
	}

	streak, _ := h.GamificationStore.GetStreak(c.Request.Context(), tenant.ID)
	step3Count, _ := h.GamificationStore.CountStep3ViolationsInQuarter(c.Request.Context(), tenant.ID)

	type rewardView struct {
		domain.RewardsCatalogItem
		Eligible bool   `json:"eligible"`
		Reason   string `json:"reason,omitempty"`
	}

	views := make([]rewardView, len(catalog))
	for i, r := range catalog {
		v := rewardView{RewardsCatalogItem: r, Eligible: true}
		if r.Category == "cash_credit" {
			if step3Count > 0 {
				v.Eligible = false
				v.Reason = "Blocked this quarter due to step-3 violation"
			} else if streak.OnTimeMonths < r.MinTenureMonths {
				v.Eligible = false
				v.Reason = "Requires 3 consecutive on-time rent payments"
			}
		}
		views[i] = v
	}

	c.JSON(http.StatusOK, gin.H{"rewards": views})
}

// TenantRedeem executes redemption with row-lock concurrency protection.
func (h *Handlers) TenantRedeem(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	rewardID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.GamificationStore != nil {
		reward, err := h.GamificationStore.GetRewardByID(c.Request.Context(), rewardID)
		if err != nil || reward == nil || reward.PropertyID != tenant.PropertyID {
			c.JSON(http.StatusNotFound, gin.H{"error": "reward not found"})
			return
		}
	}

	red, err := h.Gamification.RedeemReward(c.Request.Context(), tenant.ID, rewardID)
	if err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"redemption": red})
}

// TenantInspections returns room and floor inspection reports.
func (h *Handlers) TenantInspections(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	inspections, err := h.GamificationStore.ListInspections(c.Request.Context(), tenant.PropertyID, tenant.RoomID, nil, 20)
	if err != nil {
		respondErr(c, err)
		return
	}

	// Fetch items for each inspection
	full := make([]domain.Inspection, len(inspections))
	for i, insp := range inspections {
		detailed, err := h.GamificationStore.GetInspectionByID(c.Request.Context(), insp.ID)
		if err == nil && detailed != nil {
			full[i] = *detailed
		} else {
			full[i] = insp
		}
	}

	c.JSON(http.StatusOK, gin.H{"inspections": full})
}

// TenantDisputeInspectionItem files a dispute within 48 hours.
func (h *Handlers) TenantDisputeInspectionItem(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	itemID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body struct {
		DisputeNote string `json:"dispute_note"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.DisputeNote == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dispute_note is required"})
		return
	}

	if err := h.Gamification.DisputeInspectionItem(c.Request.Context(), itemID, tenant.ID, body.DisputeNote); err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	if h.OutboxEvents != nil {
		payload, _ := json.Marshal(map[string]string{
			"item_id": itemID.String(),
		})
		tid := tenant.ID
		_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
			EventType:  string(domain.EvtInspectionDisputed),
			PropertyID: tenant.PropertyID,
			TenantID:   &tid,
			ActorRole:  string(domain.RoleTenant),
			Payload:    payload,
		})
	}

	c.JSON(http.StatusOK, gin.H{"status": "disputed"})
}

// TenantMealRSVP returns tomorrow's RSVP and allows confirmation.
func (h *Handlers) TenantGetMealRSVP(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)

	b, _ := h.GamificationStore.GetTenantMealRSVP(c.Request.Context(), tenant.ID, tomorrow, "breakfast")
	l, _ := h.GamificationStore.GetTenantMealRSVP(c.Request.Context(), tenant.ID, tomorrow, "lunch")
	d, _ := h.GamificationStore.GetTenantMealRSVP(c.Request.Context(), tenant.ID, tomorrow, "dinner")

	c.JSON(http.StatusOK, gin.H{
		"date":      tomorrow.Format("2006-01-02"),
		"breakfast": b,
		"lunch":     l,
		"dinner":    d,
	})
}

func (h *Handlers) TenantSubmitMealRSVP(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	var body struct {
		Date      string `json:"date"` // YYYY-MM-DD
		Slot      string `json:"slot"` // breakfast, lunch, dinner
		Attending bool   `json:"attending"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	mealDate, err := time.Parse("2006-01-02", body.Date)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "date must be YYYY-MM-DD"})
		return
	}

	rsvp, err := h.Gamification.SubmitMealRSVP(c.Request.Context(), tenant.ID, mealDate, body.Slot, body.Attending)
	if err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"rsvp": rsvp})
}

// TenantMenuPoll gets active poll and casts vote.
func (h *Handlers) TenantGetMenuPoll(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	monthYear := time.Now().UTC().Format("2006-01")

	poll, err := h.GamificationStore.GetActiveMenuPoll(c.Request.Context(), tenant.PropertyID, monthYear)
	if err != nil {
		respondErr(c, err)
		return
	}
	if poll == nil {
		c.JSON(http.StatusOK, gin.H{"poll": nil})
		return
	}

	votes, _ := h.GamificationStore.GetMenuPollVotes(c.Request.Context(), poll.ID)
	c.JSON(http.StatusOK, gin.H{"poll": poll, "votes": votes})
}

func (h *Handlers) TenantVoteMenuPoll(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	var body struct {
		PollID   string `json:"poll_id"`
		OptionID string `json:"option_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	pid, err := uuid.Parse(body.PollID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid poll_id"})
		return
	}

	if h.GamificationStore != nil {
		monthYear := time.Now().UTC().Format("2006-01")
		activePoll, err := h.GamificationStore.GetActiveMenuPoll(c.Request.Context(), tenant.PropertyID, monthYear)
		if err != nil || activePoll == nil || activePoll.ID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "poll not found"})
			return
		}
	}

	vote := &domain.MenuVote{
		PollID:   pid,
		TenantID: tenant.ID,
		OptionID: body.OptionID,
	}
	if err := h.GamificationStore.VoteMenuPoll(c.Request.Context(), vote); err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "voted"})
}

// TenantReportHazard anonymously submits a safety hazard or water leak.
func (h *Handlers) TenantReportHazard(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)

	category := c.PostForm("category")
	description := c.PostForm("description")
	if category == "" || description == "" {
		// Try JSON fallback
		var jsonBody struct {
			Category    string `json:"category"`
			Description string `json:"description"`
			PhotoBase64 string `json:"photo_base64"`
		}
		if err := c.ShouldBindJSON(&jsonBody); err == nil && jsonBody.Category != "" {
			category = jsonBody.Category
			description = jsonBody.Description
			var photoBytes []byte
			if jsonBody.PhotoBase64 != "" {
				photoBytes, _ = base64.StdEncoding.DecodeString(jsonBody.PhotoBase64)
			}
			hz, err := h.Gamification.ReportHazard(c.Request.Context(), tenant.ID, category, description, photoBytes)
			if err != nil {
				respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
				return
			}
			if h.OutboxEvents != nil && hz != nil {
				payload, _ := json.Marshal(map[string]string{"hazard_id": hz.ID.String()})
				tid := tenant.ID
				_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
					EventType:  string(domain.EvtHazardReported),
					PropertyID: hz.PropertyID,
					TenantID:   &tid,
					ActorRole:  string(domain.RoleTenant),
					Payload:    payload,
				})
			}
			c.JSON(http.StatusOK, gin.H{"hazard": hz})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "category and description are required"})
		return
	}

	var photoBytes []byte
	file, _, err := c.Request.FormFile("photo")
	if err == nil && file != nil {
		defer file.Close()
		photoBytes, _ = io.ReadAll(file)
	}

	hz, err := h.Gamification.ReportHazard(c.Request.Context(), tenant.ID, category, description, photoBytes)
	if err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	if h.OutboxEvents != nil && hz != nil {
		payload, _ := json.Marshal(map[string]string{"hazard_id": hz.ID.String()})
		tid := tenant.ID
		_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
			EventType:  string(domain.EvtHazardReported),
			PropertyID: hz.PropertyID,
			TenantID:   &tid,
			ActorRole:  string(domain.RoleTenant),
			Payload:    payload,
		})
	}

	c.JSON(http.StatusOK, gin.H{"hazard": hz})
}

// TenantViolations lists private notices and status on the ladder.
func (h *Handlers) TenantViolations(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	violations, err := h.GamificationStore.ListViolationsByTenant(c.Request.Context(), tenant.ID)
	if err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"violations": violations})
}

// TenantLeaderboard returns top positive streaks.
func (h *Handlers) TenantLeaderboard(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	streaks, err := h.GamificationStore.GetTopStreaks(c.Request.Context(), tenant.PropertyID, 10)
	if err != nil {
		respondErr(c, err)
		return
	}

	monthYear := time.Now().UTC().Format("2006-01")
	floorScores, _ := h.GamificationStore.GetFloorCleanScores(c.Request.Context(), tenant.PropertyID, monthYear)

	c.JSON(http.StatusOK, gin.H{
		"streaks":      streaks,
		"floor_scores": floorScores,
	})
}

// TenantReferrals handles tenant referral creation and tracking.
func (h *Handlers) TenantReferrals(c *gin.Context) {
	tenant := c.MustGet(auth.ContextTenantKey).(*domain.Tenant)
	if c.Request.Method == http.MethodPost {
		var body struct {
			Phone string `json:"phone"`
			Name  string `json:"name"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.Phone == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "phone is required"})
			return
		}
		ref := &domain.Referral{
			PropertyID:       tenant.PropertyID,
			ReferrerTenantID: tenant.ID,
			Phone:            body.Phone,
			Name:             body.Name,
		}
		if err := h.GamificationStore.CreateReferral(c.Request.Context(), ref); err != nil {
			respondErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"referral": ref})
		return
	}

	list, err := h.GamificationStore.ListReferralsByTenant(c.Request.Context(), tenant.ID)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"referrals": list})
}

// --- Manager / Warden Handlers ---

// ManagerSubmitInspection conducts a room or floor inspection.
func (h *Handlers) ManagerSubmitInspection(c *gin.Context) {
	claims, _ := auth.ClaimsFromContext(c)
	var body struct {
		PropertyID     string                  `json:"property_id"`
		RoomID         *string                 `json:"room_id"`
		FloorID        *string                 `json:"floor_id"`
		InspectionType string                  `json:"inspection_type"` // room, floor
		Notes          string                  `json:"notes"`
		Items          []inspectionItemPayload `json:"items"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid payload"))
		return
	}

	propID, err := uuid.Parse(body.PropertyID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid property_id"})
		return
	}

	// Scoped property check
	if claims.PropertyID == nil || *claims.PropertyID != propID {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
		return
	}

	var roomID *uuid.UUID
	if body.RoomID != nil && *body.RoomID != "" {
		rid, err := uuid.Parse(*body.RoomID)
		if err == nil {
			roomID = &rid
		}
	}
	var floorID *uuid.UUID
	if body.FloorID != nil && *body.FloorID != "" {
		fid, err := uuid.Parse(*body.FloorID)
		if err == nil {
			floorID = &fid
		}
	}

	items := make([]domain.InspectionItem, len(body.Items))
	for i, it := range body.Items {
		var photoBytes []byte
		if it.PhotoBase64 != "" {
			photoBytes, _ = base64.StdEncoding.DecodeString(it.PhotoBase64)
		}
		items[i] = domain.InspectionItem{
			ItemKey:     it.ItemKey,
			Description: it.Description,
			Passed:      it.Passed,
			PhotoBytes:  photoBytes,
			Notes:       it.Notes,
		}
	}

	insp := &domain.Inspection{
		PropertyID:      propID,
		RoomID:          roomID,
		FloorID:         floorID,
		InspectorUserID: claims.UserID,
		InspectionType:  body.InspectionType,
		Notes:           body.Notes,
		Items:           items,
	}

	res, err := h.Gamification.SubmitInspection(c.Request.Context(), insp)
	if err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"inspection": res})
}

type inspectionItemPayload struct {
	ItemKey     string `json:"item_key"`
	Description string `json:"description"`
	Passed      bool   `json:"passed"`
	PhotoBase64 string `json:"photo_base64"`
	Notes       string `json:"notes"`
}

// ManagerListInspections lists inspections.
func (h *Handlers) ManagerListInspections(c *gin.Context) {
	claims, _ := auth.ClaimsFromContext(c)
	if claims.PropertyID == nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "no property scope"})
		return
	}
	propID := *claims.PropertyID
	if q := c.Query("property_id"); q != "" {
		if reqPID, err := uuid.Parse(q); err != nil || reqPID != propID {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
			return
		}
	}

	list, err := h.GamificationStore.ListInspections(c.Request.Context(), propID, nil, nil, 30)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"inspections": list})
}

// ManagerResolveInspectionItem upholds or overturns a tenant dispute.
func (h *Handlers) ManagerResolveInspectionItem(c *gin.Context) {
	claims, _ := auth.ClaimsFromContext(c)
	if claims.PropertyID == nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "no property scope"})
		return
	}
	itemID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body struct {
		Status string `json:"status"` // upheld, overturned
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be upheld or overturned"})
		return
	}

	if err := h.Gamification.ResolveInspectionItem(c.Request.Context(), *claims.PropertyID, itemID, claims.UserID, body.Status); err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "resolved", "resolution": body.Status})
}

// ManagerLogViolation records a rule violation.
func (h *Handlers) ManagerLogViolation(c *gin.Context) {
	claims, _ := auth.ClaimsFromContext(c)
	if claims.PropertyID == nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "no property scope"})
		return
	}
	var body struct {
		TenantID       string `json:"tenant_id"`
		RuleCode       string `json:"rule_code"`
		Severity       string `json:"severity"` // safety, lifestyle
		Description    string `json:"description"`
		EvidenceBase64 string `json:"evidence_base64"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid payload"))
		return
	}

	tid, err := uuid.Parse(body.TenantID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant_id"})
		return
	}

	if h.TenantStore != nil {
		tenant, err := h.TenantStore.GetByID(c.Request.Context(), tid)
		if err != nil || tenant == nil || tenant.PropertyID != *claims.PropertyID {
			c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
			return
		}
	}

	var evidence []byte
	if body.EvidenceBase64 != "" {
		evidence, _ = base64.StdEncoding.DecodeString(body.EvidenceBase64)
	}

	v, err := h.Gamification.LogViolation(c.Request.Context(), tid, body.RuleCode, body.Severity, body.Description, evidence, claims.UserID)
	if err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"violation": v})
}

// ManagerRecordMeterReading submits a meter reading with floor & ceiling checks.
func (h *Handlers) ManagerRecordMeterReading(c *gin.Context) {
	claims, _ := auth.ClaimsFromContext(c)
	var body struct {
		PropertyID     string  `json:"property_id"`
		RoomID         *string `json:"room_id"`
		FloorID        *string `json:"floor_id"`
		Kind           string  `json:"kind"` // electricity, water
		ReadingValue   float64 `json:"reading_value"`
		MeterReplaced  bool    `json:"meter_replaced"`
		ConfirmAnomaly bool    `json:"confirm_anomaly"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid payload"))
		return
	}

	propID, err := uuid.Parse(body.PropertyID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid property_id"})
		return
	}

	if claims.PropertyID == nil || *claims.PropertyID != propID {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
		return
	}

	var roomID *uuid.UUID
	if body.RoomID != nil && *body.RoomID != "" {
		rid, err := uuid.Parse(*body.RoomID)
		if err == nil {
			roomID = &rid
		}
	}
	var floorID *uuid.UUID
	if body.FloorID != nil && *body.FloorID != "" {
		fid, err := uuid.Parse(*body.FloorID)
		if err == nil {
			floorID = &fid
		}
	}

	input := gamification.MeterReadingInput{
		PropertyID:     propID,
		RoomID:         roomID,
		FloorID:        floorID,
		Kind:           body.Kind,
		ReadingValue:   body.ReadingValue,
		MeterReplaced:  body.MeterReplaced,
		ConfirmAnomaly: body.ConfirmAnomaly,
		RecordedBy:     claims.UserID,
	}

	res, err := h.Gamification.RecordMeterReading(c.Request.Context(), input)
	if err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"result": res})
}

// ManagerKitchenHeadcount returns live headcount report for kitchen staff.
func (h *Handlers) ManagerKitchenHeadcount(c *gin.Context) {
	claims, _ := auth.ClaimsFromContext(c)
	if claims.PropertyID == nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "no property scope"})
		return
	}
	propID := *claims.PropertyID
	if q := c.Query("property_id"); q != "" {
		if reqPID, err := uuid.Parse(q); err != nil || reqPID != propID {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
			return
		}
	}

	dateStr := c.Query("date")
	targetDate := time.Now().UTC()
	if dateStr != "" {
		if d, err := time.Parse("2006-01-02", dateStr); err == nil {
			targetDate = d
		}
	}

	rep, err := h.Gamification.GetHeadcountReport(c.Request.Context(), propID, targetDate)
	if err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"headcount": rep})
}

// ManagerHazards lists and resolves maintenance hazards.
func (h *Handlers) ManagerListHazards(c *gin.Context) {
	claims, _ := auth.ClaimsFromContext(c)
	if claims.PropertyID == nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "no property scope"})
		return
	}
	propID := *claims.PropertyID
	if q := c.Query("property_id"); q != "" {
		if reqPID, err := uuid.Parse(q); err != nil || reqPID != propID {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
			return
		}
	}

	status := c.Query("status")
	list, err := h.GamificationStore.ListHazards(c.Request.Context(), propID, status)
	if err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"hazards": list})
}

func (h *Handlers) ManagerResolveHazard(c *gin.Context) {
	claims, _ := auth.ClaimsFromContext(c)
	if claims.PropertyID == nil {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "no property scope"})
		return
	}
	hazardID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.GamificationStore != nil {
		hz, err := h.GamificationStore.GetHazardByID(c.Request.Context(), hazardID)
		if err != nil || hz == nil || hz.PropertyID != *claims.PropertyID {
			c.JSON(http.StatusNotFound, gin.H{"error": "hazard not found"})
			return
		}
	}

	var body struct {
		Status string `json:"status"` // resolved, rejected
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Status == "" {
		body.Status = "resolved"
	}

	if err := h.Gamification.ResolveHazard(c.Request.Context(), hazardID, claims.UserID, body.Status); err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": body.Status})
}

// ManagerVendorInspection audits the mess vendor.
func (h *Handlers) ManagerSubmitVendorInspection(c *gin.Context) {
	claims, _ := auth.ClaimsFromContext(c)
	var body struct {
		PropertyID   string `json:"property_id"`
		VendorName   string `json:"vendor_name"`
		ScorePercent int    `json:"score_percent"`
		Notes        string `json:"notes"`
		PenaltyPaise int64  `json:"penalty_paise"`
		PhotoBase64  string `json:"photo_base64"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	propID, err := uuid.Parse(body.PropertyID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid property_id"})
		return
	}

	if claims.PropertyID == nil || *claims.PropertyID != propID {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
		return
	}

	var photoBytes []byte
	if body.PhotoBase64 != "" {
		photoBytes, _ = base64.StdEncoding.DecodeString(body.PhotoBase64)
	}

	vi := &domain.VendorInspection{
		PropertyID:      propID,
		InspectorUserID: claims.UserID,
		VendorName:      body.VendorName,
		ScorePercent:    body.ScorePercent,
		Notes:           body.Notes,
		PenaltyPaise:    body.PenaltyPaise,
		PhotoBytes:      photoBytes,
	}

	res, err := h.Gamification.SubmitVendorInspection(c.Request.Context(), vi)
	if err != nil {
		respondErr(c, gamificationClientErr(http.StatusBadRequest, err))
		return
	}

	c.JSON(http.StatusOK, gin.H{"vendor_inspection": res})
}

// --- Owner Handlers ---

// OwnerGamificationSettings gets gamification rules & budget settings.
func (h *Handlers) OwnerGetGamificationSettings(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if q := c.Query("property_id"); q != "" {
		if reqPID, err := uuid.Parse(q); err != nil || reqPID != pid {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
			return
		}
	}

	settings, err := h.GamificationStore.GetSettings(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}

	rules, err := h.GamificationStore.ListPointRules(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"settings": settings, "rules": rules})
}

// OwnerUpdateGamificationSettings updates caps, point values, or floor multipliers.
func (h *Handlers) OwnerUpdateGamificationSettings(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var body domain.PropertyGamificationSettings
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}
	body.PropertyID = pid

	if err := h.GamificationStore.UpdateSettings(c.Request.Context(), &body); err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"settings": body})
}

// OwnerFloorsAndRooms lists floors and rooms for a property.
func (h *Handlers) OwnerListFloors(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if q := c.Query("property_id"); q != "" {
		if reqPID, err := uuid.Parse(q); err != nil || reqPID != pid {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
			return
		}
	}

	floors, err := h.GamificationStore.ListFloors(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"floors": floors})
}

func (h *Handlers) OwnerCreateFloor(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var body struct {
		FloorNumber int    `json:"floor_number"`
		Name        string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}

	f := &domain.Floor{
		PropertyID:  pid,
		FloorNumber: body.FloorNumber,
		Name:        body.Name,
	}
	if err := h.GamificationStore.CreateFloor(c.Request.Context(), f); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"floor": f})
}

func (h *Handlers) OwnerListRooms(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if q := c.Query("property_id"); q != "" {
		if reqPID, err := uuid.Parse(q); err != nil || reqPID != pid {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
			return
		}
	}

	rooms, err := h.GamificationStore.ListRooms(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"rooms": rooms})
}

func (h *Handlers) OwnerCreateRoom(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var body struct {
		FloorID       string `json:"floor_id"`
		RoomNumber    string `json:"room_number"`
		Capacity      int16  `json:"capacity"`
		IncludedUnits int    `json:"included_units"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}
	floorID, err := uuid.Parse(body.FloorID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid floor_id"})
		return
	}

	// Verify floor belongs to this property
	floors, err := h.GamificationStore.ListFloors(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}
	floorFound := false
	for _, f := range floors {
		if f.ID == floorID {
			floorFound = true
			break
		}
	}
	if !floorFound {
		c.JSON(http.StatusBadRequest, gin.H{"error": "floor does not belong to this property"})
		return
	}

	rm := &domain.Room{
		PropertyID:    pid,
		FloorId:       floorID,
		RoomNumber:    body.RoomNumber,
		Capacity:      body.Capacity,
		IncludedUnits: body.IncludedUnits,
	}
	if err := h.GamificationStore.CreateRoom(c.Request.Context(), rm); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"room": rm})
}

// OwnerCreateManager provisions a warden/manager user account scoped to a property.
func (h *Handlers) OwnerCreateManager(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var body struct {
		PropertyID string `json:"property_id"`
		Phone      string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone is required"})
		return
	}

	if body.PropertyID != "" {
		if reqPID, err := uuid.Parse(body.PropertyID); err != nil || reqPID != pid {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
			return
		}
	}

	u := &domain.User{
		Phone:      body.Phone,
		Role:       domain.RoleManager,
		PropertyID: &pid,
	}
	if err := h.UserStore.Create(c.Request.Context(), u); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"manager": u})
}

// OwnerListRedemptions lists coupon and perk redemptions for a property.
func (h *Handlers) OwnerListRedemptions(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if q := c.Query("property_id"); q != "" {
		if reqPID, err := uuid.Parse(q); err != nil || reqPID != pid {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
			return
		}
	}
	status := c.Query("status")
	redemptions, err := h.Gamification.ListRedemptionsByProperty(c.Request.Context(), pid, status)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"redemptions": redemptions})
}

// OwnerFulfilRedemption marks a pending coupon or perk redemption as fulfilled.
func (h *Handlers) OwnerFulfilRedemption(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	redemptionID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	red, err := h.Gamification.FulfilRedemption(c.Request.Context(), pid, redemptionID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"redemption": red})
}
