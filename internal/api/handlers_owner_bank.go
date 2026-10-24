package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/csv"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment/bankimport"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

const maxCSVBytes = 5 << 20

// ImportStatements handles POST /owner/statements/import.
func (h *Handlers) ImportStatements(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	file, hdr, err := c.Request.FormFile("file")
	if err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "file required"))
		return
	}
	defer file.Close()
	if hdr.Size > maxCSVBytes {
		respondErr(c, clientErr(http.StatusBadRequest, "file too large (max 5MB)"))
		return
	}
	limited := io.LimitReader(file, maxCSVBytes+1)
	parseRes, err := csv.ParseWithMeta(limited)
	if err != nil {
		if errors.Is(err, csv.ErrUnknownSchema) {
			respondErr(c, clientErr(http.StatusBadRequest, "unknown CSV schema — required columns: date, note, and amount (or deposit/withdrawal)"))
			return
		}
		respondErr(c, clientErr(http.StatusBadRequest, "csv parse error: invalid format or syntax"))
		return
	}
	rows := parseRes.Rows

	var bankAccountID *uuid.UUID
	if acctStr := strings.TrimSpace(c.Request.FormValue("bank_account_id")); acctStr != "" {
		parsedID, err := uuid.Parse(acctStr)
		if err != nil {
			respondErr(c, clientErr(http.StatusBadRequest, "invalid bank_account_id"))
			return
		}
		if h.BankAccountRepo != nil {
			acct, err := h.BankAccountRepo.GetByID(c.Request.Context(), parsedID)
			if err != nil || acct == nil || acct.PropertyID != pid {
				respondErr(c, clientErr(http.StatusNotFound, "bank account not found"))
				return
			}
		}
		bankAccountID = &parsedID
	}

	var ledger bankimport.Ledger
	if h.Finance != nil {
		ledger = h.Finance
	}
	imported, impErr := bankimport.Import(c.Request.Context(), h.BankTxnRepo, ledger, h.Payments, bankimport.Input{
		PropertyID:     pid,
		BankAccountID:  bankAccountID,
		Rows:           rows,
		FinanceEnabled: h.FinanceEnabled,
	})
	if impErr != nil {
		respondErr(c, clientErr(http.StatusInternalServerError, impErr.Error()))
		return
	}
	matched := imported.Matched
	suggested := imported.Suggested
	unmatched := imported.Unmatched
	duplicates := imported.Duplicates
	debits := imported.Debits

	if (matched + suggested + unmatched + debits) > 0 {
		if h.ImportStore != nil {
			if err := h.ImportStore.Create(c.Request.Context(), &postgres.ImportLog{
				PropertyID: pid,
				Filename:   hdr.Filename,
				RowCount:   len(rows),
				ImportedBy: uid,
			}); err != nil {
				slog.Error("ImportStatements recording import log failed", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record import log"})
				return
			}
		}
	}

	resp := gin.H{
		"row_count":  len(rows),
		"matched":    matched,   // 0 while Tier 1 auto-posting is held off for verification
		"suggested":  suggested, // candidate matches staged for owner confirmation
		"unmatched":  unmatched, // credits requiring manual allocation or refund
		"duplicates": duplicates,
		"debits":     debits,    // tagged outflows stored for statement reconciliation tie-out
		"failed":     unmatched, // backward-compatibility alias
	}
	if parseRes.OpeningBalancePaise != nil {
		resp["opening_balance_paise"] = *parseRes.OpeningBalancePaise
	}
	if len(parseRes.Warnings) > 0 {
		resp["warnings"] = parseRes.Warnings
	}
	c.JSON(http.StatusOK, resp)
}

type ClassifyBankTransactionReq struct {
	Classification string `json:"classification" binding:"required"`
}

