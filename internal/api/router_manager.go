package api

import (
	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/auth"
)

func registerManagerRoutes(manager, managerAuth *gin.RouterGroup, d Deps, h *Handlers) {
	capOf := func(name ...string) gin.HandlerFunc {
		if len(name) == 1 {
			return auth.RequireCapability(name[0], d.CapabilityStore)
		}
		return auth.RequireAnyCapability(d.CapabilityStore, name...)
	}
	manager.GET("/me/capabilities", h.ManagerMyCapabilities)
	manager.GET("/onboarding-settings", h.ManagerGetOnboardingSettings)
	managerAuth.POST("/slot-invites", capOf("slot_invites.create"), h.OwnerCreateSlotInvite)
	manager.GET("/slot-invites", capOf("slot_invites.create"), h.OwnerListSlotInvites)
	managerAuth.POST("/slot-invites/:id/revoke", capOf("slot_invites.create"), h.OwnerRevokeSlotInvite)
	manager.GET("/tenants", capOf("tenants.view"), h.ListTenants)
	manager.GET("/dues", capOf("dues.view"), h.ListDues)
	manager.POST("/attendance/daily", capOf("attendance.mark"), h.OwnerMarkDailyAttendance)
	manager.POST("/inspections", capOf("inspections.write"), h.ManagerSubmitInspection)
	manager.GET("/inspections", capOf("inspections.write"), h.ManagerListInspections)
	manager.POST("/inspections/items/:id/resolve", capOf("inspections.write"), h.ManagerResolveInspectionItem)
	manager.POST("/violations", capOf("inspections.write"), h.ManagerLogViolation)
	manager.POST("/meter-readings", capOf("meters.write"), h.ManagerRecordMeterReading)
	manager.GET("/kitchen/headcount", capOf("kitchen.headcount"), h.ManagerKitchenHeadcount)
	manager.GET("/hazards", capOf("complaints.respond", "inspections.write"), h.ManagerListHazards)
	manager.POST("/hazards/:id/resolve", capOf("complaints.respond", "inspections.write"), h.ManagerResolveHazard)
	manager.GET("/complaints", capOf("complaints.respond", "inspections.write"), h.ManagerListHazards)
	manager.POST("/complaints/:id/resolve", capOf("complaints.respond", "inspections.write"), h.ManagerResolveHazard)
	manager.POST("/vendor-inspections", capOf("inspections.write"), h.ManagerSubmitVendorInspection)
	manager.GET("/finance/expenses", capOf("expenses.post"), h.ListExpenses)
	managerAuth.POST("/finance/expenses", capOf("expenses.post"), h.PostExpense)
	managerAuth.POST("/finance/expenses/:id/void", capOf("expenses.void"), h.VoidExpense)
	managerAuth.POST("/finance/expenses/:id/payments", capOf("expenses.post"), h.PostExpensePayment)
	manager.GET("/finance/today", capOf("expenses.post"), h.ManagerFinanceToday)
	managerAuth.POST("/finance/meal-prep", capOf("expenses.post"), h.PostMealPrep)
	// KYC review routes are owner-only under Invariant 0.1 and removed from manager group.
	managerAuth.POST("/payments/:id/refund/requests", capOf("refunds.request"), h.ManagerCreateRefundRequest)
	managerAuth.POST("/payments/:id/refunds/:refund_id/cancel", capOf("refunds.request"), h.ManagerCancelRefund)
	manager.GET("/payments/:id/refunds", capOf("refunds.request"), h.GetPaymentRefundHistory)
}
