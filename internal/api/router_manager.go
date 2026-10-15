package api

import (
	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/auth"
)

func registerManagerRoutes(manager *gin.RouterGroup, d Deps, h *Handlers) {
	capOf := func(name string) gin.HandlerFunc {
		return auth.RequireCapability(name, d.CapabilityStore)
	}
	manager.GET("/me/capabilities", h.ManagerMyCapabilities)
	manager.GET("/onboarding-settings", h.ManagerGetOnboardingSettings)
	manager.POST("/slot-invites", capOf("slot_invites.create"), h.OwnerCreateSlotInvite)
	manager.GET("/slot-invites", capOf("slot_invites.create"), h.OwnerListSlotInvites)
	manager.POST("/slot-invites/:id/revoke", capOf("slot_invites.create"), h.OwnerRevokeSlotInvite)
	manager.GET("/tenants", capOf("tenants.view"), h.ListTenants)
	manager.GET("/dues", capOf("dues.view"), h.ListDues)
	manager.POST("/attendance/daily", capOf("attendance.mark"), h.OwnerMarkDailyAttendance)
	manager.POST("/inspections", capOf("inspections.write"), h.ManagerSubmitInspection)
	manager.GET("/inspections", capOf("inspections.write"), h.ManagerListInspections)
	manager.POST("/inspections/items/:id/resolve", capOf("inspections.write"), h.ManagerResolveInspectionItem)
	manager.POST("/violations", capOf("inspections.write"), h.ManagerLogViolation)
	manager.POST("/meter-readings", capOf("meters.write"), h.ManagerRecordMeterReading)
	manager.GET("/kitchen/headcount", capOf("kitchen.headcount"), h.ManagerKitchenHeadcount)
	manager.GET("/hazards", capOf("inspections.write"), h.ManagerListHazards)
	manager.POST("/hazards/:id/resolve", capOf("inspections.write"), h.ManagerResolveHazard)
	manager.POST("/vendor-inspections", capOf("inspections.write"), h.ManagerSubmitVendorInspection)
	manager.GET("/finance/expenses", capOf("expenses.post"), h.ListExpenses)
	manager.POST("/finance/expenses", capOf("expenses.post"), h.PostExpense)
	manager.POST("/finance/expenses/:id/void", capOf("expenses.void"), h.VoidExpense)
	manager.POST("/finance/expenses/:id/payments", capOf("expenses.post"), h.PostExpensePayment)
	manager.GET("/finance/today", capOf("expenses.post"), h.ManagerFinanceToday)
	manager.POST("/finance/meal-prep", capOf("expenses.post"), h.PostMealPrep)
}
