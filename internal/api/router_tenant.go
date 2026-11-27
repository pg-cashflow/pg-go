package api

import (
	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// registerTenantRoutes wires tenant routes in two explicit groups.
//
// Exempt (no KYC onboarding gate): the profile, the identity steps the gate depends on,
// dues viewing, payment history, push setup, and lifecycle status.
//
// Gated: amenities and auxiliary features requiring KYC onboarding completion.
func registerTenantRoutes(tenant, tenantAuth *gin.RouterGroup, h *Handlers) {
	// --- exempt from the KYC onboarding gate ---
	tenant.GET("/me", h.TenantMe)
	tenant.PATCH("/me", h.TenantPatchMe)
	tenant.GET("/lifecycle", h.TenantLifecycle)
	tenant.POST("/push/subscribe", h.TenantPushSubscribe)
	tenantAuth.POST("/aadhaar", h.TenantIDRateLimit(10.0/60, 5), h.TenantAadhaar)

	tenantAuth.POST("/kyc/consent", h.TenantIDRateLimit(10.0/60, 5), h.TenantKYCConsent)
	tenantAuth.POST("/kyc/digilocker/init", h.TenantIDRateLimit(10.0/60, 2), h.TenantKYCDigiLockerInit)
	tenantAuth.GET("/kyc/digilocker/status", tenantIDRateLimit(20.0/60, 5), h.TenantKYCDigiLockerStatus)
	tenantAuth.POST("/kyc/upload", h.TenantIDRateLimit(5.0/60, 2), h.TenantKYCUpload)
	tenantAuth.POST("/kyc/qr/verify", h.TenantIDRateLimit(10.0/60, 5), h.TenantKYCSecureQRVerify)
	// Disabled by PG Cashflow product policy. Both endpoints return 410 Gone; use /kyc/upload or /kyc/qr/verify.
	tenantAuth.POST("/kyc/aadhaar/otp/request", ipRateLimit(5.0/60, 2), h.TenantKYCAadhaarOTPRequest)
	tenantAuth.POST("/kyc/aadhaar/otp/verify", ipRateLimit(10.0/60, domain.AadhaarOTPMaxAttempts), h.TenantKYCAadhaarOTPVerify)
	tenantAuth.POST("/kyc/revoke", h.TenantIDRateLimit(10.0/60, 5), h.TenantKYCRevoke)
	tenantAuth.GET("/kyc/status", h.TenantKYCStatus)

	// Financial obligations: viewing dues and payment receipts is accessible
	// even if KYC has expired or is under review, so tenants can see their balance.
	tenant.GET("/dues", h.TenantDues)
	tenant.GET("/dues/options", h.TenantDuesOptions)
	tenant.GET("/payments", h.TenantPayments)
	tenant.GET("/payments/:id/receipt", h.TenantPaymentReceipt)
	tenant.GET("/refund-account", h.TenantGetRefundAccount)
	tenantAuth.POST("/refund-account", h.TenantIDRateLimit(5.0/60, 3), h.TenantRegisterRefundAccount)

	// Order verification routes remain accessible without the new-payment KYC gate:
	// an already-created payment order must stay reconcilable if KYC status changes
	// while the tenant is at Cashfree checkout. Auth and ownership checks still apply.
	tenantAuth.POST("/dues/:id/pay/verify", h.TenantDuePayVerify)
	tenantAuth.POST("/payments/orders/:order_id/verify", h.TenantOrderPayVerify)

	// Payment routes enforce mandatory platform-wide KYC verification + authoritative auth
	payGated := tenantAuth.Group("", h.RequireKYCIfPolicyEnforced())
	payGated.GET("/dues/:id/qr", h.TenantDueQR)
	payGated.POST("/dues/pay-batch", h.TenantDuePayBatch)
	payGated.GET("/dues/:id/pay", h.TenantDuePay)
	payGated.POST("/dues/:id/pay", h.TenantDuePay)
	payGated.POST("/dues/:id/reports", h.TenantSubmitReport)

	// --- gated: requires the KYC onboarding gate (default-deny) ---
	gated := tenant.Group("", h.RequireKYCOnboardingPassed())

	gated.GET("/points", h.TenantPoints)
	gated.GET("/rewards", h.TenantRewards)
	gated.POST("/rewards/:id/redeem", h.TenantRedeem)
	gated.GET("/inspections", h.TenantInspections)
	gated.POST("/inspections/items/:id/dispute", h.TenantDisputeInspectionItem)
	gated.GET("/meal-rsvp", h.TenantGetMealRSVP)
	gated.POST("/meal-rsvp", h.TenantSubmitMealRSVP)
	gated.GET("/menu-poll", h.TenantGetMenuPoll)
	gated.POST("/menu-poll/vote", h.TenantVoteMenuPoll)
	gated.POST("/hazards", h.TenantReportHazard)
	gated.POST("/complaints", h.TenantReportHazard)
	gated.GET("/violations", h.TenantViolations)
	gated.GET("/leaderboard", h.TenantLeaderboard)
	gated.GET("/referrals", h.TenantReferrals)
	gated.POST("/referrals", h.TenantReferrals)
}
