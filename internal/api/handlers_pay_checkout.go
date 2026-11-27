package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type verifyCacheEntry struct {
	checkedAt time.Time
	paid      bool
	status    string
	cfID      string
	bankRef   string
	amount    int64
}

var (
	verifyCacheMu sync.Mutex
	verifyCache   = make(map[string]verifyCacheEntry)
)

const (
	verifyThrottleDuration = 3 * time.Second
	maxVerifyCacheEntries  = 1000
)

func setVerifyCache(orderID string, entry verifyCacheEntry) {
	verifyCacheMu.Lock()
	defer verifyCacheMu.Unlock()

	now := entry.checkedAt
	for k, v := range verifyCache {
		if now.Sub(v.checkedAt) > 2*time.Minute {
			delete(verifyCache, k)
		}
	}
	for len(verifyCache) >= maxVerifyCacheEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range verifyCache {
			if oldestKey == "" || v.checkedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.checkedAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(verifyCache, oldestKey)
	}
	verifyCache[orderID] = entry
}

func (h *Handlers) duePayJSON(c *gin.Context, due *domain.Due, role string) {
	if due.Status == domain.DueStatusPaid || due.Status == domain.DueStatusWaived {
		avail := "PAID"
		if due.Status == domain.DueStatusWaived {
			avail = "WAIVED"
		}
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:               domain.PaymentModeManual,
			Note:               domain.UPINote(due.DueCode),
			DueCode:            due.DueCode,
			AmountPaise:        due.Amount,
			QRPNGURL:           collector.PNGURL(role, due.ID),
			Payable:            false,
			AvailabilityStatus: avail,
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
	// For read-only queries (GET) when property uses Cashfree:
	// Do NOT initiate an external Cashfree order on GET.
	// Only return the active/unexpired intent if one already exists;
	// otherwise return a read-only PayIntent without creating a provider order.
	if c.Request.Method == http.MethodGet && prop.PaymentMode == domain.PaymentModeCashfree {
		payable := due.Status == domain.DueStatusPending || due.Status == domain.DueStatusPartial
		if h.IntentLifecycle != nil {
			if existing, _ := h.IntentLifecycle.ReusableSessionForDue(c.Request.Context(), due.ID, 0); existing != nil && existing.PaymentSessionID != nil && *existing.PaymentSessionID != "" {
				c.JSON(http.StatusOK, domain.PayIntent{
					Mode:             domain.PaymentModeCashfree,
					PaymentSessionID: *existing.PaymentSessionID,
					DueCode:          due.DueCode,
					AmountPaise:      due.Amount,
					Note:             domain.UPINote(due.DueCode),
					Payable:          payable,
				})
				return
			}
		} else if h.IntentStore != nil {
			if existing, _ := h.IntentStore.LatestOpenForDue(c.Request.Context(), due.ID); existing != nil && existing.PaymentSessionID != nil && *existing.PaymentSessionID != "" {
				c.JSON(http.StatusOK, domain.PayIntent{
					Mode:             domain.PaymentModeCashfree,
					PaymentSessionID: *existing.PaymentSessionID,
					DueCode:          due.DueCode,
					AmountPaise:      due.Amount,
					Note:             domain.UPINote(due.DueCode),
					Payable:          payable,
				})
				return
			}
		}
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:        domain.PaymentModeCashfree,
			DueCode:     due.DueCode,
			AmountPaise: due.Amount,
			Note:        domain.UPINote(due.DueCode),
			Payable:     payable,
		})
		return
	}

	if h.Collector == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "collector"})
		return
	}

	intent, _, err := h.Collector.PayIntent(c.Request.Context(), due, prop, room, collector.PNGURL(role, due.ID), phone)
	if err != nil {
		respondErr(c, checkoutClientErr(err))
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
	if ok, reason := h.tenantPayability(c.Request.Context(), t); !ok {
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:               domain.PaymentModeManual,
			Payable:            false,
			AvailabilityStatus: reason,
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

type orderAttemptVerifier interface {
	FetchOrderPayments(ctx context.Context, orderID string) ([]cashfree.PaymentAttempt, error)
	GetOrder(ctx context.Context, orderID string) (*cashfree.OrderDetails, error)
}

func isTerminalAttemptFailure(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "FAILED", "USER_DROPPED", "CANCELLED", "VOID", "FLAGGED":
		return true
	default:
		return false
	}
}

func (h *Handlers) verifyAndClassifyOrder(ctx context.Context, orderID string, intent *domain.PaymentIntent) (paid bool, status string) {
	verifyCacheMu.Lock()
	cached, hasCached := verifyCache[orderID]
	if hasCached && time.Since(cached.checkedAt) < verifyThrottleDuration && !cached.paid && cached.status != "" {
		verifyCacheMu.Unlock()
		return false, cached.status
	}
	verifyCacheMu.Unlock()

	if verifier, ok := h.CashfreeClient.(orderAttemptVerifier); ok {
		attempts, err := verifier.FetchOrderPayments(ctx, orderID)
		if err != nil {
			setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "PENDING"})
			return false, "PENDING"
		}

		var successes []cashfree.PaymentAttempt
		seenSuccess := make(map[string]struct{})
		hasNonTerminal := false
		for _, a := range attempts {
			if a.PaymentStatus == "SUCCESS" {
				if a.CFPaymentID == "" || a.AmountPaise <= 0 {
					setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "PENDING"})
					return false, "PENDING"
				}
				if _, dup := seenSuccess[a.CFPaymentID]; !dup {
					seenSuccess[a.CFPaymentID] = struct{}{}
					successes = append(successes, a)
				}
				continue
			}
			if !isTerminalAttemptFailure(a.PaymentStatus) {
				hasNonTerminal = true
			}
		}

		if len(successes) == 1 {
			s := successes[0]
			txn := s.BankReference
			if txn == "" {
				txn = s.CFPaymentID
			}
			if err := h.SettleGatewayOrder(ctx, orderID, s.CFPaymentID, txn, s.AmountPaise, intent, nil); err != nil {
				setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "PENDING"})
				return false, "PENDING"
			}
			setVerifyCache(orderID, verifyCacheEntry{
				checkedAt: time.Now(),
				paid:      true,
				status:    "PAID",
				cfID:      s.CFPaymentID,
				bankRef:   s.BankReference,
				amount:    s.AmountPaise,
			})
			return true, "PAID"
		}
		if len(successes) > 1 || hasNonTerminal {
			setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "PENDING"})
			return false, "PENDING"
		}

		// Zero attempts or all attempts are confirmed terminal failures: check order status.
		order, oErr := verifier.GetOrder(ctx, orderID)
		if oErr != nil || order == nil {
			setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "PENDING"})
			return false, "PENDING"
		}
		switch order.OrderStatus {
		case "EXPIRED", "TERMINATED":
			setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "EXPIRED"})
			return false, "EXPIRED"
		case "ACTIVE":
			setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "RETRYABLE"})
			return false, "RETRYABLE"
		default:
			setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "PENDING"})
			return false, "PENDING"
		}
	}

	cfID, bankRef, amount, ok, err := h.CashfreeClient.FetchSuccessfulPayment(ctx, orderID)
	if err != nil || !ok || amount <= 0 {
		setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "PENDING"})
		return false, "PENDING"
	}
	txn := bankRef
	if txn == "" {
		txn = cfID
	}
	if err := h.SettleGatewayOrder(ctx, orderID, cfID, txn, amount, intent, nil); err != nil {
		setVerifyCache(orderID, verifyCacheEntry{checkedAt: time.Now(), paid: false, status: "PENDING"})
		return false, "PENDING"
	}
	setVerifyCache(orderID, verifyCacheEntry{
		checkedAt: time.Now(),
		paid:      true,
		status:    "PAID",
		cfID:      cfID,
		bankRef:   bankRef,
		amount:    amount,
	})
	return true, "PAID"
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
	if due.Status == domain.DueStatusPaid {
		c.JSON(http.StatusOK, gin.H{"paid": true, "status": "PAID"})
		return
	}
	if due.Status == domain.DueStatusWaived {
		c.JSON(http.StatusOK, gin.H{"paid": false, "status": "WAIVED"})
		return
	}

	if (h.IntentLifecycle == nil && h.IntentStore == nil) || h.CashfreeClient == nil || h.Payments == nil {
		c.JSON(http.StatusOK, gin.H{"paid": false, "status": "PENDING"})
		return
	}

	var intent *domain.PaymentIntent
	if h.IntentLifecycle != nil {
		intent, err = h.IntentLifecycle.IntentToVerifyForDue(c.Request.Context(), id)
	} else {
		intent, err = h.IntentStore.LatestOpenForDue(c.Request.Context(), id)
	}
	if err != nil || intent == nil || intent.ProviderOrderID == "" {
		c.JSON(http.StatusOK, gin.H{"paid": false, "status": "PENDING"})
		return
	}

	paid, status := h.verifyAndClassifyOrder(c.Request.Context(), intent.ProviderOrderID, intent)
	c.JSON(http.StatusOK, gin.H{"paid": paid, "status": status})
}

