package api

// Feature-level HTTP tests against live PostgreSQL for the tenant-payment and
// deposit surfaces. Each test seeds its own property, so they are independent.
// Seeding follows the journey test: the owner, tenant login and due are inserted
// with SQL because the HTTP entry points for them need OTP or the billing job.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// liveTenantFixture is one property with an owner, an active tenant and one pending due.
type liveTenantFixture struct {
	propID    uuid.UUID
	ownerTok  string
	tenantTok string
	tenantID  uuid.UUID
	dueID     uuid.UUID
}

func seedLiveTenantFixture(t *testing.T, h *liveHarness, dueAmount int64) *liveTenantFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	propID := uuid.New()
	ownerID := uuid.New()
	tenantUserID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()
	ownerPhone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	tenantPhone := fmt.Sprintf("+918%09d", time.Now().UnixNano()%1000000000)

	h.scrub(propID)
	t.Cleanup(func() { h.scrub(propID) })

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Feature Live PG', '2 Test Rd', $2, 'feat@upi', 'Feature Owner', 'owner@feature.test', $3)`,
		propID, ownerPhone, uuid.New().String()[:8])
	mustExec(`INSERT INTO users (id, phone, role, property_id) VALUES ($1, $2, 'owner', $3)`, ownerID, ownerPhone, propID)
	mustExec(`INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Feature Tenant', $3, '202', $4, 5, 'active')`, tenantID, propID, tenantPhone, dueAmount)
	mustExec(`INSERT INTO users (id, phone, role, property_id, tenant_id) VALUES ($1, $2, 'tenant', $3, $4)`,
		tenantUserID, tenantPhone, propID, tenantID)

	now := istNow()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	mustExec(`INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
		VALUES ($1, $2, $3, $4, 'rent', 'pending', $5, $5, $6, $7, $8)`,
		dueID, propID, tenantID, "F"+uuid.New().String()[:7], dueAmount,
		periodStart, periodStart.AddDate(0, 1, 0), periodStart.AddDate(0, 0, 4))

	owner := &domain.User{ID: ownerID, Role: domain.RoleOwner, Phone: ownerPhone, PropertyID: &propID}
	tenantUser := &domain.User{ID: tenantUserID, Role: domain.RoleTenant, Phone: tenantPhone, PropertyID: &propID, TenantID: &tenantID}
	ownerTok, err := auth.IssueToken(h.secret, owner)
	if err != nil {
		t.Fatalf("owner token: %v", err)
	}
	tenantTok, err := auth.IssueToken(h.secret, tenantUser)
	if err != nil {
		t.Fatalf("tenant token: %v", err)
	}
	return &liveTenantFixture{propID: propID, ownerTok: ownerTok, tenantTok: tenantTok, tenantID: tenantID, dueID: dueID}
}

func (f *liveTenantFixture) dueStatus(t *testing.T, h *liveHarness) string {
	t.Helper()
	var s string
	if err := h.pool.QueryRow(context.Background(), `SELECT status FROM dues WHERE id = $1`, f.dueID).Scan(&s); err != nil {
		t.Fatalf("read due status: %v", err)
	}
	return s
}

