package api

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/localization"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/push"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

const (
	RefreshCookieName = "pg_refresh_token"
	RefreshCookiePath = "/api/auth"
)

func (h *Handlers) isProduction() bool {
	return strings.EqualFold(h.AppEnv, "production")
}

// refreshCookieSecure reports whether the refresh cookie must carry the Secure flag.
// Production always sets it. Otherwise it is set whenever the request arrived over HTTPS,
// directly or through a reverse proxy. Setting Secure can only restrict the cookie, so a
// spoofed X-Forwarded-Proto cannot weaken it.
func (h *Handlers) refreshCookieSecure(c *gin.Context) bool {
	if h.isProduction() || c.Request.TLS != nil {
		return true
	}
	return strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}

func (h *Handlers) setRefreshCookie(c *gin.Context, token string, expiresAt time.Time) {
	c.SetSameSite(http.SameSiteStrictMode)
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = -1
	}
	c.SetCookie(
		RefreshCookieName,
		token,
		maxAge,
		RefreshCookiePath,
		"", // host-only
		h.refreshCookieSecure(c),
		true, // HttpOnly
	)
}

func (h *Handlers) clearRefreshCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteStrictMode)
	// Attributes must match the set call, or browsers keep the original cookie.
	c.SetCookie(RefreshCookieName, "", -1, RefreshCookiePath, "", h.refreshCookieSecure(c), true)
}