// TenantOrderPayVerify handles POST /tenant/payments/orders/:order_id/verify
// and owner order verification. It verifies a checkout order return from Cashfree using
// the order_id, ensuring that the order belongs to the authenticated tenant or owner property.
func (h *Handlers) TenantOrderPayVerify(c *gin.Context) {
	t := tenantFromContext(c)
	var pid uuid.UUID
	var isOwner bool
	if t == nil {
		pid, isOwner = propertyIDFromClaims(c)
		if !isOwner || pid == uuid.Nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
	}
	orderID := strings.TrimSpace(c.Param("order_id"))
	if orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "order_id required"})
		return
	}

	if (h.IntentLifecycle == nil && h.IntentStore == nil) || h.CashfreeClient == nil || h.Payments == nil {
		c.JSON(http.StatusOK, gin.H{"paid": false, "status": "PENDING", "due_ids": []string{}})
		return
	}

	var intent *domain.PaymentIntent
	var err error
	if h.IntentLifecycle != nil {
		intent, err = h.IntentLifecycle.IntentToVerify(c.Request.Context(), orderID)
	} else {
		intent, err = h.IntentStore.GetByOrderID(c.Request.Context(), orderID)
	}
	if err != nil || intent == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
		return
	}

	// Ownership verification: Verify intent's dues belong strictly to this authenticated tenant or owner property.
	// Deduplicate legacy intent.DueID and normalized snapshot entries, and fail closed if snapshot read errors.
	seenDue := make(map[uuid.UUID]struct{})
	var targetDueIDs []uuid.UUID
	addDueID := func(id uuid.UUID) {
		if id == uuid.Nil {
			return
		}
		if _, ok := seenDue[id]; ok {
			return
		}
		seenDue[id] = struct{}{}
		targetDueIDs = append(targetDueIDs, id)
	}
	addDueID(intent.DueID)
	snapshot, sErr := h.IntentStore.GetDuesSnapshot(c.Request.Context(), intent.ID)
	if sErr != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unable to verify linked dues"})
		return
	}
	for _, sn := range snapshot {
		addDueID(sn.DueID)
	}

	if len(targetDueIDs) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "order has no linked dues"})
		return
	}

	// Verify ownership of linked dues
	for _, dueID := range targetDueIDs {
		due, dErr := h.DueStore.GetByID(c.Request.Context(), dueID)
		if dErr != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "linked due not found"})
			return
		}
		if t != nil && due.TenantID != t.ID {
			c.JSON(http.StatusNotFound, gin.H{"error": "order does not belong to tenant"})
			return
		}
		if isOwner && due.PropertyID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "order does not belong to owner property"})
			return
		}
	}

	dueIDStrs := make([]string, 0, len(targetDueIDs))
	for _, id := range targetDueIDs {
		dueIDStrs = append(dueIDStrs, id.String())
	}

	if intent.Status == domain.IntentPaid {
		c.JSON(http.StatusOK, gin.H{"paid": true, "status": "PAID", "due_ids": dueIDStrs})
		return
	}

	paid, status := h.verifyAndClassifyOrder(c.Request.Context(), orderID, intent)
	c.JSON(http.StatusOK, gin.H{"paid": paid, "status": status, "due_ids": dueIDStrs})
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
	if ok, reason := h.tenantPayability(c.Request.Context(), t); !ok {
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:               domain.PaymentModeManual,
			Payable:            false,
			AvailabilityStatus: reason,
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
	options, err := domain.CalculatePaymentOptionsChecked(ptrList)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "total due amount exceeds supported limit"})
		return
	}
	if len(options) == 0 {
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:               domain.PaymentModeManual,
			Payable:            false,
			AvailabilityStatus: "NO_OPEN_DUES",
			Note:               "no open dues",
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
		respondErr(c, checkoutClientErr(err))
		return
	}
	c.JSON(http.StatusOK, intent)
}
