package api

import "github.com/gin-gonic/gin"

func registerTenantRoutes(tenant *gin.RouterGroup, h *Handlers) {
	tenant.GET("/me", h.TenantMe)
	tenant.PATCH("/me", h.TenantPatchMe)
	tenant.GET("/dues", h.TenantDues)
	tenant.GET("/dues/options", h.TenantDuesOptions)
	tenant.POST("/dues/pay-batch", h.TenantDuePayBatch)
	tenant.GET("/dues/:id/qr", h.TenantDueQR)
	tenant.GET("/dues/:id/pay", h.TenantDuePay)
	tenant.POST("/dues/:id/reports", h.TenantSubmitReport)
	tenant.GET("/payments", h.TenantPayments)
	tenant.GET("/payments/:id/receipt", h.TenantPaymentReceipt)
	tenant.GET("/lifecycle", h.TenantLifecycle)
	tenant.POST("/push/subscribe", h.TenantPushSubscribe)
	tenant.POST("/aadhaar", h.TenantAadhaar)

	tenant.POST("/kyc/consent", h.TenantKYCConsent)
	tenant.POST("/kyc/upload", h.TenantIDRateLimit(5.0/60, 2), h.TenantKYCUpload)
	tenant.POST("/kyc/aadhaar/otp/request", h.TenantIDRateLimit(5.0/60, 2), h.TenantKYCAadhaarOTPRequest)
	tenant.POST("/kyc/aadhaar/otp/verify", h.TenantIDRateLimit(10.0/60, 3), h.TenantKYCAadhaarOTPVerify)
	tenant.POST("/kyc/qr", h.TenantKYCSubmitQR)
	tenant.POST("/kyc/revoke", h.TenantKYCRevoke)
	tenant.GET("/kyc/status", h.TenantKYCStatus)

	tenant.GET("/points", h.TenantPoints)
	tenant.GET("/rewards", h.TenantRewards)
	tenant.POST("/rewards/:id/redeem", h.TenantRedeem)
	tenant.GET("/inspections", h.TenantInspections)
	tenant.POST("/inspections/items/:id/dispute", h.TenantDisputeInspectionItem)
	tenant.GET("/meal-rsvp", h.TenantGetMealRSVP)
	tenant.POST("/meal-rsvp", h.TenantSubmitMealRSVP)
	tenant.GET("/menu-poll", h.TenantGetMenuPoll)
	tenant.POST("/menu-poll/vote", h.TenantVoteMenuPoll)
	tenant.POST("/hazards", h.TenantReportHazard)
	tenant.GET("/violations", h.TenantViolations)
	tenant.GET("/leaderboard", h.TenantLeaderboard)
	tenant.GET("/referrals", h.TenantReferrals)
	tenant.POST("/referrals", h.TenantReferrals)
}
