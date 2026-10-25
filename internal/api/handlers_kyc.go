package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
	"golang.org/x/time/rate"
)

// ---------------------------------------------------------------------------
// Rate limiting by tenant_id (not IP) for KYC initiation.
// The RequireTenant group middleware runs first and populates auth.ContextTenantKey,
// so by the time this middleware fires the tenant is guaranteed to be set.
// ---------------------------------------------------------------------------

// tenantIDRateLimit returns a per-tenant-ID token-bucket middleware. The limiter
// map is local to each returned closure, so different routes get independent budgets.
//
// Memory bounding: tracks up to 5,000 unique tenant IDs with a 30-minute idle TTL.
// When capacity is reached, expired entries are purged; if still full, the oldest
// entry is evicted. This matches the bounding strategy of ipRateLimitBounded.
//
// NOTE: Single-process in-memory limitation:
// This rate limiter maintains an in-memory map per process instance. In a
// multi-replica deployment each instance enforces its own limit independently,
// allowing a tenant up to N × burst across N replicas. For true distributed
// enforcement, replace with a Redis sliding window or Postgres advisory store.
func tenantIDRateLimit(rps float64, burst int) gin.HandlerFunc {
	const (
		maxEntries = 5000
		entryTTL   = 30 * time.Minute
	)

	type entry struct {
		limiter  *rate.Limiter
		lastSeen time.Time
	}

	var mu sync.Mutex
	limiters := make(map[string]*entry)

	sweep := func(now time.Time) {
		for k, e := range limiters {
			if now.Sub(e.lastSeen) > entryTTL {
				delete(limiters, k)
			}
		}
	}

	return func(c *gin.Context) {
		v, ok := c.Get(auth.ContextTenantKey)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		t, ok := v.(*domain.Tenant)
		if !ok || t == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		key := t.ID.String()
		now := time.Now()

		mu.Lock()
		e, ok := limiters[key]
		if !ok {
			if len(limiters) >= maxEntries {
				sweep(now)
			}
			if len(limiters) >= maxEntries {
				// Evict oldest entry if still at capacity after sweep.
				var oldestKey string
				var oldestTime time.Time
				for k, v := range limiters {
					if oldestKey == "" || v.lastSeen.Before(oldestTime) {
						oldestKey = k
						oldestTime = v.lastSeen
					}
				}
				if oldestKey != "" {
					delete(limiters, oldestKey)
				}
			}
			e = &entry{
				limiter:  rate.NewLimiter(rate.Limit(rps), burst),
				lastSeen: now,
			}
			limiters[key] = e
		} else {
			e.lastSeen = now
		}
		allowed := e.limiter.Allow()
		mu.Unlock()

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
			return
		}
		c.Next()
	}
}

// ---------------------------------------------------------------------------
// Error mapping — translates service sentinel errors to typed ClientErrors.
// ---------------------------------------------------------------------------

func kycClientErr(err error) error {
	var apiErr *cashfree.APIError
	if errors.As(err, &apiErr) {
		msg := apiErr.Error()
		if apiErr.Body != "" {
			var bodyMap map[string]any
			if json.Unmarshal([]byte(apiErr.Body), &bodyMap) == nil {
				if m, ok := bodyMap["message"].(string); ok && m != "" {
					msg = m
				}
			}
		}
		return clientErrWithCode(http.StatusBadGateway, msg, apierr.Code("kyc.upstream_error"))
	}
	switch {
	case errors.Is(err, kycsvc.ErrNoActiveConsent):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.no_consent"))
	case errors.Is(err, domain.ErrAlreadyVerified):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.already_verified"))
	case errors.Is(err, domain.ErrVerificationInProgress):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.in_flight"))
	case errors.Is(err, domain.ErrConsentRevoked):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.consent_revoked"))
	case errors.Is(err, kycsvc.ErrUnsupportedDocumentType):
		return clientErrWithCode(http.StatusUnprocessableEntity, err.Error(), apierr.CodeRequestInvalidBody)
	case errors.Is(err, kycsvc.ErrQRSignatureInvalid):
		return clientErrWithCode(http.StatusUnprocessableEntity, err.Error(), apierr.Code("kyc.qr_invalid"))
	case errors.Is(err, kycsvc.ErrQRDataIncomplete):
		return clientErrWithCode(http.StatusUnprocessableEntity, err.Error(), apierr.Code("kyc.qr_incomplete"))
	case errors.Is(err, kycsvc.ErrCashfreeUnavailable):
		return clientErrWithCode(http.StatusServiceUnavailable, err.Error(), apierr.Code("kyc.cashfree_unavailable"))
	case errors.Is(err, domain.ErrVerificationNotFound):
		return clientErrWithCode(http.StatusNotFound, "verification not found", apierr.Code("kyc.not_found"))
	case errors.Is(err, domain.ErrVerificationNotPending):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.not_pending"))
	default:
		return err
	}
}

