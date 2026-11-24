package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
	"github.com/pg-cashflow/pg-go/internal/postgres"
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
// NOTE: This in-memory limiter is only the fallback when no database pool exists (unit tests,
// local runs). Production routes use Handlers.TenantIDRateLimit, which is PostgreSQL-backed
// (ratelimit_pg.go), is shared by all replicas, and fails closed on a database error.
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
// Error mapping â€” translates service sentinel errors to typed ClientErrors.
// ---------------------------------------------------------------------------

func kycClientErr(err error) error {
	var apiErr *cashfree.APIError
	if errors.As(err, &apiErr) {
		// Data minimization: log HTTP status code only, strictly redacting raw provider body containing PII or document tokens
		slog.Error("kyc upstream provider error", "status", apiErr.StatusCode, "status_text", apiErr.Status)
		return clientErrWithCode(http.StatusBadGateway, "identity verification service encountered an upstream error; please try again later", apierr.Code("kyc.upstream_error"))
	}
	switch {
	case errors.Is(err, kycsvc.ErrNoActiveConsent):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.no_consent"))
	case errors.Is(err, domain.ErrAlreadyVerified):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.already_verified"))
	case errors.Is(err, domain.ErrVerificationInProgress):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.in_flight"))
	case errors.Is(err, domain.ErrDailyOTPLimitExceeded):
		return clientErrWithCode(http.StatusTooManyRequests, err.Error(), apierr.Code("kyc.daily_limit_exceeded"))
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
	case errors.Is(err, kycsvc.ErrInvalidAadhaarNumber):
		return clientErrWithCode(http.StatusBadRequest, err.Error(), apierr.Code("kyc.invalid_aadhaar"))
	case errors.Is(err, domain.ErrVerificationNotFound):
		return clientErrWithCode(http.StatusNotFound, "verification not found", apierr.Code("kyc.not_found"))
	case errors.Is(err, domain.ErrVerificationNotPending):
		return clientErrWithCode(http.StatusConflict, err.Error(), apierr.Code("kyc.not_pending"))
	case errors.Is(err, postgres.ErrCapabilityDenied), errors.Is(err, postgres.ErrManagerInactive):
		return clientErrWithCode(http.StatusForbidden, "manager lacks required permission to review kyc documents", apierr.CodeAuthForbidden)
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
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)

	var body kycConsentBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "purpose and consent_version are required", apierr.CodeRequestInvalidBody)
		return
	}

	locale := body.Locale
	if locale == "" {
		locale = c.GetHeader("Accept-Language")
	}
	if err := kycsvc.ValidateConsentPurpose(body.Purpose); err != nil {
		apierr.RespondBindErr(c, "unsupported consent purpose", apierr.CodeRequestInvalidBody)
		return
	}
	// Canonical server-owned consent text guarantees cryptographic proof of what was displayed.
	// Client-authored strings are ignored to prevent tampering with DPDP consent evidence.
	consentText, err := kycsvc.ResolveConsentText(body.ConsentVersion, locale)
	if err != nil {
		apierr.RespondBindErr(c, "unsupported consent_version", apierr.CodeRequestInvalidBody)
		return
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
// Invariant: Consent revocation must remain unconditionally accessible even if
// the property owner has deactivated the KYC module.
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
		// methods tells the client which verification routes the server will accept, so the
		// UI never shows a button that the server rejects (for example, a disabled DigiLocker).
		"methods": gin.H{
			"digilocker": h.FeatureKYCDigiLocker,
			"upload":     true,
			"qr":         true,
		},
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
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 6*1024*1024)

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
	docTypeRaw := strings.TrimSpace(c.PostForm("document_type"))
	normDocType, parseErr := domain.ParseDocumentType(docTypeRaw)
	if parseErr != nil {
		apierr.RespondBindErr(c, parseErr.Error(), apierr.CodeRequestInvalidBody)
		return
	}

	v, err := svc.VerifyDocument(c.Request.Context(), t.ID, file, fileHeader.Filename, ct, string(normDocType), actor)
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
// is the lowest-latency KYC path available â€” lower than Aadhaar OTP, which depends on
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
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)

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

type tenantKYCDigiLockerInitRequest struct {
	UserFlow string `json:"user_flow"`
}