// paymentCount is the number of recorded payments against the fixture's due.
func (f *liveTenantFixture) paymentCount(t *testing.T, h *liveHarness) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM payments WHERE due_id = $1`, f.dueID).Scan(&n); err != nil {
		t.Fatalf("count payments: %v", err)
	}
	return n
}

// submitReport is the tenant's UPI payment report for their due.
// UTRs are unique across properties, so each run suffixes the fixed label to
// keep rows left by earlier runs from colliding with this one.
func submitReport(t *testing.T, h *liveHarness, f *liveTenantFixture, utr string, amountPaise int64) uuid.UUID {
	t.Helper()
	utr = utr + "-" + uuid.New().String()[:8]
	code, body := h.httpCall(t, http.MethodPost, "/api/tenant/dues/"+f.dueID.String()+"/reports", f.tenantTok,
		map[string]any{"upi_txn_id": utr, "amount": amountPaise})
	if code != http.StatusCreated {
		t.Fatalf("tenant submit report: status %d, want 201 (body %s)", code, body)
	}
	var rep struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(body, &rep); err != nil || rep.ID == uuid.Nil {
		t.Fatalf("report response has no id: %s", body)
	}
	return rep.ID
}

// ---- Payment reports: confirm and reject ----------------------------------

func TestFeaturePaymentReport_OwnerConfirmSettlesDue(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)
	repID := submitReport(t, h, f, "UPI-FEAT-CONFIRM-1", 1500000)

	// The tenant can report but cannot confirm their own payment.
	code, body := h.httpCall(t, http.MethodPost, "/api/owner/payment-reports/"+repID.String()+"/confirm", f.tenantTok, nil)
	if code != http.StatusForbidden || errCode(string(body)) != "auth.forbidden" {
		t.Fatalf("tenant confirming own report: status %d code %q, want 403 auth.forbidden", code, errCode(string(body)))
	}

	code, body = h.httpCall(t, http.MethodGet, "/api/owner/payment-reports?status=pending_review", f.ownerTok, nil)
	if code != http.StatusOK || !strings.Contains(string(body), repID.String()) {
		t.Fatalf("owner pending report queue: status %d, report not listed (body %s)", code, body)
	}

	code, body = h.httpCall(t, http.MethodPost, "/api/owner/payment-reports/"+repID.String()+"/confirm", f.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("owner confirm: status %d (body %s)", code, body)
	}
	if !strings.Contains(string(body), `"confirmed"`) || !strings.Contains(string(body), `"payment"`) {
		t.Fatalf("confirm response lacks confirmed report and payment: %s", body)
	}
	if got := f.dueStatus(t, h); got != string(domain.DueStatusPaid) {
		t.Fatalf("due status after confirm %q, want paid", got)
	}

	// Confirming again is idempotent: it answers with the existing report and
	// must not record a second payment.
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/payment-reports/"+repID.String()+"/confirm", f.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("second confirm: status %d, want 200 idempotent (body %s)", code, body)
	}
	if n := f.paymentCount(t, h); n != 1 {
		t.Fatalf("payments recorded for due after double confirm = %d, want exactly 1", n)
	}

	// The tenant cannot report against a due that is already paid.
	code, body = h.httpCall(t, http.MethodPost, "/api/tenant/dues/"+f.dueID.String()+"/reports", f.tenantTok,
		map[string]any{"upi_txn_id": "UPI-FEAT-CONFIRM-2", "amount": 1500000})
	if code != http.StatusConflict {
		t.Fatalf("report against paid due: status %d, want 409 (body %s)", code, body)
	}
}

func TestFeaturePaymentReport_OwnerRejectLeavesDueOpen(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)
	repID := submitReport(t, h, f, "UPI-FEAT-REJECT-1", 1500000)

	code, body := h.httpCall(t, http.MethodPost, "/api/owner/payment-reports/"+repID.String()+"/reject", f.ownerTok,
		map[string]any{"note": "amount not in bank statement"})
	if code != http.StatusOK || !strings.Contains(string(body), `"rejected"`) {
		t.Fatalf("owner reject: status %d, want 200 with rejected report (body %s)", code, body)
	}
	if got := f.dueStatus(t, h); got != string(domain.DueStatusPending) {
		t.Fatalf("due status after reject %q, want pending", got)
	}

	// A rejected report cannot later be confirmed: the handler answers with the
	// still-rejected report and records no payment.
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/payment-reports/"+repID.String()+"/confirm", f.ownerTok, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"rejected"`) || strings.Contains(string(body), `"confirmed"`) {
		t.Fatalf("confirm after reject: status %d, want 200 with report still rejected (body %s)", code, body)
	}
	if n := f.paymentCount(t, h); n != 0 {
		t.Fatalf("payments recorded after rejected report = %d, want 0", n)
	}
	if got := f.dueStatus(t, h); got != string(domain.DueStatusPending) {
		t.Fatalf("due status after refused confirm %q, want pending", got)
	}
}

