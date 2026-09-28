package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type mockBankTxnStore struct {
	items    map[string]*domain.BankTransaction
	failErr  error
	inserted int
}

func newMockBankTxnStore() *mockBankTxnStore {
	return &mockBankTxnStore{
		items: make(map[string]*domain.BankTransaction),
	}
}

func (m *mockBankTxnStore) InsertTransaction(_ context.Context, _ pgx.Tx, txn *domain.BankTransaction) (bool, error) {
	if m.failErr != nil {
		return false, m.failErr
	}
	if txn.ID == uuid.Nil {
		txn.ID = uuid.New()
	}
	if txn.DedupHash == "" {
		txn.DedupHash = domain.ComputeBankTxnDedupHash(
			txn.PropertyID,
			txn.TxnDate,
			txn.AmountPaise,
			txn.RowType,
			txn.TxnID,
			txn.ClosingBalancePaise,
			txn.OccurrenceIndex,
		)
	}
	key := txn.PropertyID.String() + "|" + txn.DedupHash
	if _, exists := m.items[key]; exists {
		return false, nil // Duplicate skipped
	}
	m.items[key] = txn
	m.inserted++
	return true, nil
}

func (m *mockBankTxnStore) GetByID(_ context.Context, id uuid.UUID) (*domain.BankTransaction, error) {
	for _, item := range m.items {
		if item.ID == id {
			return item, nil
		}
	}
	return nil, errors.New("not found")
}

func (m *mockBankTxnStore) GetByPropertyAndID(_ context.Context, propertyID, id uuid.UUID) (*domain.BankTransaction, error) {
	for _, item := range m.items {
		if item.PropertyID == propertyID && item.ID == id {
			return item, nil
		}
	}
	return nil, errors.New("not found")
}

func (m *mockBankTxnStore) ListByProperty(_ context.Context, propertyID uuid.UUID, _ domain.BankTransactionFilter) ([]*domain.BankTransaction, int, error) {
	var list []*domain.BankTransaction
	for _, item := range m.items {
		if item.PropertyID == propertyID {
			list = append(list, item)
		}
	}
	return list, len(list), nil
}

func (m *mockBankTxnStore) UpdateStatus(_ context.Context, _ pgx.Tx, id uuid.UUID, status domain.BankTransactionStatus, matchedDueID *uuid.UUID, matchedBy *uuid.UUID, matchedAt *time.Time, journalEntryID *uuid.UUID) error {
	for _, item := range m.items {
		if item.ID == id {
			item.Status = status
			item.MatchedDueID = matchedDueID
			item.MatchedBy = matchedBy
			item.MatchedAt = matchedAt
			item.JournalEntryID = journalEntryID
			return nil
		}
	}
	return nil
}

type mockCSVPaymentService struct {
	suggestResult *payment.MatchResult
	suggestErr    error
}

func (m *mockCSVPaymentService) MatchPayment(context.Context, uuid.UUID, string, int, time.Time, string) (*domain.Payment, error) {
	return nil, errors.New("auto-settlement gated off")
}

func (m *mockCSVPaymentService) SuggestMatch(context.Context, uuid.UUID, int, time.Time, string) (*payment.MatchResult, error) {
	return m.suggestResult, m.suggestErr
}

func (m *mockCSVPaymentService) ManualMatch(context.Context, uuid.UUID, int, string, uuid.UUID) (*domain.Payment, error) {
	return nil, nil
}
func (m *mockCSVPaymentService) MarkCashPaid(context.Context, uuid.UUID, int, uuid.UUID, string) (*domain.Payment, error) {
	return nil, nil
}
func (m *mockCSVPaymentService) SettleDeposit(context.Context, uuid.UUID, int64, string) error {
	return nil
}
func (m *mockCSVPaymentService) BuildSummary(context.Context, uuid.UUID, string) (*payment.ReconciliationSummary, error) {
	return nil, nil
}
func (m *mockCSVPaymentService) GatewaySettle(context.Context, uuid.UUID, int, string, ...string) (*domain.Payment, error) {
	return nil, nil
}

type mockCSVImportStore struct {
	logs []*postgres.ImportLog
}

func (m *mockCSVImportStore) Create(_ context.Context, l *postgres.ImportLog) error {
	m.logs = append(m.logs, l)
	return nil
}

func createMultipartRequest(url, fieldName, fileName string, fileContent []byte) (*http.Request, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile(fieldName, fileName)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(fileContent); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	req := httptest.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, nil
}

