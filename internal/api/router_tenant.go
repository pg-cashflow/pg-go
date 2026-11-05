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
func registerTenantRoutes(tenant *gin.RouterGroup, h *Handlers) {
	// --- exempt from the KYC onboarding gate ---
	tenant.GET("/me", h.TenantMe)
	tenant.PATCH("/me", h.TenantPatchMe)
	tenant.GET("/lifecycle", h.TenantLifecycle)
	tenant.POST("/push/subscribe", h.TenantPushSubscribe)
	tenant.POST("/aadhaar", h.TenantIDRateLimit(10.0/60, 5), h.TenantAadhaar)

	tenant.POST("/kyc/consent", h.TenantIDRateLimit(10.0/60, 5), h.TenantKYCConsent)
	tenant.POST("/kyc/upload", h.TenantIDRateLimit(5.0/60, 2), h.TenantKYCUpload)
	tenant.POST("/kyc/qr/verify", h.TenantIDRateLimit(10.0/60, 5), h.TenantKYCSecureQRVerify)
	tenant.POST("/kyc/aadhaar/otp/request", h.TenantIDRateLimit(5.0/60, 2), h.TenantKYCAadhaarOTPRequest)
	tenant.POST("/kyc/aadhaar/otp/verify", h.TenantIDRateLimit(10.0/60, domain.AadhaarOTPMaxAttempts), h.TenantKYCAadhaarOTPVerify)
	tenant.POST("/kyc/revoke", h.TenantIDRateLimit(10.0/60, 5), h.TenantKYCRevoke)
	tenant.GET("/kyc/status", h.TenantKYCStatus)

	// Financial obligations: viewing dues and payment receipts is accessible
	// even if KYC has expired or is under review, so tenants can see their balance.
	tenant.GET("/dues", h.TenantDues)
	tenant.GET("/dues/options", h.TenantDuesOptions)
	tenant.GET("/payments", h.TenantPayments)
	tenant.GET("/payments/:id/receipt", h.TenantPaymentReceipt)

	// Payment routes enforce KYC verification based on property policy (RequireKYCBeforePay)
	payGated := tenant.Group("", h.RequireKYCIfPolicyEnforced())
	payGated.GET("/dues/:id/qr", h.TenantDueQR)
	payGated.POST("/dues/pay-batch", h.TenantDuePayBatch)
	payGated.GET("/dues/:id/pay", h.TenantDuePay)
	payGated.POST("/dues/:id/pay/verify", h.TenantDuePayVerify)
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
