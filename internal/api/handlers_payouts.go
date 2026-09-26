package api

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

	items, err := repo.ListPayoutItemsByBatch(c.Request.Context(), batchID)
	if err != nil {
		respondErr(c, err)
		return
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
		inr := fmt.Sprintf("%.2f", float64(it.AmountPaise)/100.0)
		utr := ""
		if it.UTR != nil {
			utr = *it.UTR
		}
		_ = writer.Write([]string{
			it.ReferenceNumber,
			it.PayeeID.String(),
			it.Purpose,
			it.PeriodLabel,
			fmt.Sprintf("%d", it.AmountPaise),
			inr,
			string(it.Status),
			utr,
			it.CreatedAt.Format(time.RFC3339),
		})
	}
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