// ClassifyBankTransaction handles POST /owner/statements/transactions/:id/classify.
func (h *Handlers) ClassifyBankTransaction(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	txnID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req ClassifyBankTransactionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: classification is required"})
		return
	}

	validClassifications := map[string]string{
		"interest_income":   domain.AcctInterestIncome,
		"owner_equity":      domain.AcctOwnerCapital,
		"non_pg_income":     domain.AcctNonPGOtherIncome,
		"reversal":          domain.AcctOperatingExpense,
		"internal_transfer": domain.AcctBank,
	}
	targetAcct, valid := validClassifications[req.Classification]
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid classification; must be interest_income, owner_equity, non_pg_income, reversal, or internal_transfer"})
		return
	}

	if h.BankTxnRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank transaction store not configured"})
		return
	}

	txn, err := h.BankTxnRepo.GetByPropertyAndID(c.Request.Context(), pid, txnID)
	if err != nil {
		if errors.Is(err, postgres.ErrBankTransactionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bank transaction not found"})
			return
		}
		slog.Error("ClassifyBankTransaction retrieving txn failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve transaction"})
		return
	}

	var entryID *uuid.UUID
	if h.Finance != nil && h.FinanceEnabled && txn.RowType == "credit" {
		if err := h.Finance.MirrorUnappliedReclassification(c.Request.Context(), pid, txn.ID, targetAcct, txn.AmountPaise, txn.TxnDate); err != nil {
			slog.Error("ClassifyBankTransaction ledger mirror reclassification failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record financial reclassification"})
			return
		}
	}

	if err := h.BankTxnRepo.Reclassify(c.Request.Context(), nil, txn.ID, req.Classification, entryID); err != nil {
		slog.Error("ClassifyBankTransaction reclassify failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update classification"})
		return
	}

	txn.Classification = req.Classification
	c.JSON(http.StatusOK, txn)
}

// ListBankTransactions handles GET /owner/statements/transactions.
// Query filters: status, from_date, to_date, limit, offset.
func (h *Handlers) ListBankTransactions(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.BankTxnRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank transaction store not configured"})
		return
	}

	filter := domain.BankTransactionFilter{}
	if st := strings.TrimSpace(c.Query("status")); st != "" {
		status := domain.BankTransactionStatus(st)
		switch status {
		case domain.BankTxnMatched, domain.BankTxnSuggestedMatch, domain.BankTxnUnmatched, domain.BankTxnRefunded, domain.BankTxnIgnoredDebit:
			filter.Status = &status
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status filter"})
			return
		}
	}
	if fd := strings.TrimSpace(c.Query("from_date")); fd != "" {
		if t, err := time.Parse("2006-01-02", fd); err == nil {
			filter.FromDate = &t
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from_date format; use YYYY-MM-DD"})
			return
		}
	}
	if td := strings.TrimSpace(c.Query("to_date")); td != "" {
		if t, err := time.Parse("2006-01-02", td); err == nil {
			filter.ToDate = &t
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to_date format; use YYYY-MM-DD"})
			return
		}
	}
	if lim := strings.TrimSpace(c.Query("limit")); lim != "" {
		if l, err := strconv.Atoi(lim); err == nil && l > 0 {
			if l > 100 {
				l = 100
			}
			filter.Limit = l
		}
	}
	if off := strings.TrimSpace(c.Query("offset")); off != "" {
		if o, err := strconv.Atoi(off); err == nil && o >= 0 {
			filter.Offset = o
		}
	}

	txns, total, err := h.BankTxnRepo.ListByProperty(c.Request.Context(), pid, filter)
	if err != nil {
		slog.Error("ListBankTransactions failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list bank transactions"})
		return
	}
	if txns == nil {
		txns = []*domain.BankTransaction{}
	}

	c.JSON(http.StatusOK, gin.H{
		"transactions": txns,
		"total":        total,
		"limit":        filter.Limit,
		"offset":       filter.Offset,
	})
}

type ConfirmBankTransactionReq struct {
	DueID *uuid.UUID `json:"due_id"`
}

