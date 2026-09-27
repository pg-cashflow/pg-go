package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// ---------------------------------------------------------------------
// Tenant Departures (ADR-009)
// ---------------------------------------------------------------------

type createDepartureBody struct {
	PlannedVacateDate  string  `json:"planned_vacate_date" binding:"required"` // "YYYY-MM-DD"
	DepositAmountPaise *int64  `json:"deposit_amount_paise"`
	Notes              *string `json:"notes"`
}

// OwnerCreateDeparture handles POST /owner/tenants/:id/departures.
func (h *Handlers) OwnerCreateDeparture(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tenant id"})
		return
	}

	var body createDepartureBody
	if err := c.ShouldBindJSON(&body); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid request body"))
		return
	}

	plannedDate, err := time.Parse("2006-01-02", strings.TrimSpace(body.PlannedVacateDate))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid planned_vacate_date format (must be YYYY-MM-DD)"})
		return
	}

	// Verify tenant exists and belongs to this property
	t, err := h.TenantStore.GetByID(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tenant not found"})
		return
	}
	if t.PropertyID != pid {
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant does not belong to owner property"})
		return
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	// Check if active departure exists
	existing, err := repo.GetActiveDepartureByTenant(c.Request.Context(), tenantID)
	if err == nil && existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "active departure already exists for tenant", "departure_id": existing.ID})
		return
	}

	depositPaise := int64(t.RentAmount) // default to 1 month rent if unspecified
	if body.DepositAmountPaise != nil && *body.DepositAmountPaise >= 0 {
		depositPaise = *body.DepositAmountPaise
	}

	now := time.Now().UTC()
	dep := &domain.TenantDeparture{
		TenantID:           tenantID,
		PropertyID:         pid,
		NoticeGivenAt:      now,
		PlannedVacateDate:  plannedDate,
		DepositAmountPaise: depositPaise,
		Status:             domain.DeparturePending,
		Notes:              body.Notes,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	if err := repo.CreateDeparture(c.Request.Context(), dep); err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"departure": dep})
}

type inspectDepartureBody struct {
	InspectedAt *time.Time `json:"inspected_at"`
	Notes       *string    `json:"notes"`
}

// OwnerInspectDeparture handles POST /owner/departures/:id/inspect.
func (h *Handlers) OwnerInspectDeparture(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	depID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid departure id"})
		return
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	dep, err := repo.GetDepartureByID(c.Request.Context(), depID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "departure not found"})
		return
	}
	if dep.PropertyID != pid {
		c.JSON(http.StatusForbidden, gin.H{"error": "departure does not belong to owner property"})
		return
	}

	var body inspectDepartureBody
	_ = c.ShouldBindJSON(&body)

	inspectedAt := time.Now().UTC()
	if body.InspectedAt != nil && !body.InspectedAt.IsZero() {
		inspectedAt = body.InspectedAt.UTC()
	}

	if err := repo.UpdateDepartureInspection(c.Request.Context(), depID, inspectedAt, body.Notes); err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "inspected", "inspected_at": inspectedAt, "sla_deadline_at": inspectedAt.Add(24 * time.Hour)})
}

type addDeductionBody struct {
	Description      string  `json:"description" binding:"required"`
	AmountPaise      int64   `json:"amount_paise" binding:"required"`
	EvidencePhotoKey *string `json:"evidence_photo_key"`
	Status           string  `json:"status"` // agreed, disputed, waived
}

// OwnerAddDepartureDeduction handles POST /owner/departures/:id/deductions.
func (h *Handlers) OwnerAddDepartureDeduction(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	depID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid departure id"})
		return
	}

	var body addDeductionBody
	if err := c.ShouldBindJSON(&body); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid request body"))
		return
	}
	if body.AmountPaise <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount_paise must be positive"})
		return
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	dep, err := repo.GetDepartureByID(c.Request.Context(), depID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "departure not found"})
		return
	}
	if dep.PropertyID != pid {
		c.JSON(http.StatusForbidden, gin.H{"error": "departure does not belong to owner property"})
		return
	}

	status := domain.DeductionAgreed
	if body.Status != "" {
		status = domain.DeductionStatus(body.Status)
	}

	ded := &domain.DepartureDeduction{
		DepartureID:      depID,
		Description:      strings.TrimSpace(body.Description),
		AmountPaise:      body.AmountPaise,
		EvidencePhotoKey: body.EvidencePhotoKey,
		Status:           status,
	}

	if err := repo.AddDeduction(c.Request.Context(), ded); err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"deduction": ded})
}