func TestImportStatements_HDFC_AssumedLayoutFixture(t *testing.T) {
	gin.SetMode(gin.TestMode)

	propID := uuid.New()
	userID := uuid.New()
	bankStore := newMockBankTxnStore()
	memFinStore := finance.NewMemoryStore()
	finSvc := finance.NewService(memFinStore, nil)
	importStore := &mockCSVImportStore{}

	// Setup candidate match for Ramesh Kumar
	dueID := uuid.New()
	paySvc := &mockCSVPaymentService{
		suggestResult: &payment.MatchResult{
			Due: &domain.Due{
				ID:         dueID,
				PropertyID: propID,
				Amount:     1500000,
			},
			IsDeterministic: true,
			MatchedBy:       domain.MatchedByDueCode,
		},
	}

	h := &Handlers{
		Deps: Deps{
			BankTxnRepo:    bankStore,
			Finance:        finSvc,
			FinanceEnabled: true,
			Payments:       paySvc,
			ImportStore:    importStore,
		},
	}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     userID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
	})
	r.POST("/owner/statements/import", h.ImportStatements)

	hdfcCSV := "Date,Narration,Chq./Ref.No.,Value Dt,Withdrawal Amt.,Deposit Amt.,Closing Balance\n" +
		"01/09/26,UPI-RAMESH KUMAR-424512345678-HDFC0001234-PG-A3X9KR RENT,424512345678,01/09/26,,15000.00,125000.50\n" +
		"02/09/26,UPI-SURESH PATEL-424698765432-PYTM0123456-ROOM 204,424698765432,02/09/26,,12000.00,137000.50\n" +
		"03/09/26,NEFT DR-MAINTENANCE EXPENSE-N09261234567,N09261234567,03/09/26,3500.00,,133500.50\n"

	req, err := createMultipartRequest("/owner/statements/import", "file", "hdfc_statement.csv", []byte(hdfcCSV))
	if err != nil {
		t.Fatalf("failed creating multipart request: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		RowCount   int `json:"row_count"`
		Matched    int `json:"matched"`
		Suggested  int `json:"suggested"`
		Unmatched  int `json:"unmatched"`
		Duplicates int `json:"duplicates"`
		Debits     int `json:"debits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed unmarshaling response: %v", err)
	}

	if resp.RowCount != 3 {
		t.Errorf("expected 3 rows, got %d", resp.RowCount)
	}
	if resp.Debits != 1 {
		t.Errorf("expected 1 debit row, got %d", resp.Debits)
	}
	if resp.Suggested != 2 {
		t.Errorf("expected 2 suggested matches, got %d", resp.Suggested)
	}
	if resp.Duplicates != 0 {
		t.Errorf("expected 0 duplicates on first upload, got %d", resp.Duplicates)
	}

	// Invariant Check 1: Double-entry ledger quarantine
	// The 2 credit rows (₹15,000 and ₹12,000 = 2,700,000 paise) must be quarantined: Dr bank / Cr unapplied_receipts
	lines, err := memFinStore.ListJournal(context.Background(), propID, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("failed listing journal lines: %v", err)
	}

	var bankDebits, unappliedCredits int64
	for _, l := range lines {
		if l.AccountCode == domain.AcctBank {
			bankDebits += l.DebitPaise
		}
		if l.AccountCode == domain.AcctUnappliedReceipts {
			unappliedCredits += l.CreditPaise
		}
	}
	expectedCreditPaise := int64(1500000 + 1200000)
	if bankDebits != expectedCreditPaise || unappliedCredits != expectedCreditPaise {
		t.Fatalf("quarantine ledger mismatch: Dr bank=%d, Cr unapplied=%d, expected=%d", bankDebits, unappliedCredits, expectedCreditPaise)
	}

	// Invariant Check 2: Debit row (₹3,500) stored as ignored_debit with closing balance, never credited to ledger
	var foundDebit bool
	for _, item := range bankStore.items {
		if item.RowType == "debit" {
			foundDebit = true
			if item.Status != domain.BankTxnIgnoredDebit {
				t.Errorf("debit row status must be ignored_debit, got %s", item.Status)
			}
			if item.ClosingBalancePaise == nil || *item.ClosingBalancePaise != 13350050 {
				t.Errorf("debit row closing balance missing or incorrect: %v", item.ClosingBalancePaise)
			}
		}
	}
	if !foundDebit {
		t.Fatalf("debit row was not stored in bank_transactions")
	}

	// Invariant Check 3: Explicit duplicate skipping on re-upload
	req2, _ := createMultipartRequest("/owner/statements/import", "file", "hdfc_statement.csv", []byte(hdfcCSV))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("re-upload expected HTTP 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var resp2 struct {
		RowCount   int `json:"row_count"`
		Duplicates int `json:"duplicates"`
		Suggested  int `json:"suggested"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2.Duplicates != 3 {
		t.Fatalf("expected 3 skipped duplicates on re-upload, got %d", resp2.Duplicates)
	}

	// Verify no additional journal lines posted on re-upload
	linesAfterReupload, _ := memFinStore.ListJournal(context.Background(), propID, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if len(linesAfterReupload) != len(lines) {
		t.Fatalf("re-upload posted duplicate journal entries! before=%d, after=%d", len(lines), len(linesAfterReupload))
	}
}

func TestImportStatements_FailFast_OnDatabaseError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	propID := uuid.New()
	userID := uuid.New()
	bankStore := newMockBankTxnStore()
	bankStore.failErr = errors.New("connection reset by peer") // Injected infra failure

	h := &Handlers{
		Deps: Deps{
			BankTxnRepo: bankStore,
		},
	}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     userID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
	})
	r.POST("/owner/statements/import", h.ImportStatements)

	csvContent := "Date,Narration,Chq./Ref.No.,Value Dt,Withdrawal Amt.,Deposit Amt.,Closing Balance\n" +
		"01/09/26,UPI-RAMESH KUMAR,424512345678,01/09/26,,15000.00,125000.50\n"

	req, _ := createMultipartRequest("/owner/statements/import", "file", "test.csv", []byte(csvContent))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Must fail-fast with HTTP 500 immediately
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 on infrastructure error, got %d", w.Code)
	}
}