// kycSvcOrErr gates every handler against a nil KYCSvc and writes 503 if unset.
func (h *Handlers) kycSvcOrErr(c *gin.Context) KYCService {
	if h.KYCSvc == nil {
		c.JSON(http.StatusServiceUnavailable, apierr.ErrorEnvelope{Error: "kyc service not configured"})
		return nil
	}
	return h.KYCSvc
}

// ---------------------------------------------------------------------------
// Tenant-facing KYC endpoints
// ---------------------------------------------------------------------------

type kycConsentBody struct {
	Purpose        string `json:"purpose"         binding:"required"`
	ConsentVersion string `json:"consent_version" binding:"required"`
	ConsentText    string `json:"consent_text"`
	Locale         string `json:"locale"`
}

// TenantKYCConsent handles POST /api/tenant/kyc/consent.
// Records the tenant's DPDP consent before any verification is allowed.
func (h *Handlers) TenantKYCConsent(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !h.requirePropertyActiveModule(c, t.PropertyID, "kyc") {
		return
	}

	var body kycConsentBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "purpose and consent_version are required", apierr.CodeRequestInvalidBody)
		return
	}

	locale := body.Locale
	if locale == "" {
		locale = c.GetHeader("Accept-Language")
	}
	consentText := body.ConsentText
	if consentText == "" {
		consentText = kycsvc.ResolveConsentText(body.ConsentVersion, locale)
	}

	ip := c.ClientIP()
	ua := c.Request.UserAgent()
	actor := fmt.Sprintf("tenant:%s", t.ID)

	consent, err := svc.RecordConsent(c.Request.Context(),
		t.ID, body.Purpose, body.ConsentVersion, consentText, ip, ua, actor)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":               consent.ID,
		"consent_version":  consent.ConsentVersion,
		"consent_given_at": consent.ConsentGivenAt,
	})
}

// TenantKYCRevoke handles POST /api/tenant/kyc/revoke.
// Implements DPDP Rule 8 erasure: nulls PII from all related records.
func (h *Handlers) TenantKYCRevoke(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !h.requirePropertyActiveModule(c, t.PropertyID, "kyc") {
		return
	}
	actor := fmt.Sprintf("tenant:%s", t.ID)

	if err := svc.RevokeConsent(c.Request.Context(), t.ID, actor); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "consent revoked and PII scrubbed"})
}

// TenantKYCStatus handles GET /api/tenant/kyc/status.
// Returns a tenant-safe view: status, method, verified_at, expires_at.
// Does NOT expose identity_hash, duplicate_detected, or vendor_reference_id.
func (h *Handlers) TenantKYCStatus(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !h.requirePropertyActiveModule(c, t.PropertyID, "kyc") {
		return
	}

	v, consent, err := svc.GetStatus(c.Request.Context(), t.ID)
	if err != nil {
		respondErr(c, err)
		return
	}

	resp := gin.H{
		"has_consent":      consent != nil,
		"has_verification": v != nil,
	}
	if consent != nil {
		resp["consent_version"] = consent.ConsentVersion
		resp["consent_given_at"] = consent.ConsentGivenAt
	}
	if v != nil {
		resp["verification"] = tenantKYCVerificationView(v)
	}
	c.JSON(http.StatusOK, resp)
}


