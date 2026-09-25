package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
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
// NOTE: Single-process in-memory limitation:
// This rate limiter maintains an in-memory map of token buckets per instance. If pg-go is
// scaled to multiple replicas/pods behind a load balancer, each instance enforces its own
// limit independently, allowing a tenant to consume up to N × burst requests across N replicas.
// For multi-replica horizontal scaling, replace this with a distributed limiter (e.g. Redis
// sliding window or Postgres advisory/token store).
func tenantIDRateLimit(rps float64, burst int) gin.HandlerFunc {
	var mu sync.Mutex
	limiters := make(map[string]*rate.Limiter)

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

		mu.Lock()
		l, ok := limiters[key]
		if !ok {
			l = rate.NewLimiter(rate.Limit(rps), burst)
			limiters[key] = l
		}
		mu.Unlock()

		if !l.Allow() {
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
	switch {
	case errors.Is(err, kycsvc.ErrNoActiveConsent):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.no_consent"))
	case errors.Is(err, domain.ErrAlreadyVerified):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.already_verified"))
	case errors.Is(err, domain.ErrVerificationInProgress):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.in_flight"))
	case errors.Is(err, domain.ErrConsentRevoked):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.consent_revoked"))
	case errors.Is(err, kycsvc.ErrQRSignatureInvalid):
		return clientErrWithCode(http.StatusUnprocessableEntity, err.Error(), apierr.Code("kyc.qr_invalid"))
	case errors.Is(err, kycsvc.ErrQRDataIncomplete):
		return clientErrWithCode(http.StatusUnprocessableEntity, err.Error(), apierr.Code("kyc.qr_incomplete"))
	case errors.Is(err, kycsvc.ErrDigiLockerUnavailable):
		return clientErrWithCode(http.StatusServiceUnavailable, err.Error(), apierr.Code("kyc.digilocker_unavailable"))
	case errors.Is(err, domain.ErrVerificationNotFound):
		return clientErrWithCode(http.StatusNotFound, "verification not found", apierr.Code("kyc.not_found"))
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
	ConsentText    string `json:"consent_text"    binding:"required"`
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

	var body kycConsentBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "purpose, consent_version and consent_text are required", apierr.CodeRequestInvalidBody)
		return
	}

	ip := c.ClientIP()
	ua := c.Request.UserAgent()
	actor := fmt.Sprintf("tenant:%s", t.ID)

	consent, err := svc.RecordConsent(c.Request.Context(),
		t.ID, body.Purpose, body.ConsentVersion, body.ConsentText, ip, ua, actor)
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

// TenantKYCInitiate handles POST /api/tenant/kyc/initiate.
// Rate-limited per tenant_id (10 req/min, burst 2).
// Returns the DigiLocker consent URL — redirect is server-constructed from config.
func (h *Handlers) TenantKYCInitiate(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	actor := fmt.Sprintf("tenant:%s", t.ID)

	url, err := svc.InitiateDigiLocker(c.Request.Context(), t.ID, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"verification_url": url})
}

type kycSubmitQRBody struct {
	RawQR string `json:"raw_qr" binding:"required"`
}

// TenantKYCSubmitQR handles POST /api/tenant/kyc/qr.
// Offline fallback path: decodes and RSA-verifies the UIDAI Secure QR in-process.
// Consent must be active before this endpoint accepts a submission.
func (h *Handlers) TenantKYCSubmitQR(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}

	var body kycSubmitQRBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "raw_qr is required", apierr.CodeRequestInvalidBody)
		return
	}
	actor := fmt.Sprintf("tenant:%s", t.ID)

	v, err := svc.VerifySecureQR(c.Request.Context(), t.ID, body.RawQR, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	// Tenant-facing projection: no identity_hash or duplicate_detected.
	c.JSON(http.StatusOK, tenantKYCVerificationView(v))
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

// TenantKYCReturn handles GET /api/tenant/kyc/return?vendor_ref_id=<id>.
// This is the browser redirect landing endpoint after DigiLocker consent is completed.
// It inspects or synchronizes verification status and returns the tenant-safe view.
func (h *Handlers) TenantKYCReturn(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		return
	}

	vendorRefID := c.Query("vendor_ref_id")
	if vendorRefID == "" {
		// Fallback to active verification for this tenant
		v, _, err := svc.GetStatus(c.Request.Context(), t.ID)
		if err != nil {
			respondErr(c, kycClientErr(err))
			return
		}
		if v == nil {
			apierr.RespondClientErr(c, http.StatusNotFound, "verification not found", apierr.Code("kyc.not_found"))
			return
		}
		vendorRefID = v.VendorReferenceID
	}

	v, err := svc.GetDigiLockerReturnStatus(c.Request.Context(), vendorRefID)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	c.JSON(http.StatusOK, tenantKYCVerificationView(v))
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

	fileHeader, err := c.FormFile("file")
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

	actor := fmt.Sprintf("tenant:%s", t.ID)
	v, err := svc.VerifyAadhaarDocument(c.Request.Context(), t.ID, file, fileHeader.Filename, actor)
	if err != nil {
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
	ct := http.DetectContentType(b)
	c.Data(http.StatusOK, ct, b)
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
// Idempotency: ProcessDigiLockerCompletion no-ops on already-terminal verifications.
//
// Response status codes:
//   - 200: malformed payload, bad HMAC, unknown event type, or unknown vendor reference (drop forever, no retry wanted)
//   - 200: successful completion or already-processed idempotent no-op
//   - 500: transient processing failure (DB error, network blip fetching doc, etc.) so Cashfree retries delivery
func (h *Handlers) CashfreeKYCWebhook(c *gin.Context) {
	svc := h.kycSvcOrErr(c)
	if svc == nil {
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

	// 4. Ignore non-KYC event types silently.
	switch payload.Type {
	case "VERIFICATION_COMPLETED", "VERIFICATION_FAILED":
		// handled below
	default:
		c.Status(http.StatusOK)
		return
	}

	vendorRefID := payload.Data.VerificationID
	if vendorRefID == "" {
		c.Status(http.StatusOK)
		return
	}

	// 5. Delegate to service — which handles idempotency internally.
	failedReason := ""
	if payload.Type == "VERIFICATION_FAILED" {
		failedReason = payload.Data.FailedReason
		if failedReason == "" {
			failedReason = "verification failed (reason not provided)"
		}
	}

	if err := svc.ProcessDigiLockerCompletion(c.Request.Context(), vendorRefID, failedReason, "cashfree_webhook"); err != nil {
		if errors.Is(err, domain.ErrVerificationNotFound) {
			slog.Warn("cashfree kyc webhook: unknown vendor reference id",
				"vendor_ref_id", vendorRefID,
			)
			c.Status(http.StatusOK)
			return
		}
		slog.Error("cashfree kyc webhook: transient processing failure",
			"vendor_ref_id", vendorRefID,
			"err", err,
		)
		c.Status(http.StatusInternalServerError)
		return
	}
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
	return m
}
