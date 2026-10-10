package api

// Feature-level HTTP tests for the finance surface: expenses, approvals, capital,
// budgets and the collections tie-out. These run the real router, auth middleware,
// handlers and finance service against the in-memory store, so each assertion
// exercises the same code path production uses, minus the database.
//
// Role expectations come from CONTRACT.md. Every test names the role it runs as.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

// tieOutPayments supplies only BuildSummary; the tie-out handlers call nothing else.
type tieOutPayments struct {
	PaymentService
}

func (tieOutPayments) BuildSummary(_ context.Context, _ uuid.UUID, _ string) (*payment.ReconciliationSummary, error) {
	return &payment.ReconciliationSummary{}, nil
}

type financeFixture struct {
	t        *testing.T
	secret   string
	store    *finance.MemoryStore
	router   http.Handler
	propA    uuid.UUID
	propB    uuid.UUID
	ownerA   string // bearer token: owner of property A
	managerA string // bearer token: manager of property A
	ownerB   string // bearer token: owner of property B
}

func newFinanceFixture(t *testing.T) *financeFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &financeFixture{t: t, secret: "test-secret-key-with-sufficient-length-32", store: finance.NewMemoryStore()}
	f.propA = uuid.New()
	f.propB = uuid.New()
	issue := func(u *domain.User) string {
		tok, err := auth.IssueToken(f.secret, u)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}
		return tok
	}
	pa, pb := f.propA, f.propB
	f.ownerA = issue(&domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &pa})
	f.managerA = issue(&domain.User{ID: uuid.New(), Role: domain.RoleManager, PropertyID: &pa})
	f.ownerB = issue(&domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &pb})
	f.router = NewRouter(Deps{
		JWTSecret:      f.secret,
		Finance:        finance.NewService(f.store, nil),
		FinanceEnabled: true,
		Payments:       tieOutPayments{},
	})
	return f
}

// call sends one request and returns status and body. A nil body sends none.
func (f *financeFixture) call(method, path, token string, body any, headers map[string]string) (int, string) {
	f.t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("marshal body: %v", err)
		}
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func idemHeader(key string) map[string]string {
	return map[string]string{"Idempotency-Key": key}
}

// errCode extracts the apierr code from a response body, or "" if absent.
func errCode(body string) string {
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(body), &env)
	return env.Code
}

func mustJSON(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

// seedExpense inserts an expense for the given property directly in the store.
func (f *financeFixture) seedExpense(propertyID uuid.UUID, amount int64) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if err := f.store.InsertExpense(context.Background(), &domain.Expense{
		ID:             id,
		PropertyID:     propertyID,
		IdempotencyKey: "seed-" + id.String(),
		AmountPaise:    amount,
		// An approved expense is the state that accepts payments and can be voided.
		Status: domain.ExpenseApproved,
	}); err != nil {
		f.t.Fatalf("seed expense: %v", err)
	}
	return id
}

// ---- Expenses -------------------------------------------------------------

func TestFeatureExpense_OwnerCreateRequiresIdempotencyKey(t *testing.T) {
	f := newFinanceFixture(t)
	body := map[string]any{
		"category_code": "maintenance", "vendor_name": "Plumber Co",
		"description": "tap repair", "amount_paise": 20000,
	}
	code, resp := f.call(http.MethodPost, "/api/owner/finance/expenses", f.ownerA, body, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("expense without Idempotency-Key: status %d, want 400 (body %s)", code, resp)
	}
}

func TestFeatureExpense_OwnerCreateListAndIdempotentReplay(t *testing.T) {
	f := newFinanceFixture(t)
	body := map[string]any{
		"category_code": "maintenance", "vendor_name": "Plumber Co",
		"description": "tap repair", "amount_paise": 20000,
	}
	code, resp := f.call(http.MethodPost, "/api/owner/finance/expenses", f.ownerA, body, idemHeader("exp-key-1"))
	if code != http.StatusCreated {
		t.Fatalf("owner create expense: status %d, want 201 (body %s)", code, resp)
	}
	var created struct {
		Expense domain.Expense `json:"expense"`
	}
	mustJSON(t, resp, &created)
	if created.Expense.PropertyID != f.propA {
		t.Fatalf("expense created for property %s, want owner's property %s", created.Expense.PropertyID, f.propA)
	}

	code, resp = f.call(http.MethodGet, "/api/owner/finance/expenses", f.ownerA, nil, nil)
	if code != http.StatusOK || !strings.Contains(resp, created.Expense.ID.String()) {
		t.Fatalf("owner list expenses: status %d, expected new expense in body (%s)", code, resp)
	}

	// Replaying the same Idempotency-Key must not create a second expense.
	_, _ = f.call(http.MethodPost, "/api/owner/finance/expenses", f.ownerA, body, idemHeader("exp-key-1"))
	code, resp = f.call(http.MethodGet, "/api/owner/finance/expenses", f.ownerA, nil, nil)
	var list struct {
		Expenses []domain.Expense `json:"expenses"`
	}
	mustJSON(t, resp, &list)
	if code != http.StatusOK || len(list.Expenses) != 1 {
		t.Fatalf("idempotent replay created %d expenses, want exactly 1 (status %d)", len(list.Expenses), code)
	}
}