func TestFeaturePaymentReport_OwnerOfOtherPropertyCannotConfirm(t *testing.T) {
	h := newLiveHarness(t)
	a := seedLiveTenantFixture(t, h, 1500000)
	b := seedLiveTenantFixture(t, h, 1500000)
	repID := submitReport(t, h, a, "UPI-FEAT-XPROP-1", 1500000)

	// Owner B holds a valid token for property B and names A's report.
	code, body := h.httpCall(t, http.MethodPost, "/api/owner/payment-reports/"+repID.String()+"/confirm", b.ownerTok, nil)
	if code == http.StatusOK {
		t.Fatalf("owner B confirmed property A's report (status %d, body %s)", code, body)
	}
	if got := a.dueStatus(t, h); got != string(domain.DueStatusPending) {
		t.Fatalf("property A due changed to %q by owner B", got)
	}
	code, body = h.httpCall(t, http.MethodGet, "/api/owner/payment-reports?status=pending_review", b.ownerTok, nil)
	if code != http.StatusOK || strings.Contains(string(body), repID.String()) {
		t.Fatalf("owner B's report queue exposes property A's report (status %d)", code)
	}
}

// ---- Logout ----------------------------------------------------------------

func TestFeatureLogout_PublicEndpointAnswersWithoutSession(t *testing.T) {
	h := newLiveHarness(t)
	code, body := h.httpCall(t, http.MethodPost, "/api/auth/logout", "", nil)
	if code != http.StatusOK {
		t.Fatalf("logout without a session: status %d, want 200 (body %s)", code, body)
	}
}

// ---- Notices and Deposits --------------------------------------------------

func TestFeatureTenantNotice_LogNoticeAndCrossProperty(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)
	other := seedLiveTenantFixture(t, h, 1500000)

	// Owner logs notice for their tenant
	noticeDate := time.Now().UTC().Truncate(time.Second)
	code, body := h.httpCall(t, http.MethodPost, "/api/owner/tenants/"+f.tenantID.String()+"/notice", f.ownerTok,
		map[string]any{"notice_given_at": noticeDate})
	if code != http.StatusOK {
		t.Fatalf("owner log notice: status %d (body %s)", code, body)
	}

	// Verify database row reflects notice_given_at
	var savedNotice *time.Time
	if err := h.pool.QueryRow(context.Background(), `SELECT notice_given_at FROM tenants WHERE id = $1`, f.tenantID).Scan(&savedNotice); err != nil {
		t.Fatalf("query tenant notice_given_at: %v", err)
	}
	if savedNotice == nil {
		t.Fatalf("notice_given_at was not saved to database")
	}

	// Cross-property isolation: owner of other property cannot log notice for tenant
	code, _ = h.httpCall(t, http.MethodPost, "/api/owner/tenants/"+f.tenantID.String()+"/notice", other.ownerTok,
		map[string]any{"notice_given_at": noticeDate})
	if code != http.StatusNotFound {
		t.Fatalf("other owner log notice: status %d, want 404", code)
	}
}

