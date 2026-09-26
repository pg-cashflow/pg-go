package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
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
	if !t.IsPayable() {
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
	if !t.IsPayable() {
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

const maxReportImage = 2 << 20

type reportBody struct {
	UPITxnID string `json:"upi_txn_id" binding:"required"`
	Amount   int    `json:"amount"`
	Note     string `json:"note"`
}

// TenantSubmitReport handles POST /tenant/dues/:id/reports (multipart optional image, or JSON).
func (h *Handlers) TenantSubmitReport(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
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
		c.JSON(http.StatusConflict, gin.H{"error": "already recorded"})
		return
	}

	var txnID string
	var amount int
	var note string
	var img []byte
	ct := c.ContentType()
	if strings.HasPrefix(ct, "multipart/") {
		txnID = strings.TrimSpace(c.PostForm("upi_txn_id"))
		amount = atoi(c.PostForm("amount"))
		note = c.PostForm("note")
		if f, err := c.FormFile("image"); err == nil && f != nil {
			if f.Size > maxReportImage {
				apierr.RespondClientErr(c, http.StatusBadRequest, "image too large (max 2MB)", apierr.CodeRequestImageTooLarge)
				return
			}
			rc, err := f.Open()
			if err == nil {
				img, _ = io.ReadAll(io.LimitReader(rc, maxReportImage+1))
				_ = rc.Close()
			}
			if len(img) > maxReportImage {
				apierr.RespondClientErr(c, http.StatusBadRequest, "image too large (max 2MB)", apierr.CodeRequestImageTooLarge)
				return
			}
			if len(img) > 0 && !allowedReportImage(img) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "image must be jpeg, png, or webp"})
				return
			}
		}
	} else {
		var body reportBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "upi_txn_id required"})
			return
		}
		txnID = strings.TrimSpace(body.UPITxnID)
		amount = body.Amount
		note = body.Note
	}
	if txnID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "upi_txn_id required"})
		return
	}
	if amount <= 0 {
		amount = due.Amount
	}
	if existing, err := h.ReportStore.GetByUPITxnID(c.Request.Context(), txnID); err == nil && existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "duplicate utr"})
		return
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	if h.PaymentLookup != nil {
		if p, err := h.PaymentLookup.GetByUPITxnID(c.Request.Context(), txnID); err == nil && p != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "duplicate utr"})
			return
		}
	}

	var imgHash *string
	var isDuplicate bool
	if len(img) > 0 {
		hSum := sha256.Sum256(img)
		hexHash := hex.EncodeToString(hSum[:])
		imgHash = &hexHash
		if dup, err := h.ReportStore.HasImageWithHash(c.Request.Context(), due.PropertyID, hexHash); err == nil && dup {
			isDuplicate = true
		}
	}

	rep := &domain.PaymentReport{
		DueID:       due.ID,
		TenantID:    t.ID,
		PropertyID:  due.PropertyID,
		UPITxnID:    txnID,
		Amount:      amount,
		ImageBytes:  img,
		ImageHash:   imgHash,
		IsDuplicate: isDuplicate,
		Status:      domain.ReportPendingReview,
		ReportedBy:  uid,
	}
	if note != "" {
		rep.Note = &note
	}
	if err := h.ReportStore.Create(c.Request.Context(), rep); err != nil {
		respondErr(c, err)
		return
	}
	rep.HasImage = len(img) > 0
	if h.Events != nil {
		payload, _ := json.Marshal(map[string]string{"report_id": rep.ID.String(), "upi_txn_id": txnID})
		did := due.ID
		_ = h.Events.Publish(c.Request.Context(), domain.Event{
			TenantID:   domain.Ptr(t.ID),
			PropertyID: due.PropertyID,
			EventType:  domain.EvtPaymentReportSubmitted,
			DueID:      &did,
			OccurredAt: rep.CreatedAt,
			Payload:    payload,
		})
	}
	if h.OutboxEvents != nil {
		tid := t.ID
		payload, _ := json.Marshal(map[string]string{"report_id": rep.ID.String()})
		_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
			EventType:  string(domain.EvtPaymentReportSubmitted),
			PropertyID: due.PropertyID,
			TenantID:   &tid,
			ActorRole:  string(domain.RoleTenant),
			Payload:    payload,
		})
	}
	c.JSON(http.StatusCreated, tenantReportResponse(rep))
}