func TestFeatureExpense_CrossPropertyIsNotFound(t *testing.T) {
	f := newFinanceFixture(t)
	victim := f.seedExpense(f.propB, 40000)

	// Owner A names Property B's expense in every mutating form. The service refuses
	// with finance.forbidden (CONTRACT.md: "not authorized for this financial mutation").
	code, resp := f.call(http.MethodPost, "/api/owner/finance/expenses/"+victim.String()+"/void", f.ownerA,
		map[string]any{"reason": "not my property"}, nil)
	if code != http.StatusForbidden || errCode(resp) != "finance.forbidden" {
		t.Fatalf("owner A voided property B's expense: status %d code %q, want 403 finance.forbidden (body %s)",
			code, errCode(resp), resp)
	}
	code, resp = f.call(http.MethodPost, "/api/owner/finance/expenses/"+victim.String()+"/payments", f.ownerA,
		map[string]any{"amount_paise": 1000, "method": "cash"}, idemHeader("cross-pay-1"))
	if code != http.StatusForbidden || errCode(resp) != "finance.forbidden" {
		t.Fatalf("owner A paid property B's expense: status %d code %q, want 403 finance.forbidden (body %s)",
			code, errCode(resp), resp)
	}

	// The expense must be untouched for its real owner.
	code, resp = f.call(http.MethodGet, "/api/owner/finance/expenses", f.ownerB, nil, nil)
	if code != http.StatusOK || !strings.Contains(resp, victim.String()) {
		t.Fatalf("property B expense disappeared or changed after cross-property attempt (status %d)", code)
	}
	if strings.Contains(resp, `"status":"cancelled"`) || strings.Contains(resp, `"status":"voided"`) {
		t.Fatalf("property B expense was voided by owner A: %s", resp)
	}
}

func TestFeatureExpense_VoidRequiresReasonAndIsOneWay(t *testing.T) {
	f := newFinanceFixture(t)
	id := f.seedExpense(f.propA, 30000)
	path := "/api/owner/finance/expenses/" + id.String() + "/void"

	code, resp := f.call(http.MethodPost, path, f.ownerA, map[string]any{"reason": "x"}, nil)
	if code != http.StatusBadRequest || errCode(resp) != "finance.reasonRequired" {
		t.Fatalf("void with 1-character reason: status %d code %q, want 400 finance.reasonRequired", code, errCode(resp))
	}

	code, resp = f.call(http.MethodPost, path, f.ownerA, map[string]any{"reason": "duplicate bill entry"}, nil)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("owner void with valid reason: status %d (body %s)", code, resp)
	}
	code, resp = f.call(http.MethodPost, path, f.ownerA, map[string]any{"reason": "duplicate bill entry"}, nil)
	if code != http.StatusConflict || errCode(resp) != "finance.expenseNotVoidable" {
		t.Fatalf("second void: status %d code %q, want 409 finance.expenseNotVoidable", code, errCode(resp))
	}
}

func TestFeatureExpense_PaymentCannotOverpay(t *testing.T) {
	f := newFinanceFixture(t)
	id := f.seedExpense(f.propA, 30000)
	path := "/api/owner/finance/expenses/" + id.String() + "/payments"

	code, resp := f.call(http.MethodPost, path, f.ownerA,
		map[string]any{"amount_paise": 30001, "method": "bank"}, idemHeader("overpay-1"))
	if code != http.StatusBadRequest || errCode(resp) != "finance.overpay" {
		t.Fatalf("overpay: status %d code %q, want 400 finance.overpay (body %s)", code, errCode(resp), resp)
	}
	code, resp = f.call(http.MethodPost, path, f.ownerA,
		map[string]any{"amount_paise": 10000, "method": "upi"}, idemHeader("partial-1"))
	if code != http.StatusCreated {
		t.Fatalf("partial payment: status %d, want 201 (body %s)", code, resp)
	}
}

// ---- Manager spending limits and approvals --------------------------------