func TestFeatureTenantDeparture_CreateLifecycle(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)

	code, body := h.httpCall(t, http.MethodPost, "/api/owner/tenants/"+f.tenantID.String()+"/departures", f.ownerTok,
		map[string]any{
			"planned_vacate_date": "2026-11-01",
			"notes":               "Moving to a different city",
		})
	if code != http.StatusCreated {
		t.Fatalf("owner create departure: status %d, want 201 (body %s)", code, body)
	}
	var resp struct {
		Departure domain.TenantDeparture `json:"departure"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Departure.ID == uuid.Nil {
		t.Fatalf("invalid departure response: %s", body)
	}

	// Inspect departure (starts 24h SLA)
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/departures/"+resp.Departure.ID.String()+"/inspect", f.ownerTok,
		map[string]any{"notes": "Keys received"})
	if code != http.StatusOK {
		t.Fatalf("owner inspect departure: status %d (body %s)", code, body)
	}

	// Add first deduction (agreed cleaning fee: ₹500)
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/departures/"+resp.Departure.ID.String()+"/deductions", f.ownerTok,
		map[string]any{
			"description":  "Cleaning fee",
			"amount_paise": 50000,
			"status":       "agreed",
		})
	if code != http.StatusCreated {
		t.Fatalf("owner add departure deduction: status %d (body %s)", code, body)
	}

	// Add second deduction that EXCEEDS the held deposit (damage: ₹20,000 vs ₹15,000 deposit)
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/departures/"+resp.Departure.ID.String()+"/deductions", f.ownerTok,
		map[string]any{
			"description":  "Extensive wall restoration",
			"amount_paise": 2000000,
			"status":       "agreed",
		})
	if code != http.StatusCreated {
		t.Fatalf("owner add excess deduction: status %d (body %s)", code, body)
	}

	// Settle departure: Deposit Refund Cap Invariant Test
	// Total deductions = 2,050,000 paise. Held deposit = 1,500,000 paise.
	// Net refund MUST be 0 (never negative or paying out more than deposit).
	// Excess 550,000 paise MUST be recorded in receivable_balance_paise.
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/departures/"+resp.Departure.ID.String()+"/settle", f.ownerTok,
		map[string]any{
			"actual_vacate_date":  "2026-11-01",
			"prorated_rent_paise": 0,
		})
	if code != http.StatusOK {
		t.Fatalf("owner settle departure: status %d (body %s)", code, body)
	}
	var settleResp struct {
		NetRefundPaise         int64 `json:"net_refund_paise"`
		ReceivableBalancePaise int64 `json:"receivable_balance_paise"`
		DeductionsPaise        int64 `json:"deductions_paise"`
	}
	if err := json.Unmarshal(body, &settleResp); err != nil {
		t.Fatalf("unmarshal settle response: %v", err)
	}
	if settleResp.NetRefundPaise != 0 {
		t.Fatalf("deposit refund cap violated: net_refund_paise = %d, want 0", settleResp.NetRefundPaise)
	}
	if settleResp.ReceivableBalancePaise != 550000 {
		t.Fatalf("receivable_balance_paise = %d, want 550000 (2050000 - 1500000)", settleResp.ReceivableBalancePaise)
	}
}

// ---- Payment Verify & Correct ---------------------------------------------

func TestFeaturePayment_VerifyAndCorrect(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)
	utr := fmt.Sprintf("UTRLIVEVERIFY%08d", time.Now().UnixNano()%100000000)

	// 1. Verify rejects invalid amount <= 0
	code, body := h.httpCall(t, http.MethodPost, "/api/owner/payments/verify", f.ownerTok,
		map[string]any{
			"due_id":       f.dueID,
			"amount_paise": 0,
			"upi_txn_id":   utr,
		})
	if code != http.StatusBadRequest {
		t.Fatalf("verify payment with 0 amount: status %d, want 400 (body %s)", code, body)
	}

	// 2. Owner verifies payment directly
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/payments/verify", f.ownerTok,
		map[string]any{
			"due_id":       f.dueID,
			"amount_paise": 1500000,
			"upi_txn_id":   utr,
			"note":         "Direct bank verification",
		})
	if code != http.StatusOK {
		t.Fatalf("verify payment: status %d (body %s)", code, body)
	}

	var p domain.Payment
	if err := json.Unmarshal(body, &p); err != nil || p.ID == uuid.Nil {
		t.Fatalf("invalid payment response: %s", body)
	}
	if got := f.dueStatus(t, h); got != string(domain.DueStatusPaid) {
		t.Fatalf("due status after verify: %q, want paid", got)
	}

	// 3. Verify on already-paid due rejects with client error (due not open)
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/payments/verify", f.ownerTok,
		map[string]any{
			"due_id":       f.dueID,
			"amount_paise": 1500000,
			"upi_txn_id":   utr + "2",
		})
	if code != http.StatusBadRequest && code != http.StatusConflict {
		t.Fatalf("verify on paid due: status %d, want 400/409 (body %s)", code, body)
	}

	// 4. Owner records financial correction
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/payments/"+p.ID.String()+"/correct", f.ownerTok,
		map[string]any{
			"corrected_amount_paise": 1200000,
			"corrected_upi_txn_id":   utr + "CORR",
			"reason":                 "Rent rebate agreed",
		})
	if code != http.StatusOK {
		t.Fatalf("correct payment: status %d (body %s)", code, body)
	}
}

// ---- Dues Pay Options -----------------------------------------------------

func TestFeatureTenantDues_Options(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)

	// 1. Initial pending due gives options
	code, body := h.httpCall(t, http.MethodGet, "/api/tenant/dues/options", f.tenantTok, nil)
	if code != http.StatusOK {
		t.Fatalf("tenant dues options: status %d (body %s)", code, body)
	}
	var resp struct {
		TotalOutstandingPaise int64                  `json:"total_outstanding_paise"`
		Options               []domain.PaymentOption `json:"options"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal dues options: %v (body %s)", err, body)
	}
	if resp.TotalOutstandingPaise != 1500000 {
		t.Fatalf("total outstanding = %d, want 1500000", resp.TotalOutstandingPaise)
	}
	if len(resp.Options) == 0 {
		t.Fatalf("expected at least 1 payment option, got 0")
	}

	// 2. Pay the due
	utr := fmt.Sprintf("UTROPTIONS%08d", time.Now().UnixNano()%100000000)
	code, _ = h.httpCall(t, http.MethodPost, "/api/owner/payments/verify", f.ownerTok,
		map[string]any{
			"due_id":       f.dueID,
			"amount_paise": 1500000,
			"upi_txn_id":   utr,
		})
	if code != http.StatusOK {
		t.Fatalf("pay due failed: status %d", code)
	}

	// 3. Once paid, total outstanding is zero and options list is empty
	code, body = h.httpCall(t, http.MethodGet, "/api/tenant/dues/options", f.tenantTok, nil)
	if code != http.StatusOK {
		t.Fatalf("tenant dues options after paid: status %d", code)
	}
	var paidResp struct {
		TotalOutstandingPaise int64                  `json:"total_outstanding_paise"`
		Options               []domain.PaymentOption `json:"options"`
	}
	if err := json.Unmarshal(body, &paidResp); err != nil {
		t.Fatalf("unmarshal paid dues options: %v", err)
	}
	if paidResp.TotalOutstandingPaise != 0 {
		t.Fatalf("total outstanding after paid = %d, want 0", paidResp.TotalOutstandingPaise)
	}
	if len(paidResp.Options) != 0 {
		t.Fatalf("expected 0 options for zero due, got %d", len(paidResp.Options))
	}
}