// TenantKYCUpload handles POST /api/tenant/kyc/upload.
// Presents a single Aadhaar document (image/PDF up to 5MB) for Smart OCR verification.
func (h *Handlers) TenantKYCUpload(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !h.requirePropertyActiveModule(c, t.PropertyID, "kyc") {
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		fileHeader, err = c.FormFile("document")
	}
	if err != nil {
		apierr.RespondBindErr(c, "file field is required in multipart form", apierr.CodeRequestInvalidBody)
		return
	}

	if fileHeader.Size > 5*1024*1024 {
		apierr.RespondClientErr(c, http.StatusBadRequest, "file size exceeds 5MB limit", apierr.CodeRequestInvalidBody)
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		apierr.RespondClientErr(c, http.StatusBadRequest, "unable to open uploaded file", apierr.CodeRequestInvalidBody)
		return
	}
	defer file.Close()

	ct := fileHeader.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	actor := fmt.Sprintf("tenant:%s", t.ID)
	v, err := svc.VerifyAadhaarDocument(c.Request.Context(), t.ID, file, fileHeader.Filename, ct, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}

	c.JSON(http.StatusOK, tenantKYCVerificationView(v))
}

type secureQRVerifyBody struct {
	QRPayload string `json:"qr_payload" binding:"required"`
}

// TenantKYCSecureQRVerify handles POST /api/tenant/kyc/qr/verify.
// Offline Aadhaar Secure QR verification: the UIDAI RSA-SHA256 signature is checked
// in-process (no network call to Cashfree/UIDAI), so this completes synchronously and
// is the lowest-latency KYC path available — lower than Aadhaar OTP, which depends on
// SMS delivery, and lower than document upload, which depends on manual review.
func (h *Handlers) TenantKYCSecureQRVerify(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !h.requirePropertyActiveModule(c, t.PropertyID, "kyc") {
		return
	}

	var body secureQRVerifyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "qr_payload is required", apierr.CodeRequestInvalidBody)
		return
	}
	actor := fmt.Sprintf("tenant:%s", t.ID)

	v, err := svc.VerifySecureQR(c.Request.Context(), t.ID, body.QRPayload, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	c.JSON(http.StatusOK, tenantKYCVerificationView(v))
}

type aadhaarOTPRequestBody struct {
	AadhaarNumber string `json:"aadhaar_number" binding:"required"`
}

// TenantKYCAadhaarOTPRequest handles POST /api/tenant/kyc/aadhaar/otp/request.
// Triggers direct UIDAI OTP generation via Cashfree.
func (h *Handlers) TenantKYCAadhaarOTPRequest(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !h.requirePropertyActiveModule(c, t.PropertyID, "kyc") {
		return
	}

	var body aadhaarOTPRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "aadhaar_number is required", apierr.CodeRequestInvalidBody)
		return
	}
	actor := fmt.Sprintf("tenant:%s", t.ID)

	refID, err := svc.InitiateAadhaarOTP(c.Request.Context(), t.ID, body.AadhaarNumber, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"ref_id": refID})
}

type aadhaarOTPVerifyBody struct {
	RefID string `json:"ref_id" binding:"required"`
	OTP   string `json:"otp"    binding:"required"`
}

// TenantKYCAadhaarOTPVerify handles POST /api/tenant/kyc/aadhaar/otp/verify.
// Completes verification with UIDAI and auto-verifies immediately (no manual review).
func (h *Handlers) TenantKYCAadhaarOTPVerify(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !h.requirePropertyActiveModule(c, t.PropertyID, "kyc") {
		return
	}

	var body aadhaarOTPVerifyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "ref_id and otp are required", apierr.CodeRequestInvalidBody)
		return
	}
	actor := fmt.Sprintf("tenant:%s", t.ID)

	v, err := svc.CompleteAadhaarOTP(c.Request.Context(), t.ID, body.RefID, body.OTP, actor)
	if err != nil {
		var fail *kycsvc.AadhaarVerifyFailure
		if errors.As(err, &fail) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":              fail.Error(),
				"code":               "kyc.aadhaar_verify_failed",
				"status":             "failed",
				"reason_code":        fail.ReasonCode,
				"retryable":          fail.Retryable,
				"cooldown_seconds":   fail.CooldownSeconds,
				"attempts_remaining": fail.AttemptsRemaining,
			})
			return
		}
		respondErr(c, kycClientErr(err))
		return
	}
	c.JSON(http.StatusOK, tenantKYCVerificationView(v))
}

// ---------------------------------------------------------------------------
// Owner-facing KYC endpoints
// ---------------------------------------------------------------------------

