package api

import (
	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// registerTenantRoutes wires tenant routes in two explicit groups.
//
// Exempt (no KYC onboarding gate): the profile, the identity steps the gate depends on,
// push setup, and lifecycle status. Keep this list short.
//
// Gated (default): every other tenant route, including dues and payments, requires the
// KYC onboarding gate. Add new routes to the gated group unless they are identity steps.
func registerTenantRoutes(tenant *gin.RouterGroup, h *Handlers) {
	// --- exempt from the KYC onboarding gate ---
	tenant.GET("/me", h.TenantMe)
	tenant.PATCH("/me", h.TenantPatchMe)
	tenant.GET("/lifecycle", h.TenantLifecycle)
	tenant.POST("/push/subscribe", h.TenantPushSubscribe)
	tenant.POST("/aadhaar", h.TenantAadhaar)

	tenant.POST("/kyc/consent", h.TenantKYCConsent)
	tenant.POST("/kyc/upload", h.TenantIDRateLimit(5.0/60, 2), h.TenantKYCUpload)
	tenant.POST("/kyc/aadhaar/otp/request", h.TenantIDRateLimit(5.0/60, 2), h.TenantKYCAadhaarOTPRequest)
	tenant.POST("/kyc/aadhaar/otp/verify", h.TenantIDRateLimit(10.0/60, domain.AadhaarOTPMaxAttempts), h.TenantKYCAadhaarOTPVerify)
	tenant.POST("/kyc/revoke", h.TenantKYCRevoke)
	tenant.GET("/kyc/status", h.TenantKYCStatus)

	// --- gated: requires the KYC onboarding gate (default-deny) ---
	gated := tenant.Group("", h.RequireKYCOnboardingPassed())

	gated.GET("/dues", h.TenantDues)
	gated.GET("/dues/options", h.TenantDuesOptions)
	gated.POST("/dues/pay-batch", h.TenantDuePayBatch)
	gated.GET("/dues/:id/qr", h.TenantDueQR)
	gated.GET("/dues/:id/pay", h.TenantDuePay)
	gated.POST("/dues/:id/reports", h.TenantSubmitReport)
	gated.GET("/payments", h.TenantPayments)
	gated.GET("/payments/:id/receipt", h.TenantPaymentReceipt)

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
	gated.GET("/violations", h.TenantViolations)
	gated.GET("/leaderboard", h.TenantLeaderboard)
	gated.GET("/referrals", h.TenantReferrals)
	gated.POST("/referrals", h.TenantReferrals)
}