// ConfirmBankTransactionMatch handles POST /owner/statements/transactions/:id/confirm.
func (h *Handlers) ConfirmBankTransactionMatch(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	txnID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.BankTxnRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank transaction store not configured"})
		return
	}

	txn, err := h.BankTxnRepo.GetByPropertyAndID(c.Request.Context(), pid, txnID)
	if err != nil {
		if errors.Is(err, postgres.ErrBankTransactionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bank transaction not found"})
			return
		}
		slog.Error("ConfirmBankTransactionMatch retrieving txn failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve bank transaction"})
		return
	}

	if txn.Status == domain.BankTxnMatched {
		c.JSON(http.StatusOK, gin.H{"transaction": txn, "status": "already_matched"})
		return
	}
	if txn.Status == domain.BankTxnRefunded {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot match refunded bank transaction"})
		return
	}
	if txn.RowType != "credit" || txn.AmountPaise <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only positive credit transactions can be matched to dues"})
		return
	}

	var req ConfirmBankTransactionReq
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}

	targetDueID := req.DueID
	if targetDueID == nil || *targetDueID == uuid.Nil {
		targetDueID = txn.SuggestedDueID
	}
	if targetDueID == nil || *targetDueID == uuid.Nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due_id is required"})
		return
	}

	if h.DueStore == nil || h.Payments == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment service or due store not configured"})
		return
	}

	due, err := h.DueStore.GetByID(c.Request.Context(), *targetDueID)
	if err != nil || due == nil || due.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "due not found"})
		return
	}
	if due.Status == domain.DueStatusPaid || due.Status == domain.DueStatusWaived {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due is already paid or waived"})
		return
	}

	pay, err := h.Payments.ManualMatch(c.Request.Context(), due.ID, txn.AmountPaise, txn.TxnID, uid)
	if err != nil {
		respondErr(c, paymentClientErr(err))
		return
	}

	if h.Finance != nil && h.FinanceEnabled {
		if err := h.Finance.MirrorUnappliedAllocation(c.Request.Context(), pid, txn.ID, due.Kind, txn.AmountPaise, txn.TxnDate); err != nil {
			slog.Error("ConfirmBankTransactionMatch ledger mirror failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record financial allocation"})
			return
		}
	}

	now := time.Now()
	if err := h.BankTxnRepo.UpdateStatus(c.Request.Context(), nil, txn.ID, domain.BankTxnMatched, &due.ID, &uid, &now, nil); err != nil {
		slog.Error("ConfirmBankTransactionMatch updating txn status failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update bank transaction status"})
		return
	}

	txn.Status = domain.BankTxnMatched
	txn.MatchedDueID = &due.ID
	txn.MatchedBy = &uid
	txn.MatchedAt = &now

	c.JSON(http.StatusOK, gin.H{
		"transaction": txn,
		"payment":     pay,
	})
}

// RefundBankTransaction handles POST /owner/statements/transactions/:id/refund.
func (h *Handlers) RefundBankTransaction(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	txnID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.BankTxnRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank transaction store not configured"})
		return
	}

	txn, err := h.BankTxnRepo.GetByPropertyAndID(c.Request.Context(), pid, txnID)
	if err != nil {
		if errors.Is(err, postgres.ErrBankTransactionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bank transaction not found"})
			return
		}
		slog.Error("RefundBankTransaction retrieving txn failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve bank transaction"})
		return
	}

	if txn.Status == domain.BankTxnRefunded {
		c.JSON(http.StatusOK, gin.H{"transaction": txn, "status": "already_refunded"})
		return
	}
	if txn.Status == domain.BankTxnMatched {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot refund matched bank transaction; reverse payment first"})
		return
	}
	if txn.RowType != "credit" || txn.AmountPaise <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only positive credit deposits can be refunded"})
		return
	}

	if h.Finance != nil && h.FinanceEnabled {
		if err := h.Finance.MirrorBankDepositRefund(c.Request.Context(), pid, txn.ID, txn.AmountPaise, txn.TxnDate); err != nil {
			slog.Error("RefundBankTransaction ledger mirror failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record financial refund"})
			return
		}
	}

	now := time.Now()
	if err := h.BankTxnRepo.UpdateStatus(c.Request.Context(), nil, txn.ID, domain.BankTxnRefunded, nil, &uid, &now, nil); err != nil {
		slog.Error("RefundBankTransaction updating txn status failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update bank transaction status"})
		return
	}

	txn.Status = domain.BankTxnRefunded
	txn.MatchedBy = &uid
	txn.MatchedAt = &now

	c.JSON(http.StatusOK, gin.H{"transaction": txn})
}