func tenantReportResponse(p *domain.PaymentReport) gin.H {
	return gin.H{
		"id":          p.ID,
		"due_id":      p.DueID,
		"tenant_id":   p.TenantID,
		"property_id": p.PropertyID,
		"upi_txn_id":  p.UPITxnID,
		"amount":      p.Amount,
		"has_image":   p.HasImage,
		"status":      p.Status,
		"reported_by": p.ReportedBy,
		"note":        p.Note,
		"created_at":  p.CreatedAt,
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func allowedReportImage(b []byte) bool {
	switch http.DetectContentType(b) {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}

// ListPaymentReports handles GET /owner/payment-reports.
func (h *Handlers) ListPaymentReports(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var st *domain.PaymentReportStatus
	if q := c.Query("status"); q != "" {
		s := domain.PaymentReportStatus(q)
		st = &s
	}
	list, err := h.ReportStore.ListByProperty(c.Request.Context(), pid, st)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"payment_reports": list})
}

// ConfirmPaymentReport handles POST /owner/payment-reports/:id/confirm.
func (h *Handlers) ConfirmPaymentReport(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	rep, err := h.ReportStore.GetByID(c.Request.Context(), id)
	if err != nil || rep.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if rep.Status != domain.ReportPendingReview {
		c.JSON(http.StatusOK, rep)
		return
	}
	p, err := h.Payments.ManualMatch(c.Request.Context(), rep.DueID, rep.Amount, rep.UPITxnID, uid)
	if err != nil {
		respondErr(c, paymentClientErr(err))
		return
	}
	now := p.MatchedAt
	rep.Status = domain.ReportConfirmed
	rep.ReviewedBy = &uid
	rep.ReviewedAt = &now
	_ = h.ReportStore.UpdateReview(c.Request.Context(), rep)
	c.JSON(http.StatusOK, gin.H{"report": rep, "payment": p})
}

// RejectPaymentReport handles POST /owner/payment-reports/:id/reject.
func (h *Handlers) RejectPaymentReport(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	rep, err := h.ReportStore.GetByID(c.Request.Context(), id)
	if err != nil || rep.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if rep.Status != domain.ReportPendingReview {
		if rep.Status == domain.ReportConfirmed {
			c.JSON(http.StatusConflict, gin.H{"error": "already recorded"})
			return
		}
		c.JSON(http.StatusOK, rep)
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&body)
	now := time.Now().UTC()
	rep.Status = domain.ReportRejected
	rep.ReviewedBy = &uid
	rep.ReviewedAt = &now
	if body.Note != "" {
		rep.Note = &body.Note
	}
	_ = h.ReportStore.UpdateReview(c.Request.Context(), rep)
	if h.Events != nil {
		payload, _ := json.Marshal(map[string]string{"report_id": rep.ID.String()})
		did := rep.DueID
		_ = h.Events.Publish(c.Request.Context(), domain.Event{
			TenantID:   domain.Ptr(rep.TenantID),
			PropertyID: pid,
			EventType:  domain.EvtPaymentReportRejected,
			DueID:      &did,
			OccurredAt: now,
			Payload:    payload,
		})
	}
	if h.OutboxEvents != nil {
		tid := rep.TenantID
		payload, _ := json.Marshal(map[string]string{"report_id": rep.ID.String()})
		_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
			EventType:  string(domain.EvtPaymentReportRejected),
			PropertyID: pid,
			TenantID:   &tid,
			ActorRole:  string(domain.RoleOwner),
			Payload:    payload,
		})
	}
	c.JSON(http.StatusOK, rep)
}

const maxWebhookBody = 1 << 20