type settleDepartureBody struct {
	ActualVacateDate  string     `json:"actual_vacate_date" binding:"required"` // "YYYY-MM-DD"
	ProratedRentPaise int64      `json:"prorated_rent_paise"`
	PayeeID           *uuid.UUID `json:"payee_id"`
	Notes             *string    `json:"notes"`
}

// OwnerSettleDeparture handles POST /owner/departures/:id/settle.
func (h *Handlers) OwnerSettleDeparture(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	depID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid departure id"})
		return
	}

	var body settleDepartureBody
	if err := c.ShouldBindJSON(&body); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid request body"))
		return
	}

	vacateDate, err := time.Parse("2006-01-02", strings.TrimSpace(body.ActualVacateDate))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid actual_vacate_date format (must be YYYY-MM-DD)"})
		return
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	dep, err := repo.GetDepartureByID(c.Request.Context(), depID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "departure not found"})
		return
	}
	if dep.PropertyID != pid {
		c.JSON(http.StatusForbidden, gin.H{"error": "departure does not belong to owner property"})
		return
	}

	res, err := repo.SettleDepartureUnderLock(c.Request.Context(), postgres.SettleDepartureParams{
		DepartureID:       depID,
		ActualVacateDate:  vacateDate,
		ProratedRentPaise: body.ProratedRentPaise,
		PayeeID:           body.PayeeID,
		Notes:             body.Notes,
	})
	if err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"departure":                     res.Departure,
		"payout_item":                  res.PayoutItem,
		"due_adjustments":              res.DueAdjustments,
		"internal_payment_id":          res.InternalPaymentID,
		"outstanding_dues_netted_paise": res.OutstandingDuesNettedPaise,
		"unused_rent_refund_paise":      res.UnusedRentRefundPaise,
		"prorated_rent_owed_paise":      res.ProratedRentOwedPaise,
		"deductions_paise":             res.DeductionsPaise,
		"net_refund_paise":             res.NetRefundPaise,
		"receivable_balance_paise":     res.ReceivableBalancePaise,
	})
}

// ---------------------------------------------------------------------
// Operational Payouts Subsystem (ADR-009)
// ---------------------------------------------------------------------

type createPayeeBody struct {
	PayeeType  string  `json:"payee_type" binding:"required"` // staff, vendor, tenant_deposit, guardian_deposit
	Name       string  `json:"name" binding:"required"`
	Phone      *string `json:"phone"`
	AccountNum *string `json:"account_number"`
	IFSC       *string `json:"ifsc"`
	BankName   *string `json:"bank_name"`
	UPIVPA     *string `json:"upi_vpa"`
}