// OwnerGetTenantKYC handles GET /api/owner/tenants/:id/kyc.
// Returns full KYC view including duplicate_detected and audit trail.
// Property authorization is enforced: owner can only view tenants on their property.
func (h *Handlers) OwnerGetTenantKYC(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	propertyID, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	tenantID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	// Property authorization check.
	if h.TenantStore == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "internal error"})
		return
	}
	tenant, err := h.TenantStore.GetByID(c.Request.Context(), tenantID)
	if err != nil || tenant.PropertyID != propertyID {
		apierr.RespondClientErr(c, http.StatusNotFound, "tenant not found", apierr.CodeRequestInvalidId)
		return
	}

	v, logs, err := svc.GetOwnerView(c.Request.Context(), tenantID)
	if err != nil {
		respondErr(c, err)
		return
	}

	resp := gin.H{
		"tenant_id":  tenantID,
		"audit_logs": logs,
	}
	if v != nil {
		resp["verification"] = ownerKYCVerificationView(v)
	}
	c.JSON(http.StatusOK, resp)
}

type clearDuplicateBody struct {
	Reason string `json:"reason" binding:"required"`
}

// OwnerClearDuplicateKYC handles POST /api/owner/tenants/:id/kyc/clear-duplicate.
// Clears a false-positive duplicate_detected flag with a mandatory audited reason.
func (h *Handlers) OwnerClearDuplicateKYC(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	propertyID, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	tenantID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body clearDuplicateBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "reason is required", apierr.CodeRequestInvalidBody)
		return
	}

	// Property authorization check.
	if h.TenantStore == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "internal error"})
		return
	}
	tenant, err := h.TenantStore.GetByID(c.Request.Context(), tenantID)
	if err != nil || tenant.PropertyID != propertyID {
		apierr.RespondClientErr(c, http.StatusNotFound, "tenant not found", apierr.CodeRequestInvalidId)
		return
	}

	// Fetch current verification to get the verification ID.
	v, _, err := svc.GetStatus(c.Request.Context(), tenantID)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	if v == nil {
		apierr.RespondClientErr(c, http.StatusNotFound, "no active verification found", apierr.Code("kyc.not_found"))
		return
	}

	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	actor := fmt.Sprintf("owner:%s", claims.UserID)

	if err := svc.ClearDuplicateFlag(c.Request.Context(), v.ID, actor, body.Reason); err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type recordGuardianConsentBody struct {
	GuardianName  string  `json:"guardian_name" binding:"required"`
	GuardianPhone string  `json:"guardian_phone" binding:"required"`
	Relationship  string  `json:"relationship" binding:"required,oneof=parent legal_guardian"`
	ReferenceID   *string `json:"reference_id"`
}

// OwnerRecordGuardianConsent handles POST /api/owner/tenants/:id/kyc/guardian-consent.
func (h *Handlers) OwnerRecordGuardianConsent(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	propertyID, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	tenantID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body recordGuardianConsentBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid guardian details", apierr.CodeRequestInvalidBody)
		return
	}

	tenant, err := h.TenantStore.GetByID(c.Request.Context(), tenantID)
	if err != nil || tenant.PropertyID != propertyID {
		apierr.RespondClientErr(c, http.StatusNotFound, "tenant not found", apierr.CodeRequestInvalidId)
		return
	}

	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	actor := fmt.Sprintf("owner:%s", claims.UserID)

	refID := fmt.Sprintf("guard_manual_%s", uuid.New().String())
	if body.ReferenceID != nil && *body.ReferenceID != "" {
		refID = *body.ReferenceID
	}

	updatedTenant, err := svc.RecordGuardianConsent(
		c.Request.Context(),
		tenantID,
		body.GuardianName,
		body.GuardianPhone,
		body.Relationship,
		refID,
		actor,
	)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":     true,
		"tenant": tenantResponse(updatedTenant),
	})
}