// A manager may post a small expense directly, but an expense above the
// single-expense limit must not be silently accepted as a plain expense.
func TestFeatureExpense_ManagerAboveLimitNeedsApproval(t *testing.T) {
	f := newFinanceFixture(t)
	body := map[string]any{
		"category_code": "repairs", "vendor_name": "Electrician", "description": "rewire",
		"amount_paise": 600000, // ₹6,000, above the default ₹5,000 single-expense limit
	}
	code, resp := f.call(http.MethodPost, "/api/manager/finance/expenses", f.managerA, body, idemHeader("mgr-big-1"))
	if code != http.StatusCreated {
		t.Fatalf("manager over-limit expense: status %d, want 201 with a queued approval (body %s)", code, resp)
	}
	var created struct {
		Expense  domain.Expense          `json:"expense"`
		Approval *domain.ApprovalRequest `json:"approval"`
	}
	mustJSON(t, resp, &created)
	if created.Approval == nil {
		t.Fatalf("over-limit expense was not routed to maker-checker: %s", resp)
	}
	if created.Approval.Status != "pending" {
		t.Fatalf("approval status %q, want pending", created.Approval.Status)
	}
	if created.Expense.Status != domain.ExpensePendingApproval {
		t.Fatalf("expense status %q, want %q", created.Expense.Status, domain.ExpensePendingApproval)
	}

	// Money cannot leave the property while the expense is still awaiting approval.
	code, resp = f.call(http.MethodPost, "/api/manager/finance/expenses/"+created.Expense.ID.String()+"/payments",
		f.managerA, map[string]any{"amount_paise": 600000, "method": "bank"}, idemHeader("mgr-pay-pending"))
	if code != http.StatusBadRequest || errCode(resp) != "finance.expenseNotPayable" {
		t.Fatalf("paying a pending-approval expense: status %d code %q, want 400 finance.expenseNotPayable (body %s)",
			code, errCode(resp), resp)
	}
}

func TestFeatureApproval_ManagerCannotDecideOwnerReviewQueue(t *testing.T) {
	f := newFinanceFixture(t)
	code, resp := f.call(http.MethodGet, "/api/owner/finance/approvals?status=pending", f.managerA, nil, nil)
	if code != http.StatusForbidden || errCode(resp) != "auth.forbidden" {
		t.Fatalf("manager reading owner approval queue: status %d code %q, want 403 auth.forbidden", code, errCode(resp))
	}
	code, resp = f.call(http.MethodPost, "/api/owner/finance/approvals/"+uuid.New().String()+"/approve", f.managerA,
		map[string]any{"note": "self-approve"}, nil)
	if code != http.StatusForbidden || errCode(resp) != "auth.forbidden" {
		t.Fatalf("manager approving: status %d code %q, want 403 auth.forbidden", code, errCode(resp))
	}
}

func TestFeatureApproval_OwnerDecisionIsRecordedOnce(t *testing.T) {
	f := newFinanceFixture(t)
	code, resp := f.call(http.MethodPost, "/api/manager/finance/expenses", f.managerA, map[string]any{
		"category_code": "repairs", "vendor_name": "Electrician", "description": "rewire",
		"amount_paise": 600000,
	}, idemHeader("mgr-big-2"))
	if code != http.StatusCreated || !strings.Contains(resp, `"approval"`) {
		t.Fatalf("manager over-limit expense did not enter the approval queue: status %d (body %s)", code, resp)
	}
	var created struct {
		Approval struct {
			ID uuid.UUID `json:"id"`
		} `json:"approval"`
	}
	mustJSON(t, resp, &created)
	approve := "/api/owner/finance/approvals/" + created.Approval.ID.String() + "/approve"

	code, resp = f.call(http.MethodPost, approve, f.ownerA, map[string]any{"note": "ok"}, nil)
	if code != http.StatusOK {
		t.Fatalf("owner approve: status %d (body %s)", code, resp)
	}
	code, resp = f.call(http.MethodPost, approve, f.ownerA, map[string]any{"note": "again"}, nil)
	if code == http.StatusOK {
		t.Fatalf("second approve of the same request succeeded (status %d); decisions must be recorded once", code)
	}
}

// ---- Capital, budgets ------------------------------------------------------