var paymentPageTmpl = template.Must(template.New("pay").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Pay rent — {{.PropertyName}}</title>
  <meta property="og:type" content="website">
  <meta property="og:title" content="Pay ₹{{.AmountRupees}} — {{.PropertyName}}{{if .RoomNumber}} · Room {{.RoomNumber}}{{end}}">
  <meta property="og:description" content="Rent payment due for {{.PropertyName}}.">
  <style>
    body{font-family:system-ui,-apple-system,sans-serif;margin:0;padding:1.5rem;background:#f7f7f5;color:#1a1a1a}
    main{max-width:28rem;margin:2rem auto;background:#fff;padding:2rem;border-radius:14px;box-shadow:0 1px 3px rgba(0,0,0,0.05)}
    h1{font-size:1.4rem;margin:0 0 .5rem}
    .amt{font-size:2.25rem;font-weight:700;margin:1rem 0;color:#0f172a}
    .meta{color:#64748b;font-size:.9rem;margin:.25rem 0}
    .info{background:#f8fafc;border:1px solid #e2e8f0;padding:1rem;border-radius:10px;margin:1.25rem 0;font-size:.875rem;line-height:1.5;color:#334155}
    .row{display:flex;gap:.75rem;flex-wrap:wrap;margin:1.5rem 0 0}
    a.btn{appearance:none;border:0;background:#0f172a;color:#fff;padding:.75rem 1.25rem;border-radius:10px;font-size:1rem;font-weight:600;text-decoration:none;display:block;width:100%;text-align:center;box-sizing:border-box}
    a.btn:hover{background:#1e293b}
    .done{background:#f0fdf4;border:1px solid #bbf7d0;color:#166534;padding:1rem;border-radius:10px;font-size:.9rem}
  </style>
</head>
<body>
<main>
  <h1>{{.PropertyName}}</h1>
  <p class="meta">Pay {{.OwnerName}}{{if .RoomNumber}} · Room {{.RoomNumber}}{{end}}</p>
  <p class="amt">₹{{.AmountRupees}}</p>
  <p class="meta">Due code: {{.DueCode}} · expires {{.ExpiresAt}}</p>
  {{if .Payable}}
  <div class="info">
    <strong>Mandatory KYC Policy:</strong> In compliance with RBI regulations and platform security standards, tenant identity verification is required before making payments. Please log in to complete verification and pay securely via UPI, Card, or Netbanking.
  </div>
  <div class="row">
    <a class="btn" href="/tenant/dues">Log in to Pay Due</a>
  </div>
  {{else}}
  <p class="done">This due is already paid or waived.</p>
  {{end}}
</main>
</body>
</html>`))

type paymentPageData struct {
	PropertyName string
	OwnerName    string
	RoomNumber   *string
	AmountRupees string
	DueCode      string
	ExpiresAt    string
	Payable      bool
}

// PaymentPage serves GET /p/:token.
// It displays due metadata and directs the tenant to the authenticated app for mandatory KYC and payment.
// Crucially, it does not create a Cashfree order on GET (protecting against WhatsApp link crawlers)
// and does not expose unauthenticated personal UPI QR codes.
func (h *Handlers) PaymentPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("Referrer-Policy", "no-referrer")

	view, err := h.MagicLink.ResolveToken(c.Request.Context(), c.Param("token"))
	if err != nil {
		status := http.StatusNotFound
		switch {
		case errors.Is(err, magiclink.ErrTokenExpired):
			status = http.StatusGone
		case errors.Is(err, magiclink.ErrTokenUsed):
			status = http.StatusGone
		}
		c.String(status, "This payment link is invalid or expired.")
		return
	}

	payable := view.Due.Status == domain.DueStatusPending || view.Due.Status == domain.DueStatusPartial
	accept := c.GetHeader("Accept")
	if strings.Contains(accept, "application/json") {
		c.JSON(http.StatusOK, gin.H{
			"property_name": view.PropertyName,
			"owner_name":    view.OwnerName,
			"room_number":   view.RoomNumber,
			"amount_paise":  view.AmountPaise,
			"due_code":      view.Due.DueCode,
			"payable":       payable,
			"redirect_url":  "/tenant/dues",
		})
		return
	}

	data := paymentPageData{
		PropertyName: view.PropertyName,
		OwnerName:    view.OwnerName,
		RoomNumber:   view.RoomNumber,
		AmountRupees: cashfree.FormatPaiseToRupees(view.AmountPaise),
		DueCode:      view.Due.DueCode,
		ExpiresAt:    view.ExpiresAt.Format("02 Jan 2006 15:04 MST"),
		Payable:      payable,
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = paymentPageTmpl.Execute(c.Writer, data)
}

type pushSubBody struct {
	Endpoint string `json:"endpoint" binding:"required"`
	Keys     struct {
		P256dh string `json:"p256dh" binding:"required"`
		Auth   string `json:"auth" binding:"required"`
	} `json:"keys" binding:"required"`
}

// PaymentPushSubscribe registers push from the magic-link page.
func (h *Handlers) PaymentPushSubscribe(c *gin.Context) {
	view, err := h.MagicLink.ResolveToken(c.Request.Context(), c.Param("token"))
	if err != nil {
		apierr.RespondClientErr(c, http.StatusNotFound, "invalid token", apierr.CodeAuthInvalidToken)
		return
	}
	var body pushSubBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid body", apierr.CodeRequestInvalidBody)
		return
	}
	if err := h.Push.Subscribe(c.Request.Context(), view.Due.TenantID, body.Endpoint, body.Keys.P256dh, body.Keys.Auth); err != nil {
		if errors.Is(err, push.ErrInvalidPushEndpoint) {
			apierr.RespondClientErr(c, http.StatusBadRequest, "invalid push endpoint", apierr.CodePushEndpointInvalid)
			return
		}
		if errors.Is(err, postgres.ErrPushEndpointOwned) {
			apierr.RespondClientErr(c, http.StatusConflict, "push endpoint registered to another tenant", apierr.CodePushEndpointConflict)
			return
		}
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "subscribe failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// otpEndpointDisabledMsg is returned by the deprecated custom phone-OTP login
// routes. Per docs/specs/auth-frontend-backend-contract.md §1.1, Firebase
// Phone Auth is the single front door; this path is a Gate C.4 deprecation,
// not a 404 — 410 Gone signals "this existed and is intentionally retired"
// to any client/integration still pointed at it.
const otpEndpointDisabledMsg = "custom phone OTP login is disabled; use Firebase authentication"

// OTPRequest handled POST /auth/otp/request. Disabled per ticket C.4: the
// legacy path silently downgraded phone-login verification to email whenever
// no SMS gateway was configured (always true — see internal/app/channels.go),
// bypassing Firebase's device attestation and fraud checks entirely. It no
// longer calls into auth.Service at all, so re-wiring it back on requires a
// deliberate code change, not a config flip.
func (h *Handlers) OTPRequest(c *gin.Context) {
	apierr.RespondClientErr(c, http.StatusGone, otpEndpointDisabledMsg, apierr.CodeAuthEndpointDisabled)
}

// OTPVerify handled POST /auth/otp/verify. Disabled alongside OTPRequest —
// see ticket C.4 and the comment on OTPRequest.
func (h *Handlers) OTPVerify(c *gin.Context) {
	apierr.RespondClientErr(c, http.StatusGone, otpEndpointDisabledMsg, apierr.CodeAuthEndpointDisabled)
}

// RevokeSessions handles POST /auth/revoke-sessions.
// Revokes all active sessions for the user EXCEPT the caller's current session.
// Note: We deliberately do not bump users.token_version here, as that would invalidate
// the caller's own access token immediately. Global token_version bumps are reserved
// for administrative account freezes / deactivations.
func (h *Handlers) RevokeSessions(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	if h.Auth == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "internal error"})
		return
	}
	cookie, _ := c.Cookie(RefreshCookieName)
	if err := h.Auth.RevokeOtherSessions(c.Request.Context(), claims.UserID, claims.FamilyID, cookie); err != nil {
		if errors.Is(err, auth.ErrNoCurrentSession) {
			apierr.RespondClientErr(c, http.StatusBadRequest, "cannot identify current session", apierr.CodeRequestInvalidBody)
			return
		}
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type firebaseAuthBody struct {
	IDToken    string `json:"id_token" binding:"required"`
	InviteCode string `json:"invite_code"`
}

// FirebaseAuth handles POST /auth/firebase — exchange Firebase ID token for app JWT.
func (h *Handlers) FirebaseAuth(c *gin.Context) {
	var body firebaseAuthBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "id_token required", apierr.CodeRequestInvalidBody)
		return
	}
	_, user, err := h.Auth.VerifyFirebaseAndIssueToken(c.Request.Context(), body.IDToken, body.InviteCode)
	if err != nil {
		status := http.StatusUnauthorized
		msg := "authentication failed"
		code := apierr.CodeAuthInvalidFirebaseToken
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			status = http.StatusTooManyRequests
			msg = "too many requests"
			code = apierr.CodeAuthRateLimited
		case errors.Is(err, auth.ErrFirebaseNotConfigured):
			status = http.StatusServiceUnavailable
			msg = "firebase auth not configured"
			code = apierr.CodeAuthFirebaseNotConfigured
		case errors.Is(err, auth.ErrEmailNotVerified):
			status = http.StatusForbidden
			msg = "email is not verified with Google — please verify your email or use phone OTP"
			code = apierr.CodeAuthEmailNotVerified
		case errors.Is(err, auth.ErrNoAccount):
			status = http.StatusNotFound
			msg = "account not found — if you are an owner, verify your registered phone/email; if you are a tenant, get the invite code from your owner"
			code = apierr.CodeAuthNoAccount
		case errors.Is(err, auth.ErrInvalidInvite):
			status = http.StatusNotFound
			msg = "get the PG invite code from your owner"
			code = apierr.CodeAuthInvalidInvite
		case errors.Is(err, auth.ErrTenantVacated):
			status = http.StatusForbidden
			msg = "access revoked"
			code = apierr.CodeAuthAccessRevoked
		case errors.Is(err, auth.ErrAccountReLinkDenied):
			status = http.StatusForbidden
			msg = "account re-link denied: owner or manager requires verified Google email match"
			code = apierr.CodeAuthForbidden
		}
		respondErr(c, clientErrWithCode(status, msg, code))
		return
	}
	if user != nil && user.Phone != "" && !defaultFirebasePhoneLimiter.allow(user.Phone) {
		respondErr(c, clientErrWithCode(http.StatusTooManyRequests, "too many requests", apierr.CodeAuthRateLimited))
		return
	}
	if user != nil && user.Role == domain.RoleTenant && user.TenantID == nil && user.PropertyID != nil && h.Joins != nil {
		var slotInviteID *uuid.UUID
		if h.InviteResolver != nil && body.InviteCode != "" {
			if res, err := h.InviteResolver.Resolve(c.Request.Context(), body.InviteCode); err == nil {
				if res.SlotInvite != nil && res.SlotInvite.BoundPhone != "" &&
					auth.NormalizePhone(user.Phone) != auth.NormalizePhone(res.SlotInvite.BoundPhone) {
					respondErr(c, clientErrWithCode(http.StatusNotFound, "get the PG invite code from your owner", apierr.CodeAuthInvalidInvite))
					return
				}
				slotInviteID = res.SlotInviteID
			}
		}
		if _, err := h.Joins.EnsurePending(c.Request.Context(), user, *user.PropertyID, slotInviteID); err != nil {
			slog.Error("FirebaseAuth: EnsurePending failed", "err", err, "user_id", user.ID)
			c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "join queue failed"})
			return
		}
	}
	h.attachUserLocale(c.Request.Context(), c.Request, user)
	accToken, refToken, err := h.Auth.IssueSessionWithClient(c.Request.Context(), user, c.Request.UserAgent(), c.ClientIP())
	if err != nil || refToken == "" {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{
			Error: "session creation failed",
		})
		return
	}
	h.setRefreshCookie(c, refToken, time.Now().Add(auth.RefreshTokenTTL))
	c.JSON(http.StatusOK, gin.H{"token": accToken, "user": user})
}

// AuthRefresh handles POST /auth/refresh.
// Reads the HttpOnly refresh token cookie, rotates the token in its family, and issues a fresh 15-minute access token.
func (h *Handlers) AuthRefresh(c *gin.Context) {
	cookie, err := c.Cookie(RefreshCookieName)
	if err != nil || strings.TrimSpace(cookie) == "" {
		c.JSON(http.StatusUnauthorized, apierr.ErrorEnvelope{
			Error: "missing refresh token",
			Code:  apierr.CodeAuthUnauthorized,
		})
		return
	}

	newAccessToken, newRefreshToken, user, err := h.Auth.RotateRefreshTokenWithClient(c.Request.Context(), cookie, c.Request.UserAgent(), c.ClientIP())
	if err != nil {
		// Only clear the session cookie for definitive, permanent auth failures.
		// Transient errors (DB timeouts, network blips) must not destroy the session.
		isDefinitiveFailure := errors.Is(err, auth.ErrInvalidToken) ||
			errors.Is(err, auth.ErrRefreshTokenExpired) ||
			errors.Is(err, auth.ErrReplayDetected) ||
			errors.Is(err, auth.ErrTenantVacated)

		if isDefinitiveFailure {
			h.clearRefreshCookie(c)
		}

		code := apierr.CodeAuthUnauthorized
		msg := "invalid or expired refresh token"
		status := http.StatusUnauthorized

		switch {
		case errors.Is(err, auth.ErrReplayDetected):
			code = apierr.CodeAuthAccessRevoked
			msg = "refresh token replay detected: session family revoked"
		case errors.Is(err, auth.ErrTenantVacated):
			code = apierr.CodeAuthAccessRevoked
			msg = "tenant vacated"
			status = http.StatusForbidden
		case !isDefinitiveFailure:
			// Transient server-side failure: client must retry, session is intact.
			c.JSON(http.StatusServiceUnavailable, apierr.ErrorEnvelope{
				Error: "refresh temporarily unavailable, please retry",
				Code:  apierr.CodeAuthUnauthorized,
			})
			return
		}

		c.JSON(status, apierr.ErrorEnvelope{
			Error: msg,
			Code:  code,
		})
		return
	}

	// Grace-window path: service returned an empty refresh token to signal that
	// the browser's current cookie is already the valid successor token.
	// Issue only a new access token; do not rotate the cookie.
	if newRefreshToken != "" {
		h.setRefreshCookie(c, newRefreshToken, time.Now().Add(auth.RefreshTokenTTL))
	}
	h.attachUserLocale(c.Request.Context(), c.Request, user)
	c.JSON(http.StatusOK, gin.H{"token": newAccessToken, "user": user})
}

// AuthLogout handles POST /auth/logout.
// Revokes the presented refresh token family and clears the cookie.
func (h *Handlers) AuthLogout(c *gin.Context) {
	var revokeErr error
	if cookie, err := c.Cookie(RefreshCookieName); err == nil && strings.TrimSpace(cookie) != "" {
		if err := h.Auth.RevokeSession(c.Request.Context(), cookie); err != nil {
			revokeErr = err
			// Log the failure so it appears in audit trails and on-call alerts.
			slog.Error("AuthLogout: failed to revoke session in DB",
				"error", revokeErr,
				"path", c.Request.URL.Path,
			)
		}
	}
	h.clearRefreshCookie(c)
	if revokeErr != nil {
		c.JSON(http.StatusAccepted, gin.H{
			"ok":      false,
			"warning": "local session cleared; remote session revocation failed",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// GetActiveSessions handles GET /auth/sessions.
// Returns list of active device sessions for the authenticated user.
func (h *Handlers) GetActiveSessions(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	if h.Auth == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "internal error"})
		return
	}
	cookie, _ := c.Cookie(RefreshCookieName)
	sessions, err := h.Auth.ListSessions(c.Request.Context(), claims.UserID, claims.FamilyID, cookie)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": sessions})
}

// DeleteSession handles DELETE /auth/sessions/:id.
// Revokes an individual active device session belonging to the authenticated user.
func (h *Handlers) DeleteSession(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	sessionID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		apierr.RespondBindErr(c, "invalid session id", apierr.CodeRequestInvalidBody)
		return
	}
	if h.Auth == nil {
		c.JSON(http.StatusInternalServerError, apierr.ErrorEnvelope{Error: "internal error"})
		return
	}
	if err := h.Auth.RevokeDeviceSession(c.Request.Context(), claims.UserID, sessionID); err != nil {
		respondErr(c, err)
		return
	}
	// If the user revoked their currently active session, clear the refresh cookie
	if claims.FamilyID != nil && *claims.FamilyID == sessionID {
		h.clearRefreshCookie(c)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) attachUserLocale(ctx context.Context, r *http.Request, user *domain.User) {
	if user == nil {
		return
	}
	if h.PreferencesStore != nil {
		if pref, err := h.PreferencesStore.GetByUserID(ctx, user.ID); err == nil && pref != nil {
			user.Locale = pref.Locale
			user.HasSavedPreference = true
			user.Theme = pref.Theme
			user.HasSavedTheme = pref.Theme != nil
			return
		}
	}
	if loc := localization.FromContext(ctx); loc != localization.DefaultLocale {
		user.Locale = loc
	} else {
		user.Locale = localization.ResolveLocale(nil, r)
	}
	user.HasSavedPreference = false
	user.Theme = nil
	user.HasSavedTheme = false
}

func propertyIDFromClaims(c *gin.Context) (uuid.UUID, bool) {
	if pid, hasScope := requestscope.PropertyIDFromContext(c.Request.Context()); hasScope && pid != uuid.Nil {
		return pid, true
	}
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.PropertyID == nil {
		apierr.RespondClientErr(c, http.StatusForbidden, "no property scope", apierr.CodeAuthNoPropertyScope)
		return uuid.Nil, false
	}
	c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), *claims.PropertyID))
	return *claims.PropertyID, true
}

func userIDFromClaims(c *gin.Context) (uuid.UUID, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return uuid.Nil, false
	}
	return claims.UserID, true
}
