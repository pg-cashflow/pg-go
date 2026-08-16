package api

import (
	"errors"
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/qr"
)

var paymentPageTmpl = template.Must(template.New("pay").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Pay rent — {{.PropertyName}}</title>
  <style>
    body{font-family:system-ui,sans-serif;margin:0;padding:1.5rem;background:#f7f7f5;color:#1a1a1a}
    main{max-width:28rem;margin:0 auto}
    h1{font-size:1.25rem;margin:0 0 .5rem}
    .amt{font-size:2rem;font-weight:700;margin:.75rem 0}
    .warn{background:#fff3cd;border:1px solid #ffecb5;padding:.75rem;border-radius:6px;margin:1rem 0;font-size:.9rem}
    img{display:block;width:256px;height:256px;margin:1rem auto;background:#fff;padding:.5rem;border-radius:8px}
    .meta{color:#555;font-size:.9rem}
  </style>
</head>
<body>
<main>
  <h1>{{.PropertyName}}</h1>
  <p class="meta">Pay {{.OwnerName}}{{if .RoomNumber}} · Room {{.RoomNumber}}{{end}}</p>
  <p class="amt">₹{{printf "%.2f" .AmountRupees}}</p>
  <p class="meta">Due code: {{.DueCode}} · expires {{.ExpiresAt}}</p>
  <div class="warn"><strong>Important:</strong> When you scan this QR, your UPI app may let you edit the amount.
    Please pay the exact amount shown above so we can match your payment automatically.</div>
  {{if .QRDataURI}}<img src="{{.QRDataURI}}" alt="UPI QR">{{end}}
  <p class="meta"><a href="{{.UPILink}}">Open in UPI app</a></p>
</main>
</body>
</html>`))

type paymentPageData struct {
	PropertyName string
	OwnerName    string
	RoomNumber   *string
	AmountRupees float64
	DueCode      string
	ExpiresAt    string
	UPILink      string
	QRDataURI    template.URL
}

// PaymentPage serves GET /p/:token HTML.
func (h *Handlers) PaymentPage(c *gin.Context) {
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

	room := ""
	if view.RoomNumber != nil {
		room = *view.RoomNumber
	}
	upi := qr.GenerateUPILink(view.UPIVPA, view.OwnerName, int64(view.AmountPaise), view.Due.DueCode, room)
	png, err := qr.GenerateQR(upi)
	qrURI := template.URL("")
	if err == nil {
		qrURI = template.URL("data:image/png;base64," + b64(png))
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = paymentPageTmpl.Execute(c.Writer, paymentPageData{
		PropertyName: view.PropertyName,
		OwnerName:    view.OwnerName,
		RoomNumber:   view.RoomNumber,
		AmountRupees: float64(view.AmountPaise) / 100.0,
		DueCode:      view.Due.DueCode,
		ExpiresAt:    view.ExpiresAt.Format("02 Jan 2006 15:04 MST"),
		UPILink:      upi,
		QRDataURI:    qrURI,
	})
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
		c.JSON(http.StatusNotFound, gin.H{"error": "invalid token"})
		return
	}
	var body pushSubBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := h.Push.Subscribe(c.Request.Context(), view.Due.TenantID, body.Endpoint, body.Keys.P256dh, body.Keys.Auth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "subscribe failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type otpRequestBody struct {
	Phone string `json:"phone" binding:"required"`
}

// OTPRequest handles POST /auth/otp/request.
// Login OTP is retired; the route is no longer registered. Kept for rollback.
func (h *Handlers) OTPRequest(c *gin.Context) {
	var body otpRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}
	if err := h.Auth.RequestOTP(c.Request.Context(), body.Phone); err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limited"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "otp request failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type otpVerifyBody struct {
	Phone string `json:"phone" binding:"required"`
	OTP   string `json:"otp" binding:"required"`
}

// OTPVerify handles POST /auth/otp/verify.
func (h *Handlers) OTPVerify(c *gin.Context) {
	var body otpVerifyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone and otp required"})
		return
	}
	token, user, err := h.Auth.VerifyOTPAndIssueToken(c.Request.Context(), body.Phone, body.OTP)
	if err != nil {
		status := http.StatusUnauthorized
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			status = http.StatusTooManyRequests
		case errors.Is(err, auth.ErrNoAccount):
			status = http.StatusNotFound
		case errors.Is(err, auth.ErrTenantVacated):
			status = http.StatusForbidden
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "user": user})
}

type firebaseAuthBody struct {
	IDToken string `json:"id_token" binding:"required"`
}

// FirebaseAuth handles POST /auth/firebase — exchange Firebase ID token for app JWT.
func (h *Handlers) FirebaseAuth(c *gin.Context) {
	var body firebaseAuthBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id_token required"})
		return
	}
	token, user, err := h.Auth.VerifyFirebaseAndIssueToken(c.Request.Context(), body.IDToken)
	if err != nil {
		status := http.StatusUnauthorized
		switch {
		case errors.Is(err, auth.ErrFirebaseNotConfigured):
			status = http.StatusServiceUnavailable
		case errors.Is(err, auth.ErrNoAccount):
			status = http.StatusNotFound
		case errors.Is(err, auth.ErrTenantVacated):
			status = http.StatusForbidden
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "user": user})
}

func propertyIDFromClaims(c *gin.Context) (uuid.UUID, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.PropertyID == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "no property scope"})
		return uuid.Nil, false
	}
	return *claims.PropertyID, true
}

func userIDFromClaims(c *gin.Context) (uuid.UUID, bool) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return uuid.Nil, false
	}
	return claims.UserID, true
}