// TenantKYCDigiLockerInit handles POST /api/tenant/kyc/digilocker/init.
// Generates a Cashfree DigiLocker authorization URL with server-controlled redirect URL.
func (h *Handlers) TenantKYCDigiLockerInit(c *gin.Context) {
	if !h.FeatureKYCDigiLocker {
		apierr.RespondClientErr(c, http.StatusForbidden, "digilocker verification is disabled", apierr.Code("kyc.methodDisabled"))
		return
	}
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	if !h.requirePropertyActiveModule(c, t.PropertyID, "kyc") {
		return
	}

	var req tenantKYCDigiLockerInitRequest
	if err := c.ShouldBindJSON(&req); err != nil && err != io.EOF {
		apierr.RespondBindErr(c, "invalid request body", apierr.CodeRequestInvalidBody)
		return
	}

	// Fail closed: enforce active DPDP consent before session initiation
	_, consent, err := svc.GetStatus(c.Request.Context(), t.ID)
	if err != nil {
		respondErr(c, err)
		return
	}
	if consent == nil {
		apierr.RespondClientErr(c, http.StatusConflict, "no active consent recorded", apierr.Code("kyc.no_consent"))
		return
	}

	// Security: construct redirect_url strictly on server from configured origin
	baseURL := strings.TrimRight(h.FrontendURL, "/")
	if baseURL == "" {
		baseURL = "https://app.pgcashflow.in"
	}
	redirectURL := baseURL + "/tenant/kyc/return"

	actor := fmt.Sprintf("tenant:%s", t.ID)
	v, sessionURL, err := svc.InitiateDigiLockerVerification(c.Request.Context(), t.ID, redirectURL, req.UserFlow, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"verification_id": v.VendorReferenceID,
		"redirect_url":    sessionURL,
		"status":          v.Status,
	})
}

// TenantKYCDigiLockerStatus handles GET /api/tenant/kyc/digilocker/status.
// Polls Cashfree DigiLocker consent and document status and reconciles terminal state.
func (h *Handlers) TenantKYCDigiLockerStatus(c *gin.Context) {
	if !h.FeatureKYCDigiLocker {
		apierr.RespondClientErr(c, http.StatusForbidden, "digilocker verification is disabled", apierr.Code("kyc.methodDisabled"))
		return
	}
	svc := h.kycSvcOrErr(c)
	if svc == nil {
		return
	}
	t := tenantFromContext(c)
	if t == nil {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	if !h.requirePropertyActiveModule(c, t.PropertyID, "kyc") {
		return
	}

	actor := fmt.Sprintf("tenant:%s", t.ID)
	v, err := svc.PollDigiLockerStatus(c.Request.Context(), t.ID, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":           true,
		"verification": tenantKYCVerificationView(v),
	})
}

// TenantKYCAadhaarOTPRequest handles POST /api/tenant/kyc/aadhaar/otp/request.
// Aadhaar OTP is intentionally disabled by PG Cashflow product policy. The selected
// low-friction flow is document OCR, UIDAI Secure QR where available, and manual review.
func (h *Handlers) TenantKYCAadhaarOTPRequest(c *gin.Context) {
	apierr.RespondClientErr(c, http.StatusGone, "Aadhaar OTP is not enabled. Use document upload or Secure QR instead.", apierr.Code("kyc.method_discontinued"))
}

// TenantKYCAadhaarOTPVerify handles POST /api/tenant/kyc/aadhaar/otp/verify.
// Aadhaar OTP is intentionally disabled by PG Cashflow product policy. The selected
// low-friction flow is document OCR, UIDAI Secure QR where available, and manual review.
func (h *Handlers) TenantKYCAadhaarOTPVerify(c *gin.Context) {
	apierr.RespondClientErr(c, http.StatusGone, "Aadhaar OTP is not enabled. Use document upload or Secure QR instead.", apierr.Code("kyc.method_discontinued"))
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
	OTP           *string `json:"otp"`
}

type requestGuardianConsentOTPBody struct {
	GuardianPhone string `json:"guardian_phone" binding:"required,e164|len=10"`
}

