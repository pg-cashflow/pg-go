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