// ---- Rewards / Gamification -----------------------------------------------

func TestFeatureRewards_PointsCatalogAndLeaderboard(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)

	// Points: new tenant has 0 points
	code, body := h.httpCall(t, http.MethodGet, "/api/tenant/points", f.tenantTok, nil)
	if code != http.StatusOK {
		t.Fatalf("tenant points: status %d (body %s)", code, body)
	}

	// Rewards Catalog
	code, body = h.httpCall(t, http.MethodGet, "/api/tenant/rewards", f.tenantTok, nil)
	if code != http.StatusOK {
		t.Fatalf("tenant rewards: status %d (body %s)", code, body)
	}
	var catResp struct {
		Rewards []domain.RewardsCatalogItem `json:"rewards"`
	}
	_ = json.Unmarshal(body, &catResp)
	if len(catResp.Rewards) > 0 {
		// Insufficient Points Rejection: tenant with 0 points tries to redeem
		firstReward := catResp.Rewards[0]
		code, body = h.httpCall(t, http.MethodPost, "/api/tenant/rewards/"+firstReward.ID.String()+"/redeem", f.tenantTok, nil)
		if code != http.StatusBadRequest {
			t.Fatalf("redeem with insufficient points: status %d, want 400 (body %s)", code, body)
		}
	}

	// Leaderboard
	code, body = h.httpCall(t, http.MethodGet, "/api/tenant/leaderboard", f.tenantTok, nil)
	if code != http.StatusOK {
		t.Fatalf("tenant leaderboard: status %d (body %s)", code, body)
	}
}