func TestFeatureCapital_OwnerRecordsManagerCannot(t *testing.T) {
	f := newFinanceFixture(t)
	body := map[string]any{"kind": "initial", "amount_paise": 50000000, "purpose": "startup capital"}

	code, resp := f.call(http.MethodPost, "/api/owner/finance/capital", f.managerA, body, idemHeader("cap-m-1"))
	if code != http.StatusForbidden || errCode(resp) != "auth.forbidden" {
		t.Fatalf("manager posting capital: status %d code %q, want 403 auth.forbidden", code, errCode(resp))
	}
	code, resp = f.call(http.MethodPost, "/api/owner/finance/capital", f.ownerA, body, idemHeader("cap-o-1"))
	if code != http.StatusCreated {
		t.Fatalf("owner posting capital: status %d, want 201 (body %s)", code, resp)
	}
	code, resp = f.call(http.MethodGet, "/api/owner/finance/capital", f.ownerA, nil, nil)
	if code != http.StatusOK || !strings.Contains(resp, `"capital"`) || !strings.Contains(resp, `50000000`) {
		t.Fatalf("owner capital list: status %d, want the 50,000,000 paise entry (body %s)", code, resp)
	}
}

func TestFeatureBudget_OwnerUpsertsAndManagerIsRefused(t *testing.T) {
	f := newFinanceFixture(t)
	body := map[string]any{"category_code": "groceries", "period_month": "2026-10", "amount_paise": 9000000}

	code, resp := f.call(http.MethodPost, "/api/owner/finance/budgets", f.managerA, body, nil)
	if code != http.StatusForbidden || errCode(resp) != "auth.forbidden" {
		t.Fatalf("manager posting budget: status %d code %q, want 403", code, errCode(resp))
	}
	code, resp = f.call(http.MethodPost, "/api/owner/finance/budgets", f.ownerA, body, nil)
	if code != http.StatusCreated {
		t.Fatalf("owner posting budget: status %d, want 201 (body %s)", code, resp)
	}
	var created struct {
		Budget struct {
			ID uuid.UUID `json:"id"`
		} `json:"budget"`
	}
	mustJSON(t, resp, &created)
	if created.Budget.ID == uuid.Nil {
		t.Fatalf("budget response has no id: %s", resp)
	}

	// PATCH on the same id changes the amount.
	code, resp = f.call(http.MethodPatch, "/api/owner/finance/budgets/"+created.Budget.ID.String(), f.ownerA,
		map[string]any{"category_code": "groceries", "period_month": "2026-10", "amount_paise": 7000000}, nil)
	if code != http.StatusCreated || !strings.Contains(resp, `7000000`) {
		t.Fatalf("owner patch budget: status %d, want 201 with 7000000 (body %s)", code, resp)
	}
}

// ---- Collections tie-out ---------------------------------------------------

func TestFeatureTieOut_CloseAndReopenAreOwnerOnly(t *testing.T) {
	f := newFinanceFixture(t)
	code, resp := f.call(http.MethodPost, "/api/owner/finance/tie-out/close?period=2026-09", f.managerA, nil, nil)
	if code != http.StatusForbidden || errCode(resp) != "auth.forbidden" {
		t.Fatalf("manager closing tie-out: status %d code %q, want 403", code, errCode(resp))
	}
	code, resp = f.call(http.MethodGet, "/api/owner/finance/tie-out?period=2026-09", f.ownerA, nil, nil)
	if code != http.StatusOK || !strings.Contains(resp, `"tie_out"`) {
		t.Fatalf("owner reading tie-out: status %d (body %s)", code, resp)
	}
	// With no collections and no ledger lines the difference is zero, so the close succeeds.
	code, resp = f.call(http.MethodPost, "/api/owner/finance/tie-out/close?period=2026-09", f.ownerA, nil, nil)
	if code != http.StatusOK || !strings.Contains(resp, `"status":"closed"`) || !strings.Contains(resp, `"difference_paise":0`) {
		t.Fatalf("owner closing zero-difference tie-out: status %d, want 200 closed with difference 0 (body %s)", code, resp)
	}
}