// CashfreeWebhook handles POST /webhooks/cashfree.
func (h *Handlers) CashfreeWebhook(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body"})
		return
	}
	if len(raw) > maxWebhookBody {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body too large"})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	// Defense-in-depth: when no secret is configured but IntentStore is wired,
	// refuse unauthenticated webhooks. Only skip when gateway is fully absent.
	if h.CashfreeSecret == "" {
		if h.IntentStore != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment gateway not configured"})
			return
		}
		c.Status(http.StatusOK)
		return
	}

	sig := c.GetHeader("x-webhook-signature")
	ts := c.GetHeader("x-webhook-timestamp")
	if !cashfree.VerifyWebhookHMAC(h.CashfreeSecret, ts, string(raw), sig) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	// Verify timestamp tolerance (drift check) if configured
	if h.WebhookToleranceSec > 0 {
		if err := cashfree.VerifyWebhookTimestamp(ts, h.WebhookToleranceSec, time.Now().UTC()); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp tolerance exceeded"})
			return
		}
	}

	// Ingest raw arrival into webhook_events audit log
	evtRecord := &domain.WebhookEvent{
		Provider:         "cashfree",
		Signature:        &sig,
		TimestampHeader:  &ts,
		RawPayload:       raw,
		ProcessingStatus: "received",
	}
	if h.GatewayPaymentRepo != nil {
		_ = h.GatewayPaymentRepo.CreateWebhookEvent(c.Request.Context(), evtRecord)
	}

	val, evtType, err := cashfree.ParseWebhook(raw)
	if err != nil {
		// DEAD-LETTER ISOLATION: mark error, alert owner, return 200 OK to stop gateway retry storms
		errMsg := err.Error()
		if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(c.Request.Context(), evtRecord.ID, "dead_letter", &errMsg)
		}
		slog.Default().Error("cashfree webhook dead-letter (unparseable payload)", "err", err)
		c.Status(http.StatusOK)
		return
	}

	evtRecord.EventType = evtType

	switch evtType {
	case "PAYMENT_SUCCESS_WEBHOOK":
		succ, ok := val.(cashfree.SuccessWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handlePaymentSuccessWebhook(c, succ, evtRecord)

	case "PAYMENT_FAILED_WEBHOOK":
		fail, ok := val.(cashfree.FailedWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handlePaymentFailedWebhook(c, fail, evtRecord)

	case "REFUND_STATUS_WEBHOOK", "AUTO_REFUND_STATUS_WEBHOOK":
		ref, ok := val.(cashfree.RefundWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handleRefundWebhook(c, ref, evtRecord)

	default:
		if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(c.Request.Context(), evtRecord.ID, "ignored", nil)
		}
		c.Status(http.StatusOK)
	}
}

func (h *Handlers) handlePaymentSuccessWebhook(c *gin.Context, succ cashfree.SuccessWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.IntentStore == nil {
		c.Status(http.StatusOK)
		return
	}

	intent, err := h.IntentStore.GetByOrderID(ctx, succ.OrderID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if h.GatewayPaymentRepo != nil {
				_ = h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, nil, int64(succ.AmountPaise), "unknown_order", evtRecord.RawPayload)
			}
			if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
				_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
			}
			c.Status(http.StatusOK)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	if succ.AmountPaise != intent.AmountPaise {
		if h.GatewayPaymentRepo != nil {
			_ = h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, &intent.ID, int64(succ.AmountPaise), "amount_mismatch", evtRecord.RawPayload)
		}
		c.Status(http.StatusOK)
		return
	}

	// When Pool & GatewayPaymentRepo are wired, run atomic resolve-then-lock settlement:
	if h.Pool != nil && h.GatewayPaymentRepo != nil {
		err := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			txDueRepo := postgres.NewDueRepo(tx)
			txIntentRepo := postgres.NewPaymentIntentRepo(tx)

			// Step 1: In-transaction deduplication
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_SETTLED", succ.CFPaymentID, "")
			if err != nil {
				return err
			}
			if !firstSeen {
				return nil // already processed
			}

			// Step 2: Dues snapshot from payment_intent_dues
			snapshot, err := txIntentRepo.GetDuesSnapshot(ctx, intent.ID)
			if err != nil || len(snapshot) == 0 {
				snapshot = []domain.PaymentIntentDue{{DueID: intent.DueID, AmountPaise: int64(intent.AmountPaise)}}
			}

			// Step 3: Resolve first due to determine tenantID
			due0, err := txDueRepo.GetByID(ctx, snapshot[0].DueID)
			if err != nil {
				return err
			}
			tenantID := due0.TenantID

			// Step 4: Acquire locks strictly top-down: tenants -> dues -> payment_intents
			var dummy uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&dummy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}

			allClosed := true
			lockedDues := make(map[uuid.UUID]*domain.Due)
			for _, item := range snapshot {
				d, err := txDueRepo.GetByIDForUpdate(ctx, item.DueID)
				if err != nil {
					return err
				}
				lockedDues[d.ID] = d
				if d.Status == domain.DueStatusPending || d.Status == domain.DueStatusPartial {
					allClosed = false
				}
			}

			var lockedIntent domain.PaymentIntent
			err = tx.QueryRow(ctx, `SELECT id, status FROM payment_intents WHERE id=$1 FOR UPDATE`, intent.ID).Scan(&lockedIntent.ID, &lockedIntent.Status)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if lockedIntent.Status == domain.IntentPaid {
				return nil // already paid
			}

			txnID := succ.TxnID()
			cfID := succ.CFPaymentID
			at := time.Now().UTC()

			if allClosed {
				// UNAPPLIED / RACE LOSER BRANCH (e.g. cash paid offline or duplicate payment)
				failureReason := "due_already_settled"
				if lockedIntent.Status == "superseded" {
					failureReason = "session_superseded"
				}
				_ = txPayRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, &intent.ID, int64(succ.AmountPaise), failureReason, evtRecord.RawPayload)

				note := "unapplied duplicate: dues already closed, pending owner refund or manual allocation"
				p := &domain.Payment{
					DueID:       snapshot[0].DueID,
					TenantID:    tenantID,
					UPITxnID:    &txnID,
					CFPaymentID: &cfID,
					Amount:      succ.AmountPaise,
					MatchedBy:   domain.MatchedByCashfree,
					MatchedAt:   at,
					IsUnapplied: true,
					RawNote:     &note,
				}
				if err := txPayRepo.Create(ctx, p); err != nil {
					return err
				}
				if h.Finance != nil {
					_ = h.Finance.MirrorUnappliedPayment(ctx, due0.PropertyID, p.ID, int64(succ.AmountPaise), at)
				}
				_ = txIntentRepo.MarkPaid(ctx, intent.ID, cfID)
				return nil
			}

			// APPLIED TO OPEN DUES BRANCH
			p := &domain.Payment{
				DueID:       snapshot[0].DueID,
				TenantID:    tenantID,
				UPITxnID:    &txnID,
				CFPaymentID: &cfID,
				Amount:      succ.AmountPaise,
				MatchedBy:   domain.MatchedByCashfree,
				MatchedAt:   at,
				IsUnapplied: false,
			}
			if err := txPayRepo.Create(ctx, p); err != nil {
				return err
			}

			remainingPaise := int64(succ.AmountPaise)
			for _, item := range snapshot {
				if remainingPaise <= 0 {
					break
				}
				targetDue := lockedDues[item.DueID]
				if targetDue.Status != domain.DueStatusPending && targetDue.Status != domain.DueStatusPartial {
					continue
				}
				allocPaise := int64(targetDue.Amount)
				if allocPaise > remainingPaise {
					allocPaise = remainingPaise
				}
				if err := txPayRepo.CreateAllocation(ctx, p.ID, targetDue.ID, allocPaise); err != nil {
					return err
				}
				credit := targetDue.ApplyPayment(int(allocPaise), at)
				if err := txDueRepo.Update(ctx, targetDue); err != nil {
					return err
				}
				if credit > 0 {
					_, _ = tx.Exec(ctx, `UPDATE tenants SET credit_balance_paise = credit_balance_paise + $2 WHERE id=$1`, tenantID, credit)
				}
				remainingPaise -= allocPaise
			}

			if h.Finance != nil {
				_ = h.Finance.MirrorPayment(ctx, p, due0)
			}
			_ = txIntentRepo.MarkPaid(ctx, intent.ID, cfID)
			return nil
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "settle failed"})
			return
		}
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
		}
		c.Status(http.StatusOK)
		return
	}

	// Legacy fallback path for test stubs without a pgxpool
	var dedupKey string
	if succ.CFPaymentID != "" {
		dedupKey = fmt.Sprintf("cashfree:pg:%s", succ.CFPaymentID)
		if existing, err := h.IntentStore.GetByCFPaymentID(ctx, succ.CFPaymentID); err == nil && existing != nil {
			c.Status(http.StatusOK)
			return
		}
	}
	txn := succ.TxnID()
	if dedupKey != "" {
		_, err = h.Payments.GatewaySettle(ctx, intent.DueID, succ.AmountPaise, txn, dedupKey)
	} else {
		_, err = h.Payments.GatewaySettle(ctx, intent.DueID, succ.AmountPaise, txn)
	}
	if err != nil {
		if errors.Is(err, payment.ErrDuplicateTxn) || errors.Is(err, payment.ErrDueNotOpen) || errors.Is(err, payment.ErrEmptyTxnID) {
			c.Status(http.StatusOK)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settle failed"})
		return
	}
	if succ.CFPaymentID != "" {
		_ = h.IntentStore.MarkPaid(ctx, intent.ID, succ.CFPaymentID)
	}
	if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

func (h *Handlers) handlePaymentFailedWebhook(c *gin.Context, fail cashfree.FailedWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.Pool != nil && h.GatewayPaymentRepo != nil {
		_ = postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_FAILED_WEBHOOK", fail.CFPaymentID, "")
			if err != nil || !firstSeen {
				return err
			}
			_, _ = tx.Exec(ctx, `UPDATE payment_intents SET status='failed', updated_at=NOW() WHERE provider_order_id=$1`, fail.OrderID)
			return nil
		})
	}
	if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