// OwnerCreatePayee handles POST /owner/payouts/payees.
func (h *Handlers) OwnerCreatePayee(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	var body createPayeeBody
	if err := c.ShouldBindJSON(&body); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid request body"))
		return
	}

	hasAcct := body.AccountNum != nil && strings.TrimSpace(*body.AccountNum) != ""
	hasUPI := body.UPIVPA != nil && strings.TrimSpace(*body.UPIVPA) != ""
	if !hasAcct && !hasUPI {
		c.JSON(http.StatusBadRequest, gin.H{"error": "either account_number or upi_vpa is required"})
		return
	}

	var acctIdentifier string
	var last4 *string
	var encryptedAcct []byte
	if hasAcct {
		raw := strings.TrimSpace(*body.AccountNum)
		acctIdentifier = raw
		if len(raw) >= 4 {
			l4 := raw[len(raw)-4:]
			last4 = &l4
		} else {
			last4 = &raw
		}
		// Envelope encryption placeholder: store bytes
		encryptedAcct = []byte(raw)
	} else {
		acctIdentifier = strings.TrimSpace(*body.UPIVPA)
	}

	salt := []byte(h.getChecksumSecret())
	acctHash := domain.ComputeAccountHash(salt, acctIdentifier)

	payee := &domain.PayoutPayee{
		PropertyID:             pid,
		PayeeType:              domain.PayeeType(body.PayeeType),
		Name:                   strings.TrimSpace(body.Name),
		Phone:                  body.Phone,
		AccountNumberEncrypted: encryptedAcct,
		AccountNumberLast4:     last4,
		AccountNumberHash:      acctHash,
		IFSC:                   body.IFSC,
		BankName:               body.BankName,
		UPIVPA:                 body.UPIVPA,
		KeyVersion:             1,
		IsVerified:             true, // Owner manual creation is pre-verified
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	if err := repo.CreatePayee(c.Request.Context(), payee); err != nil {
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"payee": payee})
}

// OwnerListPayees handles GET /owner/payouts/payees.
func (h *Handlers) OwnerListPayees(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}
	payees, err := repo.ListPayeesByProperty(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"payees": payees})
}

// OwnerListUnbatchedPayoutItems handles GET /owner/payouts/items/unbatched.
func (h *Handlers) OwnerListUnbatchedPayoutItems(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}
	items, err := repo.ListUnbatchedPendingPayoutItems(c.Request.Context(), pid)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

type createBatchBody struct {
	Notes *string `json:"notes"`
}

// OwnerCreatePayoutBatch handles POST /owner/payouts/batches.
func (h *Handlers) OwnerCreatePayoutBatch(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}

	var body createBatchBody
	_ = c.ShouldBindJSON(&body)

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	// Generate deterministic batch number
	randBytes := make([]byte, 3)
	_, _ = rand.Read(randBytes)
	batchNumber := fmt.Sprintf("BATCH-%s-%s", time.Now().Format("20060102"), hex.EncodeToString(randBytes))

	secret := []byte(h.getChecksumSecret())
	batch, items, err := repo.CreateBatchFromUnbatchedItems(c.Request.Context(), pid, uid, batchNumber, secret, body.Notes)
	if err != nil {
		if strings.Contains(err.Error(), "no unbatched") {
			respondErr(c, clientErr(http.StatusBadRequest, "no unbatched pending payout items found"))
			return
		}
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"batch": batch, "items": items})
}

type approveBatchBody struct {
	ExpectedItemCount   int    `json:"expected_item_count" binding:"required"`
	ExpectedTotalPaise  int64  `json:"expected_total_paise" binding:"required"`
	OTP                 string `json:"otp"`
	FirebaseIDToken     string `json:"firebase_id_token"`
	ReauthConfirmation  string `json:"reauth_confirmation"`
}

func maskPhone(phone string) string {
	cleaned := strings.TrimSpace(phone)
	if len(cleaned) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(cleaned)-4) + cleaned[len(cleaned)-4:]
}

// OwnerRequestPayoutBatchOTP handles POST /owner/payouts/batches/:id/approve/request-otp.
func (h *Handlers) OwnerRequestPayoutBatchOTP(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	batchID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	batch, err := repo.GetBatchByID(c.Request.Context(), batchID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
		return
	}
	if batch.PropertyID != pid {
		c.JSON(http.StatusForbidden, gin.H{"error": "batch does not belong to owner property"})
		return
	}
	if batch.Status != domain.BatchDraft {
		respondErr(c, clientErr(http.StatusConflict, fmt.Sprintf("batch cannot request approval OTP from status '%s'", batch.Status)))
		return
	}

	if h.UserStore == nil || h.Auth == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth service not configured"})
		return
	}

	user, err := h.UserStore.GetByID(c.Request.Context(), uid)
	if err != nil {
		respondErr(c, clientErr(http.StatusUnauthorized, "authenticated owner account not found"))
		return
	}
	phone := strings.TrimSpace(user.Phone)
	if phone == "" {
		respondErr(c, clientErr(http.StatusBadRequest, "owner account has no registered phone for OTP step-up"))
		return
	}

	if err := h.Auth.RequestOTPWithPurpose(c.Request.Context(), phone, "payout_approval"); err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			respondErr(c, clientErr(http.StatusTooManyRequests, "otp rate limit exceeded; please wait before requesting another code"))
			return
		}
		respondErr(c, fmt.Errorf("dispatch approval otp: %w", err))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "payout approval code sent via SMS",
		"phone":   maskPhone(phone),
	})
}