func TestImportStatements_ICICI_AssumedLayoutFixture(t *testing.T) {
	gin.SetMode(gin.TestMode)

	propID := uuid.New()
	userID := uuid.New()
	bankStore := newMockBankTxnStore()
	importStore := &mockCSVImportStore{}

	h := &Handlers{
		Deps: Deps{
			BankTxnRepo: bankStore,
			Payments:    &mockCSVPaymentService{},
			ImportStore: importStore,
		},
	}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     userID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
	})
	r.POST("/owner/statements/import", h.ImportStatements)

	iciciCSV := "Transaction Date,Value Date,Cheque No.,Transaction Remarks,Withdrawal Amount (INR ),Deposit Amount (INR ),Balance (INR )\n" +
		"01/09/2026,01/09/2026,-,UPI/424512345678/Rent Room 102/HDFC/ramesh@upi,0.00,15000.00,150000.00\n" +
		"03/09/2026,03/09/2026,-,ATM/CASH-WDL/MUMBAI,2000.00,0.00,148000.00\n"

	req, _ := createMultipartRequest("/owner/statements/import", "file", "icici.csv", []byte(iciciCSV))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		RowCount  int `json:"row_count"`
		Debits    int `json:"debits"`
		Unmatched int `json:"unmatched"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.RowCount != 2 || resp.Debits != 1 || resp.Unmatched != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

type failingFinanceStore struct {
	*finance.MemoryStore
	failJournal bool
}

func (f *failingFinanceStore) InsertJournal(ctx context.Context, lines []domain.JournalLine) error {
	if f.failJournal {
		return errors.New("simulated transient ledger database error")
	}
	return f.MemoryStore.InsertJournal(ctx, lines)
}

func TestImportStatements_MirrorFailsFirst_ThenRetryConverges(t *testing.T) {
	gin.SetMode(gin.TestMode)

	propID := uuid.New()
	userID := uuid.New()
	bankStore := newMockBankTxnStore()
	memFinStore := finance.NewMemoryStore()
	failingStore := &failingFinanceStore{
		MemoryStore: memFinStore,
		failJournal: true, // Step 1: Force ledger mirror failure
	}
	finSvc := finance.NewService(failingStore, nil)

	h := &Handlers{
		Deps: Deps{
			BankTxnRepo:    bankStore,
			Finance:        finSvc,
			FinanceEnabled: true,
			Payments:       &mockCSVPaymentService{},
			ImportStore:    &mockCSVImportStore{},
		},
	}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     userID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
	})
	r.POST("/owner/statements/import", h.ImportStatements)

	csvContent := "Date,Narration,Chq./Ref.No.,Value Dt,Withdrawal Amt.,Deposit Amt.,Closing Balance\n" +
		"01/09/26,UPI-RAMESH KUMAR,424512345678,01/09/26,,15000.00,125000.50\n"

	// Attempt 1: Ledger fails
	req1, _ := createMultipartRequest("/owner/statements/import", "file", "statement.csv", []byte(csvContent))
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	if w1.Code != http.StatusInternalServerError {
		t.Fatalf("attempt 1 expected HTTP 500, got %d", w1.Code)
	}

	// Verify bank_transactions row was NOT committed before the ledger failure!
	if bankStore.inserted != 0 {
		t.Fatalf("bank transaction was committed before ledger succeeded! inserted=%d", bankStore.inserted)
	}

	// Attempt 2: Ledger issue resolved -> Retry must succeed and post exactly 1 journal entry
	failingStore.failJournal = false
	req2, _ := createMultipartRequest("/owner/statements/import", "file", "statement.csv", []byte(csvContent))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("attempt 2 retry expected HTTP 200, got %d: %s", w2.Code, w2.Body.String())
	}
	if bankStore.inserted != 1 {
		t.Fatalf("expected 1 bank transaction inserted on retry, got %d", bankStore.inserted)
	}

	lines, err := memFinStore.ListJournal(context.Background(), propID, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("failed listing journal: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 journal lines (1 entry) after retry, got %d", len(lines))
	}

	// Attempt 3: Immediate re-import of same file -> Duplicates skipped, exactly 2 journal lines remain
	req3, _ := createMultipartRequest("/owner/statements/import", "file", "statement.csv", []byte(csvContent))
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)

	if w3.Code != http.StatusOK {
		t.Fatalf("attempt 3 re-import expected HTTP 200, got %d", w3.Code)
	}
	var resp3 struct {
		Duplicates int `json:"duplicates"`
	}
	_ = json.Unmarshal(w3.Body.Bytes(), &resp3)
	if resp3.Duplicates != 1 {
		t.Fatalf("expected 1 skipped duplicate on attempt 3, got %d", resp3.Duplicates)
	}

	linesAfterThird, _ := memFinStore.ListJournal(context.Background(), propID, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if len(linesAfterThird) != 2 {
		t.Fatalf("re-import generated duplicate journal entries! lines=%d", len(linesAfterThird))
	}
}

func TestImportStatements_SBI_PreambleMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	propID := uuid.New()
	userID := uuid.New()
	bankStore := newMockBankTxnStore()
	memFinStore := finance.NewMemoryStore()
	finSvc := finance.NewService(memFinStore, nil)

	h := &Handlers{
		Deps: Deps{
			BankTxnRepo:    bankStore,
			Finance:        finSvc,
			FinanceEnabled: true,
			Payments:       &mockCSVPaymentService{},
			ImportStore:    &mockCSVImportStore{},
		},
	}

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     userID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
	})
	r.POST("/owner/statements/import", h.ImportStatements)

	sbiCSV := "Account Name: Ramesh Kumar\n" +
		"Account Number: 00000012345678901\n" +
		"Branch: KORAMANGALA BANGALORE\n" +
		"(Amounts in INR)\n" +
		"\n" +
		"Txn Date,Value Date,Description,Ref No./Cheque No.,Debit,Credit,Balance\n" +
		"01/09/2026,01/09/2026,TRANSFER FROM RAMESH KUMAR - RENT,TRANSFER424512,,15000.00,85000.00\n" +
		"02/09/2026,02/09/2026,ATM WDL-KORAMANGALA,ATM998877,2000.00,,83000.00\n"

	req, _ := createMultipartRequest("/owner/statements/import", "file", "sbi.csv", []byte(sbiCSV))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		RowCount   int `json:"row_count"`
		Debits     int `json:"debits"`
		Unmatched  int `json:"unmatched"`
		Duplicates int `json:"duplicates"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.RowCount != 2 || resp.Debits != 1 || resp.Unmatched != 1 {
		t.Fatalf("unexpected response from SBI preamble: %+v", resp)
	}

	// Verify quarantine Dr bank / Cr unapplied_receipts for 15,000 INR
	lines, err := memFinStore.ListJournal(context.Background(), propID, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("failed listing journal: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 journal lines, got %d", len(lines))
	}
	if lines[0].DebitPaise != 1500000 && lines[1].DebitPaise != 1500000 {
		t.Fatalf("expected 1,500,000 paise debit, got %v", lines)
	}
}