func (h *Handlers) handleRefundWebhook(c *gin.Context, ref cashfree.RefundWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.GatewayPaymentRepo == nil {
		c.Status(http.StatusOK)
		return
	}

	// Step 1: Unlocked ancestry resolution
	existingRef, _ := h.GatewayPaymentRepo.GetRefundByCFRefundID(ctx, ref.CFRefundID)
	var p *domain.Payment
	if existingRef != nil {
		p, _ = h.GatewayPaymentRepo.GetByID(ctx, existingRef.PaymentID)
	} else if ref.CFPaymentID != "" {
		p, _ = h.GatewayPaymentRepo.GetByCFPaymentID(ctx, ref.CFPaymentID)
	}

	if p == nil {
		_ = h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, ref.OrderID, ref.CFPaymentID, nil, ref.RefundAmount, "payment_not_found", evtRecord.RawPayload)
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
		}
		c.Status(http.StatusOK)
		return
	}

	if h.DueStore != nil {
		_, err := h.DueStore.GetByID(ctx, p.DueID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "due lookup failed"})
			return
		}
	}

	// Step 2: Lock hierarchy and forward-only status transition
	if h.Pool != nil {
		err := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			txDueRepo := postgres.NewDueRepo(tx)

			// In-transaction deduplication
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", ref.Type, ref.CFRefundID, ref.RefundStatus)
			if err != nil {
				return err
			}
			if !firstSeen {
				return nil
			}

			// Locks: tenants -> dues -> payments -> gateway_refunds
			var dummy uuid.UUID
			_ = tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, p.TenantID).Scan(&dummy)
			d, err := txDueRepo.GetByIDForUpdate(ctx, p.DueID)
			if err != nil {
				return err
			}
			_ = tx.QueryRow(ctx, `SELECT id FROM payments WHERE id=$1 FOR UPDATE`, p.ID).Scan(&dummy)

			// Forward-only state transition check
			if existingRef != nil && isTerminalRefund(existingRef.Status) && !isTerminalRefund(ref.RefundStatus) {
				return nil // reject regression from terminal status
			}

			source := "system"
			if ref.IsAutoRefund {
				source = "cashfree_auto"
			}
			rfRow := &domain.GatewayRefund{
				PaymentID:       p.ID,
				CFRefundID:      &ref.CFRefundID,
				RefundReference: &ref.RefundID,
				AmountPaise:     ref.RefundAmount,
				Status:          strings.ToLower(ref.RefundStatus),
				Source:          source,
				Reason:          ref.RefundReason,
				RawPayload:      evtRecord.RawPayload,
			}
			if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
				return err
			}

			// Financial ledger reversal upon successful refund
			if strings.EqualFold(ref.RefundStatus, "SUCCESS") {
				now := time.Now().UTC()
				if p.IsUnapplied {
					if h.Finance != nil {
						_ = h.Finance.MirrorRefund(ctx, d.PropertyID, rfRow.ID, ref.RefundAmount, true, d.Kind, now)
					}
				} else {
					_ = txPayRepo.CreateRefundAllocation(ctx, &domain.RefundAllocation{
						RefundID:    rfRow.ID,
						DueID:       &d.ID,
						AmountPaise: ref.RefundAmount,
					})
					_ = recomputeDueStatusUnderLock(ctx, txDueRepo, txPayRepo, d)
					if h.Finance != nil {
						_ = h.Finance.MirrorRefund(ctx, d.PropertyID, rfRow.ID, ref.RefundAmount, false, d.Kind, now)
					}
				}
			}
			return nil
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "refund settle failed"})
			return
		}
	} else if h.GatewayPaymentRepo != nil {
		source := "system"
		if ref.IsAutoRefund {
			source = "cashfree_auto"
		}
		rfRow := &domain.GatewayRefund{
			PaymentID:       p.ID,
			CFRefundID:      &ref.CFRefundID,
			RefundReference: &ref.RefundID,
			AmountPaise:     ref.RefundAmount,
			Status:          strings.ToUpper(strings.TrimSpace(ref.RefundStatus)),
			Source:          source,
			Reason:          ref.RefundReason,
			RawPayload:      evtRecord.RawPayload,
		}
		_ = h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow)
	}

	if evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