func TestFeatureExpense_ManagerVoidSelfAndForbiddenOther(t *testing.T) {
	f := newFinanceFixture(t)

	// Manager A creates an expense under threshold
	code, resp := f.call(http.MethodPost, "/api/manager/finance/expenses", f.managerA, map[string]any{
		"category_code": "maintenance",
		"amount_paise":  10000,
		"vendor_name":   "Hardware Store",
	}, idemHeader("mgr-exp-1"))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("manager create expense failed: status %d (body %s)", code, resp)
	}

	var created struct {
		Expense domain.Expense `json:"expense"`
	}
	mustJSON(t, resp, &created)
	expID := created.Expense.ID

	// Manager A voids their own expense via /api/manager/finance/expenses/:id/void
	mgrVoidPath := "/api/manager/finance/expenses/" + expID.String() + "/void"
	code, resp = f.call(http.MethodPost, mgrVoidPath, f.managerA, map[string]any{
		"reason": "cancelled work order",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("manager void own expense: status %d code %q (body %s)", code, errCode(resp), resp)
	}

	// Manager A attempts to void Owner's expense -> 403 Forbidden
	ownerExpID := f.seedExpense(f.propA, 25000)
	ownerVoidPath := "/api/manager/finance/expenses/" + ownerExpID.String() + "/void"
	code, resp = f.call(http.MethodPost, ownerVoidPath, f.managerA, map[string]any{
		"reason": "unauthorized attempt",
	}, nil)
	if code != http.StatusForbidden || errCode(resp) != "finance.forbidden" {
		t.Fatalf("manager void owner expense: status %d code %q, want 403 finance.forbidden (body %s)",
			code, errCode(resp), resp)
	}
}

func TestFeatureExpense_ManagerVoidPendingExpenseCancelsApproval(t *testing.T) {
	f := newFinanceFixture(t)

	// Manager A creates expense above single limit without emergency -> pending approval
	code, resp := f.call(http.MethodPost, "/api/manager/finance/expenses", f.managerA, map[string]any{
		"category_code": "vendor",
		"amount_paise":  600000,
		"vendor_name":   "Big Equipment",
	}, idemHeader("mgr-pending-void-1"))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create pending expense failed: status %d (body %s)", code, resp)
	}

	var created struct {
		Expense domain.Expense `json:"expense"`
	}
	mustJSON(t, resp, &created)
	if created.Expense.Status != domain.ExpensePendingApproval {
		t.Fatalf("expected pending_approval status, got %s", created.Expense.Status)
	}

	// Manager voids their own pending expense
	voidPath := "/api/manager/finance/expenses/" + created.Expense.ID.String() + "/void"
	code, resp = f.call(http.MethodPost, voidPath, f.managerA, map[string]any{
		"reason": "no longer needed",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("manager void pending expense failed: status %d code %q (body %s)", code, errCode(resp), resp)
	}

	// Verify expense status is cancelled in store
	exp, err := f.store.GetExpense(context.Background(), created.Expense.ID)
	if err != nil || exp.Status != domain.ExpenseCancelled {
		t.Fatalf("expected cancelled expense status, got %v, err %v", exp, err)
	}

	// Verify pending approvals for this expense are cancelled (no pending approvals remaining)
	apprs, err := f.store.ListApprovals(context.Background(), f.propA, "pending")
	if err != nil {
		t.Fatalf("list pending approvals: %v", err)
	}
	for _, a := range apprs {
		if a.SubjectID == created.Expense.ID {
			t.Fatalf("expected approval for voided expense to be cancelled, found pending approval: %v", a)
		}
	}
}

func TestFeatureExpense_ManagerVoidAboveOwnerThresholdRequiresOwner(t *testing.T) {
	f := newFinanceFixture(t)

	// Manager A creates an emergency expense above owner approval threshold (600,000 > 500,000)
	code, resp := f.call(http.MethodPost, "/api/manager/finance/expenses", f.managerA, map[string]any{
		"category_code": "vendor",
		"amount_paise":  600000,
		"emergency":     true,
		"vendor_name":   "Emergency Generator Repair",
	}, idemHeader("mgr-emerg-void-1"))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create emergency expense failed: status %d (body %s)", code, resp)
	}

	var created struct {
		Expense domain.Expense `json:"expense"`
	}
	mustJSON(t, resp, &created)
	if created.Expense.Status != domain.ExpenseApproved {
		t.Fatalf("expected approved emergency expense, got %s", created.Expense.Status)
	}

	// Manager attempts to void approved expense above threshold -> ErrApprovalRequired (400)
	mgrVoidPath := "/api/manager/finance/expenses/" + created.Expense.ID.String() + "/void"
	code, resp = f.call(http.MethodPost, mgrVoidPath, f.managerA, map[string]any{
		"reason": "repair cancelled",
	}, nil)
	if code != http.StatusBadRequest || errCode(resp) != "finance.approvalRequired" {
		t.Fatalf("expected 400 finance.approvalRequired, got status %d code %q (body %s)",
			code, errCode(resp), resp)
	}

	// Owner CAN void it
	ownerVoidPath := "/api/owner/finance/expenses/" + created.Expense.ID.String() + "/void"
	code, resp = f.call(http.MethodPost, ownerVoidPath, f.ownerA, map[string]any{
		"reason": "owner approved cancellation",
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("owner void failed: status %d code %q (body %s)", code, errCode(resp), resp)
	}
}


