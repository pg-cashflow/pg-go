package api

import (
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
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
    .row{display:flex;gap:.5rem;flex-wrap:wrap;margin:.75rem 0}
    button,a.btn{appearance:none;border:0;background:#1a1a1a;color:#fff;padding:.6rem .9rem;border-radius:8px;font-size:.9rem;text-decoration:none;display:inline-block}
    button.secondary{background:#fff;color:#1a1a1a;border:1px solid #ccc}
    .done{background:#e8f5e9;border:1px solid #c8e6c9;padding:.75rem;border-radius:6px}
  </style>
</head>
<body>
<main>
  <h1>{{.PropertyName}}</h1>
  <p class="meta">Pay {{.OwnerName}}{{if .RoomNumber}} · Room {{.RoomNumber}}{{end}}</p>
  <p class="amt">₹{{printf "%.2f" .AmountRupees}}</p>
  <p class="meta">Due code: {{.DueCode}} · expires {{.ExpiresAt}}</p>
  {{if .Payable}}
  {{if eq .Mode "cashfree"}}
  <p class="meta">UPI checkout — payment is confirmed automatically. Do not use a personal UPI QR.</p>
  <div class="row"><button type="button" id="cfpay">Pay with UPI</button></div>
  <script src="https://sdk.cashfree.com/js/v3/cashfree.js"></script>
  <script>
  (function(){
    var btn=document.getElementById('cfpay');
    if(!btn) return;
    btn.addEventListener('click', function(){
      var cashfree=Cashfree({mode: {{printf "%q" .CashfreeEnv}}});
      cashfree.checkout({paymentSessionId: {{printf "%q" .PaymentSessionID}}, redirectTarget:'_self'});
    });
  })();
  </script>
  {{else}}
  <div class="warn"><strong>Important:</strong> When you scan this QR, your UPI app may let you edit the amount.
    Please pay the exact amount shown above so we can match your payment automatically.</div>
  {{if .QRDataURI}}<img id="qr" src="{{.QRDataURI}}" alt="UPI QR">{{end}}
  <div class="row">
    <a class="btn" href="{{.UPILink}}">Open in UPI app</a>
    <a class="btn" id="save" download="rent-{{.DueCode}}.png">Save QR</a>
    <button type="button" class="secondary" data-copy="{{.VPA}}">Copy UPI ID</button>
    <button type="button" class="secondary" data-copy="{{.Note}}">Copy note</button>
  </div>
  <p class="meta">UPI ID: {{.VPA}} · Note: {{.Note}}</p>
  {{end}}
  {{else}}
  <p class="done">This due is already paid or waived. Do not reuse an old QR.</p>
  {{end}}
</main>
<script>
(function(){
  var img=document.getElementById('qr');
  var save=document.getElementById('save');
  if(img&&save) save.href=img.src;
  document.querySelectorAll('[data-copy]').forEach(function(btn){
    btn.addEventListener('click', function(){
      var t=btn.getAttribute('data-copy')||'';
      if(navigator.clipboard) navigator.clipboard.writeText(t);
    });
  });
})();
</script>
</body>
</html>`))

type paymentPageData struct {
	PropertyName     string
	OwnerName        string
	RoomNumber       *string
	AmountRupees     float64
	DueCode          string
	ExpiresAt        string
	UPILink          string
	QRDataURI        template.URL
	VPA              string
	Note             string
	Payable          bool
	Mode             string
	PaymentSessionID string
	CashfreeEnv      string
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
	payable := view.Due.Status == domain.DueStatusPending || view.Due.Status == domain.DueStatusPartial
	data := paymentPageData{
		PropertyName: view.PropertyName,
		OwnerName:    view.OwnerName,
		RoomNumber:   view.RoomNumber,
		AmountRupees: float64(view.AmountPaise) / 100.0,
		DueCode:      view.Due.DueCode,
		ExpiresAt:    view.ExpiresAt.Format("02 Jan 2006 15:04 MST"),
		Note:         domain.UPINote(view.Due.DueCode),
		Payable:      payable,
		Mode:         domain.PaymentModeManual,
		CashfreeEnv:  h.CashfreeEnv,
	}
	if strings.EqualFold(data.CashfreeEnv, "production") {
		data.CashfreeEnv = "production"
	} else {
		data.CashfreeEnv = "sandbox"
	}

	if payable && h.Collector != nil && h.PropertyStore != nil {
		if prop, err := h.PropertyStore.GetByID(c.Request.Context(), view.Due.PropertyID); err == nil {
			intent, png, err := h.Collector.PayIntent(c.Request.Context(), &view.Due, prop, room, "", view.TenantPhone)
			if err == nil && intent != nil {
				data.Mode = intent.Mode
				data.PaymentSessionID = intent.PaymentSessionID
				data.VPA = intent.VPA
				data.UPILink = intent.UPILink
				if len(png) > 0 {
					data.QRDataURI = template.URL("data:image/png;base64," + b64(png))
				}
			}
		}
	}
	if data.Mode != domain.PaymentModeCashfree && payable && data.UPILink == "" {
		data.UPILink = qr.GenerateUPILink(view.UPIVPA, view.OwnerName, int64(view.AmountPaise), view.Due.DueCode, room)
		data.VPA = view.UPIVPA
		if png, err := qr.GenerateQR(data.UPILink); err == nil {
			data.QRDataURI = template.URL("data:image/png;base64," + b64(png))
		}
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
		respondErr(c, clientErr(status, err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "user": user})
}

type firebaseAuthBody struct {
	IDToken    string `json:"id_token" binding:"required"`
	InviteCode string `json:"invite_code"`
}

// FirebaseAuth handles POST /auth/firebase — exchange Firebase ID token for app JWT.
func (h *Handlers) FirebaseAuth(c *gin.Context) {
	var body firebaseAuthBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id_token required"})
		return
	}
	token, user, err := h.Auth.VerifyFirebaseAndIssueToken(c.Request.Context(), body.IDToken, body.InviteCode)
	if err != nil {
		status := http.StatusUnauthorized
		msg := "authentication failed"
		switch {
		case errors.Is(err, auth.ErrFirebaseNotConfigured):
			status = http.StatusServiceUnavailable
			msg = "firebase auth not configured"
		case errors.Is(err, auth.ErrEmailNotVerified):
			status = http.StatusForbidden
			msg = "email is not verified with Google — please verify your email or use phone OTP"
		case errors.Is(err, auth.ErrNoAccount):
			status = http.StatusNotFound
			msg = "account not found — if you are an owner, verify your registered phone/email; if you are a tenant, get the invite code from your owner"
		case errors.Is(err, auth.ErrInvalidInvite):
			status = http.StatusNotFound
			msg = "get the PG invite code from your owner"
		case errors.Is(err, auth.ErrTenantVacated):
			status = http.StatusForbidden
			msg = "access revoked"
		}
		respondErr(c, clientErr(status, msg))
		return
	}
	if user != nil && user.Role == domain.RoleTenant && user.TenantID == nil && user.PropertyID != nil && h.Joins != nil {
		if _, err := h.Joins.EnsurePending(c.Request.Context(), user, *user.PropertyID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "join queue failed"})
			return
		}
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