func isTerminalRefund(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s == "success" || s == "succeeded" || s == "failed" || s == "cancelled"
}

func RecomputeDueStatusMath(netPaid int64, contractualCeiling int) (domain.DueStatus, int) {
	return domain.RecomputeDueStatusMath(netPaid, contractualCeiling)
}

func recomputeDueStatusUnderLock(ctx context.Context, txDueRepo *postgres.DueRepo, txPayRepo *postgres.PaymentRepo, d *domain.Due) error {
	netPaid, err := txPayRepo.GetDueNetPaidPaise(ctx, d.ID)
	if err != nil {
		return err
	}
	// Contractual ceiling: If due was prorated at departure, ContractualCeilingPaise survives
	// even when d.Amount drops to 0 on full settlement.
	ceiling := d.OriginalAmount
	if d.ContractualCeilingPaise != nil && *d.ContractualCeilingPaise > 0 {
		ceiling = *d.ContractualCeilingPaise
	}

	newStatus, newAmount := RecomputeDueStatusMath(netPaid, ceiling)
	d.Status = newStatus
	d.Amount = newAmount
	if d.Status != domain.DueStatusPaid {
		d.PaidAt = nil
	}
	return txDueRepo.Update(ctx, d)
}

type OwnerRefundRequest struct {
	AmountPaise int64  `json:"amount_paise" binding:"required,gt=0"`
	Reason      string `json:"reason" binding:"required"`
}