// OwnerApprovePayoutBatch handles POST /owner/payouts/batches/:id/approve.
func (h *Handlers) OwnerApprovePayoutBatch(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	batchID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	var body approveBatchBody
	if err := c.ShouldBindJSON(&body); err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid request body: expected_item_count and expected_total_paise are required"))
		return
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	batch, err := repo.GetBatchByID(c.Request.Context(), batchID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
		return
	}
	if batch.PropertyID != pid {
		c.JSON(http.StatusForbidden, gin.H{"error": "batch does not belong to owner property"})
		return
	}

	if batch.Status != domain.BatchDraft {
		respondErr(c, clientErr(http.StatusConflict, fmt.Sprintf("batch cannot be approved from status '%s'", batch.Status)))
		return
	}

	// Affirmative verification check: must match batch item count and total paise
	if body.ExpectedItemCount != batch.ItemCount || body.ExpectedTotalPaise != batch.TotalAmountPaise {
		respondErr(c, clientErr(http.StatusBadRequest, fmt.Sprintf("batch affirmative verification mismatch: expected %d items totaling %d paise, batch has %d items totaling %d paise",
			body.ExpectedItemCount, body.ExpectedTotalPaise, batch.ItemCount, batch.TotalAmountPaise)))
		return
	}

	// Check active owners for property to evaluate dual-control vs solo-owner step-up auth
	var activeOwners []domain.User
	if h.UserStore != nil {
		owners, err := h.UserStore.GetByPropertyAndRole(c.Request.Context(), pid, domain.RoleOwner)
		if err == nil {
			activeOwners = owners
		}
	}

	approvalMode := "dual_control"
	if len(activeOwners) > 1 {
		// Strict maker-checker enforcement
		if uid == batch.CreatedBy {
			slog.Warn("dual control approval rejected: maker cannot be checker", "batch_id", batch.ID, "user_id", uid)
			respondErr(c, clientErr(http.StatusForbidden, "dual control required: payout batch must be approved by a different owner"))
			return
		}
	} else {
		// Solo owner path (or co-owner was removed): require cryptographic step-up re-authentication
		approvalMode = "solo_owner_reauth"

		candidateOTP := strings.TrimSpace(body.OTP)
		if candidateOTP == "" && len(strings.TrimSpace(body.ReauthConfirmation)) == 6 {
			isDigits := true
			for _, r := range strings.TrimSpace(body.ReauthConfirmation) {
				if r < '0' || r > '9' {
					isDigits = false
					break
				}
			}
			if isDigits {
				candidateOTP = strings.TrimSpace(body.ReauthConfirmation)
			}
		}

		candidateFirebase := strings.TrimSpace(body.FirebaseIDToken)

		if h.UserStore == nil || h.Auth == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "auth service not configured"})
			return
		}

		user, err := h.UserStore.GetByID(c.Request.Context(), uid)
		if err != nil {
			respondErr(c, clientErr(http.StatusUnauthorized, "owner account not found for step-up verification"))
			return
		}

		if candidateOTP != "" {
			phone := strings.TrimSpace(user.Phone)
			if phone == "" {
				respondErr(c, clientErr(http.StatusUnauthorized, "step-up authentication failed: owner has no registered phone"))
				return
			}
			if err := h.Auth.VerifyStepUpOTP(c.Request.Context(), phone, candidateOTP); err != nil {
				if errors.Is(err, auth.ErrInvalidOTP) {
					respondErr(c, clientErr(http.StatusUnauthorized, "invalid payout approval code"))
					return
				}
				if errors.Is(err, auth.ErrOTPExpired) {
					respondErr(c, clientErr(http.StatusUnauthorized, "payout approval code has expired; please request a new code"))
					return
				}
				if errors.Is(err, auth.ErrOTPLocked) {
					respondErr(c, clientErr(http.StatusUnauthorized, "payout approval code locked due to too many failed attempts; please request a new code"))
					return
				}
				respondErr(c, clientErr(http.StatusUnauthorized, fmt.Sprintf("step-up authentication failed: %v", err)))
				return
			}
		} else if candidateFirebase != "" {
			ident, err := h.Auth.VerifyFirebaseStepUp(c.Request.Context(), candidateFirebase, 5*time.Minute)
			if err != nil {
				if errors.Is(err, auth.ErrStaleAuthToken) {
					respondErr(c, clientErr(http.StatusUnauthorized, "firebase re-authentication is stale: fresh authentication (<5 minutes) required for approval"))
					return
				}
				respondErr(c, clientErr(http.StatusUnauthorized, "invalid firebase token for step-up authentication"))
				return
			}
			// Verify Firebase identity matches the authenticated owner
			matches := false
			if user.FirebaseUID != nil && *user.FirebaseUID == ident.UID {
				matches = true
			} else if user.Phone != "" && strings.TrimSpace(user.Phone) == ident.Phone && ident.Phone != "" {
				matches = true
			} else if user.Email != "" && strings.EqualFold(user.Email, ident.Email) && ident.Email != "" {
				matches = true
			}
			if !matches {
				respondErr(c, clientErr(http.StatusUnauthorized, "step-up authentication failed: token identity does not match authenticated owner"))
				return
			}
		} else {
			// Fail-closed: arbitrary strings (e.g. "CONFIRM") or missing credentials are strictly rejected
			respondErr(c, clientErr(http.StatusUnauthorized, "step-up authentication required: valid otp or fresh firebase_id_token must be provided for solo-owner batch approval"))
			return
		}
	}

	approvedBatch, err := repo.ApproveBatch(c.Request.Context(), batchID, uid)
	if err != nil {
		respondErr(c, err)
		return
	}

	slog.Info("payout batch approved",
		"batch_id", approvedBatch.ID,
		"batch_number", approvedBatch.BatchNumber,
		"approved_by", uid,
		"approval_mode", approvalMode,
	)

	c.JSON(http.StatusOK, gin.H{
		"batch":         approvedBatch,
		"approval_mode": approvalMode,
	})
}

