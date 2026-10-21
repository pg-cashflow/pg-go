package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

func (h *Handlers) duePayJSON(c *gin.Context, due *domain.Due, role string) {
	if due.Status == domain.DueStatusPaid || due.Status == domain.DueStatusWaived {
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:        domain.PaymentModeManual,
			Note:        domain.UPINote(due.DueCode),
			DueCode:     due.DueCode,
			AmountPaise: due.Amount,
			QRPNGURL:    collector.PNGURL(role, due.ID),
			Payable:     false,
		})
		return
	}
	prop, err := h.PropertyStore.GetByID(c.Request.Context(), due.PropertyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "property"})
		return
	}
	room, phone := "", ""
	if t, err := h.TenantStore.GetByID(c.Request.Context(), due.TenantID); err == nil {
		if t.RoomNumber != nil {
			room = *t.RoomNumber
		}
		if t.Phone != nil {
			phone = *t.Phone
		}
	}
	if h.Collector == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "collector"})
		return
	}
	intent, _, err := h.Collector.PayIntent(c.Request.Context(), due, prop, room, collector.PNGURL(role, due.ID), phone)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "pay intent failed"})
		return
	}
	c.JSON(http.StatusOK, intent)
}

// OwnerDuePay handles GET /owner/dues/:id/pay.
func (h *Handlers) OwnerDuePay(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	due, err := h.DueStore.GetByID(c.Request.Context(), id)
	if err != nil || due.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	h.duePayJSON(c, due, "owner")
}

// TenantDuePay handles GET /tenant/dues/:id/pay.
func (h *Handlers) TenantDuePay(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !h.tenantPayable(c.Request.Context(), t) {
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:    domain.PaymentModeManual,
			Payable: false,
		})
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	due, err := h.DueStore.GetByID(c.Request.Context(), id)
	if err != nil || due.TenantID != t.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	h.duePayJSON(c, due, "tenant")
}

// TenantDuePayVerify handles POST /tenant/dues/:id/pay/verify.
func (h *Handlers) TenantDuePayVerify(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	due, err := h.DueStore.GetByID(c.Request.Context(), id)
	if err != nil || due.TenantID != t.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if due.Status == domain.DueStatusPaid || due.Status == domain.DueStatusWaived {
		c.JSON(http.StatusOK, gin.H{"paid": true})
		return
	}

	if h.IntentStore == nil || h.CashfreeClient == nil || h.Payments == nil {
		c.JSON(http.StatusOK, gin.H{"paid": false})
		return
	}

	intent, err := h.IntentStore.LatestOpenForDue(c.Request.Context(), id)
	if err != nil || intent == nil || intent.ProviderOrderID == "" {
		c.JSON(http.StatusOK, gin.H{"paid": false})
		return
	}

	cfID, bankRef, amount, ok, err := h.CashfreeClient.FetchSuccessfulPayment(c.Request.Context(), intent.ProviderOrderID)
	if err != nil || !ok || amount <= 0 {
		c.JSON(http.StatusOK, gin.H{"paid": false})
		return
	}

	txn := bankRef
	if txn == "" {
		txn = cfID
	}
	dedupKey := ""
	if cfID != "" {
		dedupKey = fmt.Sprintf("cashfree:pg:%s", cfID)
	}

	snapshot, snapErr := h.IntentStore.GetDuesSnapshot(c.Request.Context(), intent.ID)
	if snapErr == nil && len(snapshot) > 1 {
		for _, item := range snapshot {
			itemDedup := ""
			if dedupKey != "" {
				itemDedup = fmt.Sprintf("%s:%s", dedupKey, item.DueID)
			}
			_, _ = h.Payments.GatewaySettle(c.Request.Context(), item.DueID, item.AmountPaise, txn, itemDedup)
		}
	} else {
		if dedupKey != "" {
			_, err = h.Payments.GatewaySettle(c.Request.Context(), due.ID, amount, txn, dedupKey)
		} else {
			_, err = h.Payments.GatewaySettle(c.Request.Context(), due.ID, amount, txn)
		}
		if err != nil && !errors.Is(err, payment.ErrDuplicateTxn) {
			c.JSON(http.StatusOK, gin.H{"paid": false})
			return
		}
	}

	if cfID != "" {
		_ = h.IntentStore.MarkPaid(c.Request.Context(), intent.ID, cfID)
	}

	c.JSON(http.StatusOK, gin.H{"paid": true})
}

type PayBatchRequest struct {
	OptionType domain.PaymentOptionType `json:"option_type"`
	DueIDs     []uuid.UUID              `json:"due_ids"`
}

// TenantDuePayBatch handles POST /tenant/dues/pay-batch.
func (h *Handlers) TenantDuePayBatch(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !h.tenantPayable(c.Request.Context(), t) {
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:    domain.PaymentModeManual,
			Payable: false,
		})
		return
	}

	var req PayBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	prop, err := h.PropertyStore.GetByID(c.Request.Context(), t.PropertyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "property"})
		return
	}

	room, phone := "", ""
	if t.RoomNumber != nil {
		room = *t.RoomNumber
	}
	if t.Phone != nil {
		phone = *t.Phone
	}

	allDues, err := h.DueStore.ListByTenant(c.Request.Context(), t.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list dues failed"})
		return
	}
	ptrList := make([]*domain.Due, len(allDues))
	for i := range allDues {
		ptrList[i] = &allDues[i]
	}
	options := domain.CalculatePaymentOptions(ptrList)
	if len(options) == 0 {
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:    domain.PaymentModeManual,
			Payable: false,
			Note:    "no open dues",
		})
		return
	}

	var chosenOption *domain.PaymentOption
	if req.OptionType != "" {
		for i := range options {
			if options[i].OptionType == req.OptionType {
				chosenOption = &options[i]
				break
			}
		}
		if chosenOption == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid option_type for open dues"})
			return
		}
	} else if len(req.DueIDs) > 0 {
		for i := range options {
			if len(options[i].DueIDs) == len(req.DueIDs) {
				match := true
				for idx, did := range options[i].DueIDs {
					if did != req.DueIDs[idx] {
						match = false
						break
					}
				}
				if match {
					chosenOption = &options[i]
					break
				}
			}
		}
		if chosenOption == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "due_ids must match a valid FIFO payment option"})
			return
		}
	} else {
		chosenOption = &options[0]
	}

	duesMap := make(map[uuid.UUID]*domain.Due)
	for _, d := range ptrList {
		duesMap[d.ID] = d
	}
	chosenDues := make([]*domain.Due, len(chosenOption.DueIDs))
	for i, did := range chosenOption.DueIDs {
		chosenDues[i] = duesMap[did]
	}

	if h.Collector == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "collector"})
		return
	}

	intent, err := h.Collector.MultiDuePayIntent(c.Request.Context(), chosenDues, prop, room, phone)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "multi pay intent failed"})
		return
	}
	c.JSON(http.StatusOK, intent)
}