type dueAllocInfo struct {
	dueID          uuid.UUID
	dueDate        time.Time
	kind           domain.DueKind
	originalAmount int
	amount         int
	status         domain.DueStatus
	allocatedPaise int64
}

type refundValidationErr struct {
	msg string
}

func (e *refundValidationErr) Error() string { return e.msg }

// OwnerRefundPayment handles POST /owner/payments/:id/refund.
func (h *Handlers) OwnerRefundPayment(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	paymentID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req OwnerRefundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: amount_paise must be positive and reason required"})
		return
	}
	ctx := c.Request.Context()

	if h.GatewayPaymentRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payment repository unavailable"})
		return
	}
	if h.CashfreeClient == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cashfree client unavailable"})
		return
	}

	// Unlocked Read 1: Resolve payment
	p, err := h.GatewayPaymentRepo.GetByID(ctx, paymentID)
	if err != nil || p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}

	// Verify tenant belongs to property
	tenant, err := h.TenantStore.GetByID(ctx, p.TenantID)
	if err != nil || tenant == nil || tenant.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found for property"})
		return
	}

	// Gateway check: Only cashfree provider payments can be refunded via PG API
	if p.Provider != "cashfree" && p.MatchedBy != domain.MatchedByCashfree {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only cashfree gateway payments can be refunded via gateway"})
		return
	}

	// 180-day eligibility window check
	now := time.Now().UTC()
	paymentTime := p.CreatedAt
	if now.Sub(paymentTime) > 180*24*time.Hour {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payment is older than 180 days and cannot be refunded via gateway"})
		return
	}

	// Idempotency key handling
	idempotencyKey := strings.TrimSpace(c.GetHeader("X-Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = uuid.New().String()
	}

	// Unlocked Read 2: Check if already initiated/completed with this idempotency key
	existingRef, err := h.GatewayPaymentRepo.GetRefundByPaymentAndIdempotency(ctx, p.ID, idempotencyKey)
	if err == nil && existingRef != nil {
		c.JSON(http.StatusOK, existingRef)
		return
	}

	// Resolve Cashfree Order ID
	var orderID string
	if p.CFPaymentID != nil && *p.CFPaymentID != "" && h.IntentStore != nil {
		if intent, err := h.IntentStore.GetByCFPaymentID(ctx, *p.CFPaymentID); err == nil && intent != nil {
			orderID = intent.ProviderOrderID
		}
	}
	if orderID == "" && p.ProviderPaymentID != nil && *p.ProviderPaymentID != "" && h.IntentStore != nil {
		if intent, err := h.IntentStore.GetByCFPaymentID(ctx, *p.ProviderPaymentID); err == nil && intent != nil {
			orderID = intent.ProviderOrderID
		}
	}
	if orderID == "" && p.DueID != uuid.Nil && h.DueStore != nil {
		if d, err := h.DueStore.GetByID(ctx, p.DueID); err == nil && d != nil {
			orderID = fmt.Sprintf("order_%s", d.DueCode)
		}
	}
	if orderID == "" {
		orderID = fmt.Sprintf("order_%s", p.ID.String()[:8])
	}

	// Deterministic 35-character refund reference: rf_<32_char_hex_uuid>
	cleanUUID := strings.ReplaceAll(uuid.New().String(), "-", "")
	refundRef := fmt.Sprintf("rf_%s", cleanUUID)

	rfRow := &domain.GatewayRefund{
		ID:              uuid.New(),
		PaymentID:       p.ID,
		PropertyID:      pid,
		Provider:        "cashfree",
		RefundReference: &refundRef,
		IdempotencyKey:  &idempotencyKey,
		AmountPaise:     req.AmountPaise,
		Status:          "initiated",
		Source:          "owner",
		InitiatedBy:     &uid,
		Reason:          req.Reason,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	type refundAllocItem struct {
		dueID       uuid.UUID
		amountPaise int64
		dueKind     domain.DueKind
	}
	var allocationsToCreate []refundAllocItem

	// Step B: Atomic Row Lock & Allocation Preparation (Universal Lock Hierarchy)
	if h.Pool != nil {
		err := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)

			// 1. Lock Tenant (1)
			var dummy uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, p.TenantID).Scan(&dummy); err != nil {
				return fmt.Errorf("lock tenant: %w", err)
			}

			// Gather dues linked to this payment via payment_allocations
			rows, err := tx.Query(ctx, `
				SELECT pa.due_id, pa.amount_paise, d.due_date, d.kind, d.original_amount, d.amount, d.status
				FROM payment_allocations pa
				JOIN dues d ON d.id = pa.due_id
				WHERE pa.payment_id = $1
				ORDER BY d.due_date ASC, d.id ASC`, p.ID)
			if err != nil {
				return fmt.Errorf("query payment allocations: %w", err)
			}
			defer rows.Close()

			var duesList []dueAllocInfo
			for rows.Next() {
				var info dueAllocInfo
				if err := rows.Scan(&info.dueID, &info.allocatedPaise, &info.dueDate, &info.kind, &info.originalAmount, &info.amount, &info.status); err != nil {
					return err
				}
				duesList = append(duesList, info)
			}
			rows.Close()

			// 3. Lock Dues (3) in topological sort order: ORDER BY due_date ASC, id ASC
			if len(duesList) > 0 {
				dueIDs := make([]uuid.UUID, len(duesList))
				for i, d := range duesList {
					dueIDs[i] = d.dueID
				}
				_, err = tx.Exec(ctx, `
					SELECT id FROM dues 
					WHERE id = ANY($1) 
					ORDER BY due_date ASC, id ASC FOR UPDATE`, dueIDs)
				if err != nil {
					return fmt.Errorf("lock dues: %w", err)
				}
			}

			// 6. Lock Payment (6)
			var paymentAmt int
			var isUnapplied bool
			if err := tx.QueryRow(ctx, `
				SELECT amount, is_unapplied 
				FROM payments 
				WHERE id = $1 FOR UPDATE`, p.ID).Scan(&paymentAmt, &isUnapplied); err != nil {
				return fmt.Errorf("lock payment: %w", err)
			}

			// 7. Lock Payment Allocations (7)
			_, err = tx.Exec(ctx, `
				SELECT id FROM payment_allocations 
				WHERE payment_id = $1 
				ORDER BY due_id ASC, id ASC FOR UPDATE`, p.ID)
			if err != nil {
				return fmt.Errorf("lock payment allocations: %w", err)
			}

			// 8. Lock Gateway Refunds (8)
			_, err = tx.Exec(ctx, `
				SELECT id FROM gateway_refunds 
				WHERE payment_id = $1 
				ORDER BY created_at ASC, id ASC FOR UPDATE`, p.ID)
			if err != nil {
				return fmt.Errorf("lock gateway refunds: %w", err)
			}

			// Guard A: Settlement Terminal Boundary Guard
			// Reject refunds if the tenant's departure is already approved/refunded,
			// or if any linked dues have departure_due_adjustments.
			var settledDepartureExists bool
			err = tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM tenant_departures 
					WHERE tenant_id = $1 AND status IN ('approved', 'refunded')
				)`, p.TenantID).Scan(&settledDepartureExists)
			if err != nil {
				return fmt.Errorf("check settled departure: %w", err)
			}
			if settledDepartureExists {
				return &refundValidationErr{msg: "Cannot issue gateway refund against payment tied to a settled departure. Deposit and rent adjustments must be resolved via departure disbursement/payout ledger."}
			}

			checkDueIDs := make([]uuid.UUID, 0, len(duesList)+1)
			for _, d := range duesList {
				checkDueIDs = append(checkDueIDs, d.dueID)
			}
			if p.DueID != uuid.Nil {
				checkDueIDs = append(checkDueIDs, p.DueID)
			}
			if len(checkDueIDs) > 0 {
				var adjExists bool
				err = tx.QueryRow(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM departure_due_adjustments 
						WHERE due_id = ANY($1)
					)`, checkDueIDs).Scan(&adjExists)
				if err != nil {
					return fmt.Errorf("check departure due adjustments: %w", err)
				}
				if adjExists {
					return &refundValidationErr{msg: "Cannot issue gateway refund against payment tied to a settled departure. Deposit and rent adjustments must be resolved via departure disbursement/payout ledger."}
				}
			}

			// Validate remaining refundable balance under lock
			alreadyRefunded, err := txPayRepo.GetPaymentRefundedPaise(ctx, p.ID)
			if err != nil {
				return fmt.Errorf("get refunded paise: %w", err)
			}
			if alreadyRefunded+req.AmountPaise > int64(paymentAmt) {
				return &refundValidationErr{msg: fmt.Sprintf("refund amount %d paise exceeds remaining balance %d paise", req.AmountPaise, int64(paymentAmt)-alreadyRefunded)}
			}

			// Calculate LIFO refund allocations across dues (due_date DESC)
			if !isUnapplied && len(duesList) > 0 {
				remRefund := req.AmountPaise
				for i := len(duesList) - 1; i >= 0 && remRefund > 0; i-- {
					d := duesList[i]
					netPaid, err := txPayRepo.GetDueNetPaidPaise(ctx, d.dueID)
					if err != nil {
						return fmt.Errorf("get due net paid: %w", err)
					}
					maxAlloc := d.allocatedPaise
					if netPaid < maxAlloc {
						maxAlloc = netPaid
					}
					if maxAlloc <= 0 {
						continue
					}
					allocAmt := remRefund
					if allocAmt > maxAlloc {
						allocAmt = maxAlloc
					}
					allocationsToCreate = append(allocationsToCreate, refundAllocItem{
						dueID:       d.dueID,
						amountPaise: allocAmt,
						dueKind:     d.kind,
					})
					remRefund -= allocAmt
				}
			}

			// Pre-insert gateway_refunds in status 'initiated'
			if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
				return fmt.Errorf("create initiated refund: %w", err)
			}

			// Pre-insert refund_allocations
			for _, item := range allocationsToCreate {
				dueIDCopy := item.dueID
				if err := txPayRepo.CreateRefundAllocation(ctx, &domain.RefundAllocation{
					RefundID:    rfRow.ID,
					DueID:       &dueIDCopy,
					AmountPaise: item.amountPaise,
				}); err != nil {
					return fmt.Errorf("create refund allocation: %w", err)
				}
			}

			return nil
		})
		if err != nil {
			var vErr *refundValidationErr
			if errors.As(err, &vErr) {
				c.JSON(http.StatusBadRequest, gin.H{"error": vErr.msg})
				return
			}
			respondErr(c, err)
			return
		}
	} else {
		// Mock fallback when Pool is nil
		alreadyRefunded, _ := h.GatewayPaymentRepo.GetPaymentRefundedPaise(ctx, p.ID)
		if alreadyRefunded+req.AmountPaise > int64(p.Amount) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "refund amount exceeds remaining balance"})
			return
		}
		_ = h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow)
	}

	// Step C: External Gateway Call (ZERO row locks held during external I/O)
	cfResp, cfErr := h.CashfreeClient.CreateRefund(ctx, orderID, refundRef, req.AmountPaise, req.Reason, idempotencyKey)
	if cfErr != nil {
		slog.Error("cashfree create refund error", "error", cfErr, "payment_id", p.ID, "refund_id", rfRow.ID)
		rfRow.Status = "failed"
		if h.Pool != nil {
			_ = postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
				txPayRepo := postgres.NewPaymentRepo(tx)
				_ = txPayRepo.CreateOrUpdateRefund(ctx, rfRow)
				_, _ = tx.Exec(ctx, `DELETE FROM refund_allocations WHERE refund_id = $1`, rfRow.ID)
				return nil
			})
		} else {
			_ = h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow)
		}
		respondErr(c, clientErr(http.StatusBadGateway, "gateway refund failed"))
		return
	}

	// Update status and provider IDs from gateway response
	respStatus := strings.ToLower(strings.TrimSpace(cfResp.RefundStatus))
	if respStatus == "success" {
		respStatus = "succeeded"
	}
	rfRow.Status = respStatus
	rfRow.CFRefundID = &cfResp.CFRefundID
	rfRow.ProviderRefundID = &cfResp.CFRefundID

	// Settle or finalize in DB
	if h.Pool != nil {
		err := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			txDueRepo := postgres.NewDueRepo(tx)

			if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
				return err
			}

			// If succeeded, recompute dues
			if rfRow.Status == "succeeded" {
				for _, item := range allocationsToCreate {
					d, err := txDueRepo.GetByIDForUpdate(ctx, item.dueID)
					if err == nil && d != nil {
						_ = recomputeDueStatusUnderLock(ctx, txDueRepo, txPayRepo, d)
					}
				}
			} else if rfRow.Status == "failed" || rfRow.Status == "cancelled" {
				_, _ = tx.Exec(ctx, `DELETE FROM refund_allocations WHERE refund_id = $1`, rfRow.ID)
			}
			return nil
		})
		if err != nil {
			slog.Error("refund post-settlement failed", "error", err, "refund_id", rfRow.ID)
		}
	} else {
		_ = h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow)
	}

	// Post financial ledger reversal outside row locks
	if rfRow.Status == "succeeded" && h.Finance != nil {
		finTime := time.Now().UTC()
		if p.IsUnapplied {
			_ = h.Finance.MirrorRefund(ctx, pid, rfRow.ID, req.AmountPaise, true, domain.DueKindRent, finTime)
		} else {
			for _, item := range allocationsToCreate {
				_ = h.Finance.MirrorRefund(ctx, pid, rfRow.ID, item.amountPaise, false, item.dueKind, finTime)
			}
		}
	}

	c.JSON(http.StatusOK, rfRow)
}