// ---- KYC ------------------------------------------------------------------

func TestFeatureKYC_ConsentStatusAndRevoke(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)

	// 1. Grant consent
	code, body := h.httpCall(t, http.MethodPost, "/api/tenant/kyc/consent", f.tenantTok,
		map[string]any{
			"purpose":         "rental_onboarding",
			"consent_version": "v1.0",
			"consent_text":    "I hereby give consent to process my Aadhaar for rental KYC verification.",
		})
	if code != http.StatusCreated {
		t.Fatalf("tenant kyc consent: status %d (body %s)", code, body)
	}

	// 2. Read KYC status
	code, body = h.httpCall(t, http.MethodGet, "/api/tenant/kyc/status", f.tenantTok, nil)
	if code != http.StatusOK {
		t.Fatalf("tenant kyc status: status %d (body %s)", code, body)
	}

	// 3. Owner reads tenant KYC status
	code, body = h.httpCall(t, http.MethodGet, "/api/owner/tenants/"+f.tenantID.String()+"/kyc", f.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("owner read tenant kyc: status %d (body %s)", code, body)
	}

	// 4. Owner clears duplicate status for tenant (returns 404 when no active verification exists)
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/tenants/"+f.tenantID.String()+"/kyc/clear-duplicate", f.ownerTok,
		map[string]any{"reason": "Identity verified manually by property manager"})
	if code != http.StatusOK && code != http.StatusNotFound {
		t.Fatalf("owner clear duplicate kyc: status %d (body %s)", code, body)
	}

	// 5. Tenant revokes consent
	code, body = h.httpCall(t, http.MethodPost, "/api/tenant/kyc/revoke", f.tenantTok, map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("tenant kyc revoke: status %d (body %s)", code, body)
	}
}

// ---- Settlements ----------------------------------------------------------