// OwnerExportPayoutBatch handles GET /owner/payouts/batches/:id/export.
func (h *Handlers) OwnerExportPayoutBatch(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	batchID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	batch, err := repo.GetBatchByID(c.Request.Context(), batchID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
		return
	}
	if batch.PropertyID != pid {
		c.JSON(http.StatusForbidden, gin.H{"error": "batch does not belong to owner property"})
		return
	}

	// Gated on approval: draft or processing batches cannot be exported
	if batch.Status != domain.BatchApproved {
		respondErr(c, clientErr(http.StatusConflict, fmt.Sprintf("payout batch is in status '%s'; only approved batches can be exported", batch.Status)))
		return
	}

	items, err := repo.ListPayoutItemsByBatch(c.Request.Context(), batchID)
	if err != nil {
		respondErr(c, err)
		return
	}

	// Active Checksum Verification Gate:
	// Verify that current item rows strictly match the approved HMAC checksum.
	secret := []byte(h.getChecksumSecret())
	computedChecksum := domain.ComputeBatchChecksum(secret, items)
	if batch.FileChecksum != nil && *batch.FileChecksum != "" {
		if !hmac.Equal([]byte(*batch.FileChecksum), []byte(computedChecksum)) {
			slog.Error("payout batch checksum mismatch on export - possible tampering or manual mutation",
				"batch_id", batch.ID,
				"batch_number", batch.BatchNumber,
				"stored_checksum", *batch.FileChecksum,
				"computed_checksum", computedChecksum,
			)
			respondErr(c, clientErr(http.StatusConflict, "payout batch checksum mismatch"))
			return
		}
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"payout_%s.csv\"", batch.BatchNumber))
	if batch.FileChecksum != nil {
		c.Header("X-Batch-Checksum", *batch.FileChecksum)
	}

	writer := csv.NewWriter(c.Writer)
	defer writer.Flush()

	// Write CSV Header
	_ = writer.Write([]string{
		"Reference Number",
		"Payee ID",
		"Purpose",
		"Period",
		"Amount (Paise)",
		"Amount (INR)",
		"Status",
		"UTR",
		"Created At",
	})

	for _, it := range items {
		inr := formatPaiseToINR(it.AmountPaise)
		utr := ""
		if it.UTR != nil {
			utr = *it.UTR
		}
		_ = writer.Write([]string{
			sanitizeCSVCell(it.ReferenceNumber),
			it.PayeeID.String(),
			sanitizeCSVCell(it.Purpose),
			sanitizeCSVCell(it.PeriodLabel),
			fmt.Sprintf("%d", it.AmountPaise),
			inr,
			string(it.Status),
			sanitizeCSVCell(utr),
			it.CreatedAt.Format(time.RFC3339),
		})
	}
}