// OwnerTenantKYCPhoto handles GET /api/owner/tenants/:id/kyc/photo.
// Returns the verified government/vendor-attested photo crop as raw image bytes.
func (h *Handlers) OwnerTenantKYCPhoto(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	propertyID, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	tenantID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.TenantStore == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "internal error"})
		return
	}
	tenant, err := h.TenantStore.GetByID(c.Request.Context(), tenantID)
	if err != nil || tenant.PropertyID != propertyID {
		apierr.RespondClientErr(c, http.StatusNotFound, "tenant not found", apierr.CodeRequestInvalidId)
		return
	}

	b, err := svc.GetAttestedPhoto(c.Request.Context(), tenantID)
	if err != nil || len(b) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no kyc photo"})
		return
	}
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
	ct := http.DetectContentType(b)
	c.Data(http.StatusOK, ct, b)
}

// OwnerKYCDocument handles GET /api/owner/kyc/:id/document and GET /api/manager/kyc/:id/document.
// Serves the uploaded manual review document binary (PDF/image) or fallback attested photo.
func (h *Handlers) OwnerKYCDocument(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	propertyID, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	b, _, err := svc.GetKYCDocument(c.Request.Context(), id, propertyID)
	if err != nil {
		if errors.Is(err, domain.ErrVerificationNotFound) {
			apierr.RespondClientErr(c, http.StatusNotFound, "document not found", apierr.CodeRequestInvalidId)
			return
		}
		respondErr(c, kycClientErr(err))
		return
	}
	if len(b) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no document available"})
		return
	}

	// Serve only signature-verified types, whatever content type was stored at upload time.
	// This also protects rows written before the upload check existed.
	sniffed, sniffErr := kycsvc.SniffDocumentMIME(b)
	if sniffErr != nil {
		apierr.RespondClientErr(c, http.StatusUnsupportedMediaType, "stored document is not a supported type", apierr.CodeRequestInvalidBody)
		return
	}
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, sniffed, b)
}

// OwnerKYCReviewQueue handles GET /api/owner/kyc/queue.
// Lists pending manual review verifications for the owner's property.
func (h *Handlers) OwnerKYCReviewQueue(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	propertyID, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	items, err := svc.ListPendingReviews(c.Request.Context(), propertyID)
	if err != nil {
		respondErr(c, err)
		return
	}

	views := make([]gin.H, len(items))
	for i := range items {
		views[i] = ownerKYCVerificationView(&items[i])
	}
	c.JSON(http.StatusOK, gin.H{"items": views})
}

// OwnerKYCApprove handles POST /api/owner/kyc/:id/approve.
// Approves a pending manual upload verification.
func (h *Handlers) OwnerKYCApprove(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	verificationID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	actor := fmt.Sprintf("owner:%s", claims.UserID)
	v, err := svc.ApproveManualReview(c.Request.Context(), verificationID, claims.UserID, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	c.JSON(http.StatusOK, ownerKYCVerificationView(v))
}

type kycRejectBody struct {
	Reason string `json:"reason" binding:"required"`
}

// OwnerKYCReject handles POST /api/owner/kyc/:id/reject.
// Rejects a pending manual upload verification with a mandatory human reason.
func (h *Handlers) OwnerKYCReject(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	verificationID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body kycRejectBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "reason is required", apierr.CodeRequestInvalidBody)
		return
	}

	actor := fmt.Sprintf("owner:%s", claims.UserID)
	v, err := svc.RejectManualReview(c.Request.Context(), verificationID, claims.UserID, body.Reason, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	c.JSON(http.StatusOK, ownerKYCVerificationView(v))
}

// ---------------------------------------------------------------------------
// Cashfree KYC webhook  (POST /api/public/cashfree/kyc/webhook)
// ---------------------------------------------------------------------------

// kycWebhookPayload is the minimal Cashfree Secure ID webhook envelope.
type kycWebhookPayload struct {
	Type string `json:"type"` // VERIFICATION_COMPLETED | VERIFICATION_FAILED
	Data struct {
		VerificationID string `json:"verification_id"`
		Status         string `json:"status"` // COMPLETED | FAILED
		Entity         string `json:"entity"` // AADHAAR
		FailedReason   string `json:"failed_reason,omitempty"`
	} `json:"data"`
}