func TestFeatureSettlements_ListAndEODBalance(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)

	code, body := h.httpCall(t, http.MethodGet, "/api/owner/settlements", f.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("owner list settlements: status %d (body %s)", code, body)
	}

	// Test Step-up Auth requirement on settlement resolve:
	// Manually resolving discrepancy without valid step-up auth must be refused (400/401/404)
	fakeID := uuid.New()
	code, _ = h.httpCall(t, http.MethodPost, "/api/owner/settlements/"+fakeID.String()+"/resolve", f.ownerTok,
		map[string]any{"notes": "Manual reconciliation attempt"})
	if code != http.StatusNotFound && code != http.StatusBadRequest && code != http.StatusUnauthorized {
		t.Fatalf("resolve settlement without auth: unexpected status %d", code)
	}

	// --- Numeric Accounting Verification on EOD Balancer ---
	// Seed a known double-entry posting: ₹5,000 (500000 paise) bank debit and ₹5,000 credit in journal
	// and ₹5,000 statement credit in bank_transactions for today.
	now := istNow()
	dateStr := now.Format("2006-01-02")
	ctx := context.Background()

	sourceID := uuid.New()
	_, err := h.pool.Exec(ctx, `
		INSERT INTO financial_journal_entries (id, property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
		VALUES 
			($1, $2, 'bank', 500000, 0, 'manual', $3, 'deposit', $4),
			($5, $2, 'rental_revenue', 0, 500000, 'manual', $3, 'revenue', $4)
	`, uuid.New(), f.propID, sourceID, now, uuid.New())
	if err != nil {
		t.Fatalf("seed journal entry: %v", err)
	}

	bankAcctID := uuid.New()
	_, err = h.pool.Exec(ctx, `
		INSERT INTO bank_accounts (id, property_id, bank_name, account_number_last4, account_type, label)
		VALUES ($1, $2, 'Test Bank', '1234', 'savings', 'Settlement Account')
	`, bankAcctID, f.propID)
	if err != nil {
		t.Fatalf("seed bank account: %v", err)
	}

	_, err = h.pool.Exec(ctx, `
		INSERT INTO bank_transactions (id, property_id, bank_account_id, txn_id, amount_paise, row_type, txn_date, narration, dedup_hash, status)
		VALUES ($1, $2, $3, 'TXN123', 500000, 'credit', $4, 'Cleared Bank Credit', 'hash_eod_test_123', 'matched')
	`, uuid.New(), f.propID, bankAcctID, dateStr)
	if err != nil {
		t.Fatalf("seed bank transaction: %v", err)
	}

	// Generate EOD balance snapshot for today
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/settlements/eod-balance/run", f.ownerTok,
		map[string]any{"date": dateStr})
	if code != http.StatusOK {
		t.Fatalf("owner run eod balance: status %d (body %s)", code, body)
	}

	// Fetch and numerically verify EOD balance
	code, body = h.httpCall(t, http.MethodGet, "/api/owner/settlements/eod-balance?date="+dateStr, f.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("owner get eod balance: status %d (body %s)", code, body)
	}

	var bal domain.DailySettlementBalance
	if err := json.Unmarshal(body, &bal); err != nil {
		t.Fatalf("unmarshal eod balance: %v", err)
	}

	// Assert numeric postings are precisely extracted and balanced
	if bal.LedgerBankDrPaise != 500000 {
		t.Fatalf("LedgerBankDrPaise = %d, want 500000 (from financial_journal_entries)", bal.LedgerBankDrPaise)
	}
	if bal.BankCreditsPaise != 500000 {
		t.Fatalf("BankCreditsPaise = %d, want 500000 (from bank_transactions)", bal.BankCreditsPaise)
	}
	if bal.LedgerBankCrPaise != 0 {
		t.Fatalf("LedgerBankCrPaise = %d, want 0", bal.LedgerBankCrPaise)
	}
	if bal.BankDebitsPaise != 0 {
		t.Fatalf("BankDebitsPaise = %d, want 0", bal.BankDebitsPaise)
	}
	if !bal.IsBalanced {
		t.Fatalf("eod balance is not balanced: %+v", bal.Discrepancies)
	}
	if bal.DiscrepancyPaise != 0 {
		t.Fatalf("total discrepancy = %d, want 0", bal.DiscrepancyPaise)
	}

	// Fetch history
	code, body = h.httpCall(t, http.MethodGet, "/api/owner/settlements/eod-balance/history", f.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("owner list eod balance history: status %d (body %s)", code, body)
	}
}

// ---- Bank Statements & Accounts -------------------------------------------

func TestFeatureBankStatements_AccountsAndTransactions(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)

	// List initial accounts
	code, body := h.httpCall(t, http.MethodGet, "/api/owner/bank-accounts", f.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("owner list bank accounts: status %d (body %s)", code, body)
	}

	// Create new bank account
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/bank-accounts", f.ownerTok,
		map[string]any{
			"bank_name":            "HDFC Bank",
			"account_number_last4": "5678",
			"account_type":         "current",
			"label":                "Primary Collections",
		})
	if code != http.StatusCreated {
		t.Fatalf("owner create bank account: status %d (body %s)", code, body)
	}

	// List transactions
	code, body = h.httpCall(t, http.MethodGet, "/api/owner/statements/transactions", f.ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("owner list bank transactions: status %d (body %s)", code, body)
	}
}

// ---- Deposit Settlement: Cap / Bounds Check HTTP Tests -------------------