// ListBankAccounts handles GET /owner/bank-accounts.
func (h *Handlers) ListBankAccounts(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.BankAccountRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank account store not configured"})
		return
	}

	accts, err := h.BankAccountRepo.ListByProperty(c.Request.Context(), pid)
	if err != nil {
		slog.Error("ListBankAccounts failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list bank accounts"})
		return
	}
	if accts == nil {
		accts = []*domain.BankAccount{}
	}

	c.JSON(http.StatusOK, gin.H{"bank_accounts": accts})
}

type CreateBankAccountReq struct {
	BankName           string                 `json:"bank_name" binding:"required"`
	AccountType        domain.BankAccountType `json:"account_type"`
	AccountNumberLast4 string                 `json:"account_number_last4" binding:"required"`
	Label              string                 `json:"label"`
	StatementProfile   string                 `json:"statement_profile"`
}

// CreateBankAccount handles POST /owner/bank-accounts.
func (h *Handlers) CreateBankAccount(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.BankAccountRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank account store not configured"})
		return
	}

	var req CreateBankAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: bank_name and account_number_last4 are required"})
		return
	}

	req.BankName = strings.TrimSpace(req.BankName)
	if req.BankName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bank_name cannot be empty"})
		return
	}

	req.AccountNumberLast4 = strings.TrimSpace(req.AccountNumberLast4)
	if len(req.AccountNumberLast4) != 4 || !isAllDigits(req.AccountNumberLast4) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_number_last4 must be exactly 4 digits"})
		return
	}

	if req.AccountType == "" {
		req.AccountType = domain.BankAccountTypeSavings
	}
	if req.AccountType != domain.BankAccountTypeSavings && req.AccountType != domain.BankAccountTypeCurrent {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_type must be either 'savings' or 'current'"})
		return
	}

	if req.StatementProfile == "" {
		req.StatementProfile = "generic"
	}

	acct := &domain.BankAccount{
		ID:                 uuid.New(),
		PropertyID:         pid,
		BankName:           req.BankName,
		AccountType:        req.AccountType,
		AccountNumberLast4: req.AccountNumberLast4,
		Label:              strings.TrimSpace(req.Label),
		StatementProfile:   strings.TrimSpace(req.StatementProfile),
		IsActive:           true,
	}

	if err := h.BankAccountRepo.Create(c.Request.Context(), acct); err != nil {
		slog.Error("CreateBankAccount failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create bank account"})
		return
	}

	c.JSON(http.StatusCreated, acct)
}

// DeactivateBankAccount handles DELETE /owner/bank-accounts/:id.
func (h *Handlers) DeactivateBankAccount(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	acctID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.BankAccountRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank account store not configured"})
		return
	}

	acct, err := h.BankAccountRepo.GetByID(c.Request.Context(), acctID)
	if err != nil {
		if errors.Is(err, postgres.ErrBankAccountNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bank account not found"})
			return
		}
		slog.Error("DeactivateBankAccount retrieving acct failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve bank account"})
		return
	}

	if acct.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "bank account not found"})
		return
	}

	if err := h.BankAccountRepo.Deactivate(c.Request.Context(), acctID); err != nil {
		slog.Error("DeactivateBankAccount failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to deactivate bank account"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deactivated"})
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ListPayments handles GET /owner/payments (Requirement 11 pagination, max 50).