// CashfreeKYCWebhook handles POST /api/public/cashfree/kyc/webhook.
//
// Security: raw body bytes are read FIRST, HMAC is verified BEFORE JSON parsing.
// DigiLocker sessions are retired; webhook validates authenticity and acknowledges with 200 OK.
//
// Response status codes:
//   - 200: validly signed delivery acknowledged
//   - 503: webhook secret not configured
func (h *Handlers) CashfreeKYCWebhook(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	if h.CashfreeSecret == "" {
		c.JSON(http.StatusServiceUnavailable, apierr.ErrorEnvelope{Error: "kyc webhook secret not configured"})
		return
	}

	// 1. Read raw body FIRST — HMAC verification must happen over exact wire bytes.
	raw, err := c.GetRawData()
	if err != nil {
		c.Status(http.StatusOK) // do not expose internals; Cashfree will retry
		return
	}

	// 2. Verify HMAC before any further processing.
	timestamp := c.GetHeader("x-webhook-timestamp")
	signature := c.GetHeader("x-webhook-signature")
	if !cashfree.VerifyWebhookHMAC(h.CashfreeSecret, timestamp, string(raw), signature) {
		// Log bad HMAC separately as a security signal
		slog.Warn("cashfree kyc webhook rejected: bad hmac signature",
			"remote_ip", c.ClientIP(),
			"timestamp", timestamp,
		)
		c.Status(http.StatusOK)
		return
	}

	// 2.5 Verify timestamp tolerance (replay drift check) with fail-closed default (300s)
	tolerance := h.WebhookToleranceSec
	if tolerance <= 0 {
		tolerance = 300
	}
	if err := cashfree.VerifyWebhookTimestamp(timestamp, tolerance, time.Now().UTC()); err != nil {
		slog.Warn("cashfree kyc webhook rejected: timestamp tolerance exceeded",
			"remote_ip", c.ClientIP(),
			"timestamp", timestamp,
			"err", err,
		)
		c.Status(http.StatusOK)
		return
	}

	// 3. Parse payload from raw bytes (never re-read from c.Request.Body).
	var payload kycWebhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		// Log malformed JSON as an upstream/integration issue
		slog.Error("cashfree kyc webhook rejected: malformed payload json",
			"remote_ip", c.ClientIP(),
			"err", err,
		)
		c.Status(http.StatusOK)
		return
	}

	// 4. DigiLocker is deprecated and retired. Acknowledge validly signed webhooks with 200 OK.
	slog.Info("cashfree kyc webhook received",
		"event_type", payload.Type,
		"verification_id", payload.Data.VerificationID,
	)
	c.Status(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Projection helpers — enforce tenant/owner field visibility boundaries.
// ---------------------------------------------------------------------------

// tenantKYCVerificationView strips fields that would leak fraud signals to the
// tenant (identity_hash, duplicate_detected, vendor_reference_id).
func tenantKYCVerificationView(v *domain.KYCVerification) gin.H {
	m := gin.H{
		"id":         v.ID,
		"status":     v.Status,
		"method":     v.Method,
		"created_at": v.CreatedAt,
		"updated_at": v.UpdatedAt,
	}
	if v.VerifiedAt != nil {
		m["verified_at"] = v.VerifiedAt
	}
	if v.ExpiresAt != nil {
		m["expires_at"] = v.ExpiresAt
	}
	if v.MaskedUID != nil {
		m["masked_uid"] = v.MaskedUID
	}
	if v.FailureReason != nil {
		m["failure_reason"] = v.FailureReason
	}
	if v.RejectionReason != nil {
		m["rejection_reason"] = v.RejectionReason
	}
	return m
}

// ownerKYCVerificationView includes duplicate_detected so the owner can review
// flagged records. Still excludes raw identity_hash (irreversible HMAC — no use
// to an owner, and reduces risk if the response is logged).
func ownerKYCVerificationView(v *domain.KYCVerification) gin.H {
	m := tenantKYCVerificationView(v)
	m["duplicate_detected"] = v.DuplicateDetected
	m["is_dedupable"] = v.IsDedupable
	m["trust_tier"] = v.TrustTier()
	m["qr_status"] = v.QRStatus
	m["photo_stored"] = v.PhotoStored
	m["name_mismatch"] = v.NameMismatch
	if v.ReviewedBy != nil {
		m["reviewed_by"] = v.ReviewedBy
	}
	if v.ReviewedAt != nil {
		m["reviewed_at"] = v.ReviewedAt
	}
	m["document_url"] = fmt.Sprintf("/api/owner/kyc/%s/document", v.ID)
	if v.DocumentURL != nil {
		m["document_name"] = v.DocumentURL
	}
	return m
}