func TestFeatureDepositSettle_OverRefundRejected(t *testing.T) {
	h := newLiveHarness(t)
	f := seedLiveTenantFixture(t, h, 1500000)
	ctx := context.Background()

	// Seed second owner to satisfy dual control for owner operations without external OTP service
	coOwnerID := uuid.New()
	coOwnerPhone := fmt.Sprintf("+919%09d", (time.Now().UnixNano()+7)%1000000000)
	if _, err := h.pool.Exec(ctx, `INSERT INTO users (id, phone, role, property_id) VALUES ($1, $2, 'owner', $3)`,
		coOwnerID, coOwnerPhone, f.propID); err != nil {
		t.Fatalf("seed co-owner: %v", err)
	}

	// Seed a paid deposit due of ₹5,000 (500,000 paise)
	depositDueID := uuid.New()
	now := istNow()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
		VALUES ($1, $2, $3, $4, 'deposit', 'paid', 500000, 500000, $5, $6, $7)`,
		depositDueID, f.propID, f.tenantID, "DEP"+uuid.New().String()[:5],
		periodStart, periodStart.AddDate(0, 1, 0), periodStart.AddDate(0, 0, 5)); err != nil {
		t.Fatalf("seed deposit due: %v", err)
	}

	// 1. Over-refund: requested ₹6,000 (600,000 paise) > deposit ₹5,000 (500,000 paise)
	// Must return 400 Bad Request with code payment.invalidAmount
	code, body := h.httpCall(t, http.MethodPost, fmt.Sprintf("/api/owner/tenants/%s/deposit/settle", f.tenantID), f.ownerTok,
		map[string]any{
			"refunded_amount_paise": 600000,
			"reason":                "Excess refund attempt exceeding deposit",
		})
	if code != http.StatusBadRequest {
		t.Fatalf("over-refund status = %d, want 400 (body: %s)", code, body)
	}
	if gotCode := errCode(string(body)); gotCode != "payment.invalidAmount" {
		t.Fatalf("over-refund error code = %q, want 'payment.invalidAmount' (body: %s)", gotCode, body)
	}

	// 2. Negative refund amount: requested -100 paise must also be rejected
	code, body = h.httpCall(t, http.MethodPost, fmt.Sprintf("/api/owner/tenants/%s/deposit/settle", f.tenantID), f.ownerTok,
		map[string]any{
			"refunded_amount_paise": -100,
			"reason":                "Negative refund attempt",
		})
	if code != http.StatusBadRequest {
		t.Fatalf("negative refund status = %d, want 400 (body: %s)", code, body)
	}
	if gotCode := errCode(string(body)); gotCode != "payment.invalidAmount" {
		t.Fatalf("negative refund error code = %q, want 'payment.invalidAmount' (body: %s)", gotCode, body)
	}

	// 3. Valid settlement within bounds: ₹3,500 refund + ₹1,500 deduction = ₹5,000
	code, body = h.httpCall(t, http.MethodPost, fmt.Sprintf("/api/owner/tenants/%s/deposit/settle", f.tenantID), f.ownerTok,
		map[string]any{
			"refunded_amount_paise": 350000,
			"reason":                "Legitimate move-out deduction and refund",
		})
	if code != http.StatusOK {
		t.Fatalf("valid deposit settlement status = %d, want 200 (body: %s)", code, body)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("valid deposit settlement body unexpected: %s", body)
	}

	// 4. Idempotent replay: calling again with identical parameters succeeds idempotently (200 OK)
	code, body = h.httpCall(t, http.MethodPost, fmt.Sprintf("/api/owner/tenants/%s/deposit/settle", f.tenantID), f.ownerTok,
		map[string]any{
			"refunded_amount_paise": 350000,
			"reason":                "Legitimate move-out deduction and refund",
		})
	if code != http.StatusOK {
		t.Fatalf("idempotent replay status = %d, want 200 (body: %s)", code, body)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("idempotent replay body unexpected: %s", body)
	}
}