// OwnerRequestGuardianConsentOTP handles POST /api/owner/tenants/:id/kyc/guardian-consent/request-otp.
func (h *Handlers) OwnerRequestGuardianConsentOTP(c *gin.Context) {
	propertyID, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	tenantID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	tenant, err := h.TenantStore.GetByID(c.Request.Context(), tenantID)
	if err != nil || tenant.PropertyID != propertyID {
		apierr.RespondClientErr(c, http.StatusNotFound, "tenant not found", apierr.CodeRequestInvalidId)
		return
	}

	var body requestGuardianConsentOTPBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid guardian phone", apierr.CodeRequestInvalidBody)
		return
	}

	if h.Auth == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth service unavailable"})
		return
	}

	purpose := fmt.Sprintf("guardian_consent:%s", tenantID)
	channel, destination, err := h.Auth.RequestStepUpOTPWithChannel(c.Request.Context(), body.GuardianPhone, purpose, nil)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			respondErr(c, clientErrWithCode(http.StatusTooManyRequests, "please wait before requesting another code", apierr.CodeAuthRateLimited))
		} else if errors.Is(err, auth.ErrNoDeliveryChannel) {
			respondErr(c, clientErrWithCode(http.StatusUnprocessableEntity, "no delivery channel available", apierr.CodeAuthNoDeliveryChannel))
		} else {
			respondErr(c, err)
		}
		return
	}

	if channel == "sms" {
		destination = maskPhone(destination)
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "channel": channel, "destination": destination})
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

	var refID string
	if body.OTP != nil && strings.TrimSpace(*body.OTP) != "" {
		if h.Auth == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth service unavailable"})
			return
		}
		purpose := fmt.Sprintf("guardian_consent:%s", tenantID)
		if err := h.Auth.VerifyStepUpOTPSpecific(c.Request.Context(), body.GuardianPhone, strings.TrimSpace(*body.OTP), purpose, nil); err != nil {
			apierr.RespondClientErr(c, http.StatusUnauthorized, "invalid guardian verification code", apierr.CodeAuthInvalidOtp)
			return
		}
		refID = fmt.Sprintf("guard_otp_%s", uuid.New().String())
	} else {
		// KYC-07: Manual guardian consent requires mandatory evidence reference and cannot use synthetic placeholder
		if body.ReferenceID == nil || strings.TrimSpace(*body.ReferenceID) == "" {
			apierr.RespondClientErr(c, http.StatusBadRequest, "manual guardian consent requires a mandatory evidence reference_id", apierr.CodeRequestInvalidBody)
			return
		}
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
	slog.Info("audit: KYC identity photo accessed", "tenant_id", tenantID, "property_id", propertyID, "actor", c.GetString("user_id"))
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
	slog.Info("audit: KYC identity document accessed", "verification_id", id, "property_id", propertyID, "actor", c.GetString("user_id"))
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
	propertyID, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	verificationID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	actorRole := "owner"
	if claims.Role == domain.RoleManager {
		actorRole = "manager"
	}
	actor := fmt.Sprintf("%s:%s", actorRole, claims.UserID)
	v, err := svc.ApproveManualReview(c.Request.Context(), verificationID, propertyID, claims.UserID, actor)
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
	propertyID, ok := propertyIDFromClaims(c)
	if !ok {
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

	actorRole := "owner"
	if claims.Role == domain.RoleManager {
		actorRole = "manager"
	}
	actor := fmt.Sprintf("%s:%s", actorRole, claims.UserID)
	v, err := svc.RejectManualReview(c.Request.Context(), verificationID, propertyID, claims.UserID, body.Reason, actor)
	if err != nil {
		respondErr(c, kycClientErr(err))
		return
	}
	c.JSON(http.StatusOK, ownerKYCVerificationView(v))
}

// ---------------------------------------------------------------------------
// Projection helpers â€” enforce tenant/owner field visibility boundaries.
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
	if v.DocumentType != "" {
		m["document_type"] = v.DocumentType
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

// CashfreeKYCWebhook handles incoming POST /webhooks/cashfree/kyc callbacks.
// Enforces:
// 1. Raw body capture up to 1 MiB limit.
// 2. Signature verification: HMAC-SHA256(timestamp + rawBody, secret) with secret rotation.
// 3. Timestamp replay tolerance (default 300 seconds).
// 4. In-memory UID redaction before any processing.
// 5. Monotonic status transitions: verified and rejected records are immutable.
func (h *Handlers) CashfreeKYCWebhook(c *gin.Context) {
	const maxKYCWebhookBody = 1 << 20 // 1 MiB
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxKYCWebhookBody+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body read error"})
		return
	}
	if len(raw) > maxKYCWebhookBody {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body too large"})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	// Secrets hierarchy: KYC-specific secret first, fallback to general Cashfree secret
	primarySec := h.CashfreeKYCWebhookSecret
	if primarySec == "" {
		primarySec = h.CashfreeSecret
	}
	candidateSecrets := cashfree.OrderedCandidateSecrets(primarySec, h.CashfreeKYCWebhookSecrets)
	if len(candidateSecrets) == 0 && len(h.CashfreeSecrets) > 0 {
		candidateSecrets = cashfree.OrderedCandidateSecrets(primarySec, h.CashfreeSecrets)
	}
	if len(candidateSecrets) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "kyc webhook secret not configured"})
		return
	}

	sig := strings.TrimSpace(c.GetHeader("x-webhook-signature"))
	ts := strings.TrimSpace(c.GetHeader("x-webhook-timestamp"))
	if sig == "" || ts == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing signature or timestamp header"})
		return
	}

	validSig := false
	for _, sec := range candidateSecrets {
		if cashfree.VerifyWebhookHMAC(sec, ts, string(raw), sig) {
			validSig = true
			break
		}
	}
	if !validSig {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	// Webhook payload supporting both Smart OCR and DigiLocker callbacks.
	var payload struct {
		Event          string `json:"event"`
		EventType      string `json:"event_type"`
		Type           string `json:"type"`
		VerificationID string `json:"verification_id"`
		ReferenceID    any    `json:"reference_id"`
		Status         string `json:"status"`
		DocumentType   string `json:"document_type"`
		OCRData        struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"ocr_data"`
		FraudChecks struct {
			IsForged        bool `json:"is_forged"`
			IsScreenshot    bool `json:"is_screenshot"`
			IsPhotoOfScreen bool `json:"is_photo_of_screen"`
		} `json:"fraud_checks"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json payload"})
		return
	}

	eventHashBytes := sha256.Sum256([]byte(ts + ":" + string(raw)))
	eventHash := hex.EncodeToString(eventHashBytes[:])

	// Pre-drift Idempotency Suppression:
	// Acknowledge retried delivery of already-received KYC callbacks with 200 OK
	// before timestamp drift verification to prevent gateway retry storms.
	pool := h.getPool()
	if pool != nil {
		var existingID int64
		if err := pool.QueryRow(c.Request.Context(), `
			SELECT id FROM kyc_webhook_inbox WHERE provider = 'cashfree' AND event_hash = $1`, eventHash).Scan(&existingID); err == nil && existingID > 0 {
			c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "duplicate_event"})
			return
		}
	}

	// Verify timestamp tolerance with fail-closed default (300s)
	tolerance := h.WebhookToleranceSec
	if tolerance <= 0 {
		tolerance = 300
	}
	if err := cashfree.VerifyWebhookTimestamp(ts, tolerance, time.Now().UTC()); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp tolerance exceeded"})
		return
	}

	svc := h.KYCSvc
	if svc == nil {
		// Do not acknowledge a valid event that cannot be processed. A 5xx response
		// lets the provider's configured delivery policy retry after service recovery.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "kyc webhook processing is temporarily unavailable"})
		return
	}

	event := payload.Event
	if event == "" {
		event = payload.EventType
	}
	if event == "" {
		event = payload.Type
	}

	refIDStr := ""
	if payload.ReferenceID != nil {
		refIDStr = fmt.Sprintf("%v", payload.ReferenceID)
	}

	// Immediate in-memory redaction: purge full UID
	payload.OCRData.UID = ""
	for i := range raw {
		raw[i] = 0
	}

	var inboxID int64
	if pool != nil {
		ctx := c.Request.Context()
		operationalPayload := map[string]any{
			"verification_id": payload.VerificationID,
			"reference_id":    refIDStr,
			"event":           event,
			"status":          payload.Status,
			"fraud_checks":    payload.FraudChecks,
		}
		inboxPayloadBytes, _ := json.Marshal(operationalPayload)

		err := pool.QueryRow(ctx, `
			INSERT INTO kyc_webhook_inbox (
				provider, event_hash, event_timestamp, verification_id, reference_id,
				event_type, status, payload, processing_status, received_at
			) VALUES (
				'cashfree', $1, $2, $3, $4, $5, $6, $7, 'received', NOW()
			)
			ON CONFLICT (provider, event_hash) DO NOTHING
			RETURNING id`,
			eventHash, ts, nullIfEmptyStr(payload.VerificationID), nullIfEmptyStr(refIDStr),
			nullIfEmptyStr(event), nullIfEmptyStr(payload.Status), inboxPayloadBytes,
		).Scan(&inboxID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "kyc webhook: failed to persist to inbox", "err", err, "hash", eventHash)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record webhook"})
			return
		}
		if inboxID == 0 {
			var prevStatus string
			_ = pool.QueryRow(ctx, `SELECT processing_status FROM kyc_webhook_inbox WHERE provider = 'cashfree' AND event_hash = $1`, eventHash).Scan(&prevStatus)
			if prevStatus == "processed" || prevStatus == "duplicate" {
				c.JSON(http.StatusOK, gin.H{"status": "already_processed"})
				return
			}
		}

		// Asynchronous durable worker pattern (KYC-OWN-02):
		// The HTTP handler strictly verifies HMAC & timestamp and records to inbox.
		// The background KYCInboxWorker is the single owner of callback execution,
		// preventing lock inversion, race conditions, and timeout cascades.
		c.JSON(http.StatusOK, gin.H{
			"status": "received",
			"hash":   eventHash,
		})
		return
	}

	isDigiLocker := strings.HasPrefix(payload.VerificationID, "cf_dgl_") ||
		strings.HasPrefix(refIDStr, "cf_dgl_") ||
		strings.HasPrefix(event, "DIGILOCKER_")

	if isDigiLocker {
		if !h.FeatureKYCDigiLocker {
			slog.InfoContext(c.Request.Context(), "kyc webhook: digilocker disabled; acknowledging without processing",
				"verification_id", payload.VerificationID,
				"event", event,
			)
			if pool != nil && inboxID > 0 {
				_, _ = pool.Exec(c.Request.Context(), `UPDATE kyc_webhook_inbox SET processing_status = 'processed', error_message = 'digilocker disabled', processed_at = NOW() WHERE id = $1`, inboxID)
			}
			c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "digilocker disabled"})
			return
		}
		// Demultiplex: DigiLocker Verification Callback.
		// Webhook is treated as an event notification/hint: ProcessDigiLockerCallback
		// validates state and retrieves attested credentials directly from Cashfree's secure API.
		v, err := svc.ProcessDigiLockerCallback(c.Request.Context(), payload.VerificationID, refIDStr, event, payload.Status, "cashfree:webhook")
		if err != nil {
			if pool != nil && inboxID > 0 {
				_, _ = pool.Exec(c.Request.Context(), `UPDATE kyc_webhook_inbox SET processing_status = 'failed', error_message = $1 WHERE id = $2`, err.Error(), inboxID)
			}
			if errors.Is(err, domain.ErrVerificationNotFound) {
				c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": err.Error()})
				return
			}
			slog.ErrorContext(c.Request.Context(), "kyc webhook: digilocker callback processing returned error",
				"err", err,
				"verification_id", payload.VerificationID,
				"ref_id", refIDStr,
				"event", event,
			)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process digilocker callback"})
			return
		}

		if pool != nil && inboxID > 0 {
			_, _ = pool.Exec(c.Request.Context(), `UPDATE kyc_webhook_inbox SET processing_status = 'processed', processed_at = NOW() WHERE id = $1`, inboxID)
		}

		c.JSON(http.StatusOK, gin.H{
			"status":          "received",
			"verification_id": v.ID,
			"state":           v.Status,
		})
		return
	}

	vendorRef := fmt.Sprintf("cf_ocr_%v", payload.ReferenceID)
	v, err := svc.GetVerificationByVendorRefID(c.Request.Context(), vendorRef)
	if err != nil || v == nil {
		if pool != nil && inboxID > 0 {
			_, _ = pool.Exec(c.Request.Context(), `UPDATE kyc_webhook_inbox SET processing_status = 'processed', processed_at = NOW() WHERE id = $1`, inboxID)
		}
		// Callback for unknown or already cleaned record
		c.JSON(http.StatusOK, gin.H{"status": "ignored", "reason": "verification not found"})
		return
	}

	// Replay and Monotonicity Guard: Terminal states (verified, rejected, revoked, expired) are immutable.
	if v.Status == domain.KYCStatusVerified ||
		v.Status == domain.KYCStatusRejected ||
		v.Status == domain.KYCStatusRevoked ||
		v.Status == domain.KYCStatusExpired {
		if pool != nil && inboxID > 0 {
			_, _ = pool.Exec(c.Request.Context(), `UPDATE kyc_webhook_inbox SET processing_status = 'processed', processed_at = NOW() WHERE id = $1`, inboxID)
		}
		c.JSON(http.StatusOK, gin.H{
			"status":         "already_processed",
			"current_status": v.Status,
		})
		return
	}

	// Record fraud flags in audit trail if detected by webhook
	if payload.FraudChecks.IsForged || payload.FraudChecks.IsScreenshot || payload.FraudChecks.IsPhotoOfScreen {
		slog.WarnContext(c.Request.Context(), "kyc webhook: document fraud detected in callback",
			"verification_id", v.ID,
			"reference_id", payload.ReferenceID,
		)
	}

	if pool != nil && inboxID > 0 {
		_, _ = pool.Exec(c.Request.Context(), `UPDATE kyc_webhook_inbox SET processing_status = 'processed', processed_at = NOW() WHERE id = $1`, inboxID)
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "received",
		"ref_id": payload.ReferenceID,
		"state":  v.Status,
	})
}