// sanitizeCSVCell prefixes any cell value beginning with formula-trigger characters with a single quote.
func sanitizeCSVCell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}

// formatPaiseToINR formats an integer paise balance to an INR string with exact 2-decimal precision without float conversion.
func formatPaiseToINR(paise int64) string {
	sign := ""
	if paise < 0 {
		sign = "-"
		paise = -paise
	}
	return fmt.Sprintf("%s%d.%02d", sign, paise/100, paise%100)
}

// Helper methods
func (h *Handlers) getPayoutRepo() *postgres.PayoutRepo {
	if h.PayoutRepo != nil {
		return h.PayoutRepo
	}
	if h.Pool != nil {
		return postgres.NewPayoutRepo(h.Pool, h.Finance)
	}
	return nil
}

func (h *Handlers) getChecksumSecret() string {
	if h.PayoutChecksumSecret != "" {
		return h.PayoutChecksumSecret
	}
	if h.JWTSecret != "" {
		return h.JWTSecret
	}
	return "payout_checksum_secret"
}

// OwnerDispatchPayoutBatch handles POST /owner/payouts/batches/:id/dispatch.
// It initiates automated Cashfree Transfers V2 batch transfer for an approved batch.
func (h *Handlers) OwnerDispatchPayoutBatch(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	batchID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	repo := h.getPayoutRepo()
	if repo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout repo not configured"})
		return
	}

	batch, err := repo.GetBatchByID(c.Request.Context(), batchID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
		return
	}
	if batch.PropertyID != pid {
		c.JSON(http.StatusForbidden, gin.H{"error": "batch does not belong to owner property"})
		return
	}

	if batch.Status != domain.BatchApproved {
		respondErr(c, clientErr(http.StatusConflict, fmt.Sprintf("payout batch is in status '%s'; only approved batches can be dispatched", batch.Status)))
		return
	}

	// Atomically transitions batch and items to 'processing' and enqueues outbox event
	updatedBatch, items, err := repo.InitiateBatchTransferTx(c.Request.Context(), batchID)
	if err != nil {
		respondErr(c, err)
		return
	}

	// Trigger inline dispatch if dispatcher is available (fast path)
	if h.PayoutDispatcher != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if dErr := h.PayoutDispatcher.DispatchBatch(ctx, batchID); dErr != nil {
				slog.Warn("inline payout batch dispatch failed, relying on ledger outbox worker",
					"batch_id", batchID,
					"err", dErr,
				)
			}
		}()
	}

	c.JSON(http.StatusOK, gin.H{
		"batch": updatedBatch,
		"items": items,
	})
}

