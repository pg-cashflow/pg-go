package api

// Live end-to-end journey over HTTP against PostgreSQL.
//
// Unlike TestE2EMoneyLifecycle_DoubleEntryConservation, which calls services
// directly after its webhook, this test drives every step through the real
// router: owner and tenant requests carry real JWTs, the tenant's session is
// checked against the live tenant row, the webhook is a signed HTTP POST, the
// ledger is updated by the real outbox worker, and the owner reads the results
// back through the dashboard and payments endpoints.
//
// Steps that have no HTTP entry point are seeded with SQL and marked as such:
//   - the property, the owner user and the tenant's login user row (login is OTP
//     via SMS, which the test cannot receive)
//   - the rent due (normally produced by the billing job)
//   - the Cashfree payment intent (normally created by GET /api/tenant/dues/:id/pay,
//     which calls Cashfree's order API and so cannot run in CI)

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/gamification"
	"github.com/pg-cashflow/pg-go/internal/kyc"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/tenant"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// liveHarness is a router wired to live Postgres repositories, the same way
// cmd/server/main.go wires them, restricted to what the journey needs.
type liveHarness struct {
	pool     *pgxpool.Pool
	router   http.Handler
	secret   string
	whSecret string
	dueRepo  *postgres.DueRepo
	payRepo  *postgres.PaymentRepo
	intents  *postgres.PaymentIntentRepo
	tenants  *postgres.TenantRepo
	users    *postgres.UserRepo
	outbox   *postgres.LedgerOutboxRepo
	finSvc   *finance.Service
	ledger   *finance.LedgerOutboxWorker
}

func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	_ = godotenv.Load("../../.env")
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed")
	}

	u, err := url.Parse(cfg.DatabaseURL)
	if err != nil || strings.TrimPrefix(u.Path, "/") != "pg_test" {
		t.Fatalf("SAFETY CHECK: live test harness requires disposable database 'pg_test', got %q", u.Path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("cannot connect to Postgres: %v", err))
	}
	t.Cleanup(pool.Close)

	h := &liveHarness{
		pool:     pool,
		secret:   "journey-live-secret-key-32-bytes!!",
		whSecret: "whsec_journey_live_32_bytes_long!!",
		dueRepo:  postgres.NewDueRepo(pool),
		payRepo:  postgres.NewPaymentRepo(pool),
		intents:  postgres.NewPaymentIntentRepo(pool),
		tenants:  postgres.NewTenantRepo(pool),
		users:    postgres.NewUserRepo(pool),
		outbox:   postgres.NewLedgerOutboxRepo(pool),
	}
	propRepo := postgres.NewPropertyRepo(pool)
	summaryRepo := payment.NewSQLSummaryRepository(pool)
	h.finSvc = finance.NewService(postgres.NewFinanceRepo(pool), nil)
	paySvc := payment.NewService(h.dueRepo, h.payRepo, h.tenants, summaryRepo, &events.NoopPublisher{})
	h.ledger = finance.NewLedgerOutboxWorker(pool, h.outbox, h.finSvc)
	eventRepo := postgres.NewEventRepo(pool)
	tenantSvc := tenant.NewServiceWithPool(pool, h.tenants, h.dueRepo, eventRepo, postgres.NewPushRepo(pool))
	gamRepo := postgres.NewGamificationRepo(pool)
	gamSvc := gamification.NewService(gamRepo, h.tenants, h.dueRepo, &events.NoopPublisher{}, nil)
	kycSvc := kyc.NewService(postgres.NewKYCRepo(pool), nil, kyc.Config{
		IdentitySecret:           "test-identity-secret-key-32-bytes!!",
		HashKeyVersion:           1,
		VerificationValidityDays: 365,
	})
	collectorSvc := collector.New(h.intents, nil)
	bankAcctRepo := postgres.NewBankAccountRepo(pool)
	bankTxnRepo := postgres.NewBankTransactionRepo(pool)
	settlementRepo := postgres.NewSettlementRepo(pool)
	settlementBalancerRepo := postgres.NewSettlementBalancerRepo(pool)

	gin.SetMode(gin.TestMode)
	h.router = NewRouter(Deps{
		JWTSecret:              h.secret,
		CashfreeSecret:         h.whSecret,
		PropertyStore:          propRepo,
		UserStore:              h.users,
		AuthUserRepo:           h.users,
		TenantStore:            h.tenants,
		AuthTenantRepo:         h.tenants,
		DueStore:               h.dueRepo,
		ReportStore:            postgres.NewPaymentReportRepo(pool),
		PaymentStore:           h.payRepo,
		GatewayPaymentRepo:     h.payRepo,
		IntentStore:            h.intents,
		Payments:               paySvc,
		Tenants:                tenantSvc,
		Finance:                h.finSvc,
		FinanceEnabled:         true,
		Pool:                   pool,
		LedgerOutboxRepo:       h.outbox,
		GamificationStore:      gamRepo,
		Gamification:           gamSvc,
		KYCSvc:                 kycSvc,
		Collector:              collectorSvc,
		BankAccountRepo:        bankAcctRepo,
		BankTxnRepo:            bankTxnRepo,
		SettlementRepo:         settlementRepo,
		SettlementBalancerRepo: settlementBalancerRepo,
		PayoutRepo:             postgres.NewPayoutRepo(pool),
	})
	return h
}

// scrub removes everything the journey wrote for one property. Ledger tables
// need the maintenance flag, as in the E2E test.
func (h *liveHarness) scrub(propID uuid.UUID) {
	ctx := context.Background()
	_, _ = h.pool.Exec(ctx, "ALTER TABLE financial_corrections DISABLE TRIGGER trg_financial_corrections_immutable;")
	_, _ = h.pool.Exec(ctx, "ALTER TABLE daily_settlement_balance_runs DISABLE TRIGGER trg_prevent_modification;")

	_ = postgres.WithinTx(ctx, h.pool, func(tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on';")
		for _, q := range []string{
			"DELETE FROM tenant_departures WHERE property_id = $1",
			"DELETE FROM kyc_verification WHERE tenant_id IN (SELECT id FROM tenants WHERE property_id = $1)",
			"DELETE FROM kyc_consent WHERE tenant_id IN (SELECT id FROM tenants WHERE property_id = $1)",
			"DELETE FROM points_ledger WHERE property_id = $1",
			"DELETE FROM tenant_streaks WHERE property_id = $1",
			"DELETE FROM daily_settlement_balance_runs WHERE property_id = $1",
			"DELETE FROM daily_settlement_balances WHERE property_id = $1",
			"DELETE FROM gateway_settlements WHERE property_id = $1",
			"DELETE FROM deposit_settlements WHERE property_id = $1",
			"DELETE FROM financial_corrections WHERE property_id = $1",
			"DELETE FROM bank_transactions WHERE bank_account_id IN (SELECT id FROM bank_accounts WHERE property_id = $1)",
			"DELETE FROM bank_accounts WHERE property_id = $1",
			"DELETE FROM financial_journal_entries WHERE property_id = $1",
			"DELETE FROM ledger_outbox_events WHERE property_id = $1",
			"DELETE FROM payment_allocations WHERE payment_id IN (SELECT id FROM payments WHERE property_id = $1)",
			"DELETE FROM payments WHERE property_id = $1",
			"DELETE FROM payment_intent_dues WHERE due_id IN (SELECT id FROM dues WHERE property_id = $1)",
			"DELETE FROM payment_intents WHERE due_id IN (SELECT id FROM dues WHERE property_id = $1)",
			"DELETE FROM dues WHERE property_id = $1",
			"DELETE FROM users WHERE property_id = $1",
			"DELETE FROM tenants WHERE property_id = $1",
			"DELETE FROM properties WHERE id = $1",
		} {
			_, _ = tx.Exec(ctx, q, propID)
		}
		return nil
	})

	_, _ = h.pool.Exec(ctx, "ALTER TABLE financial_corrections ENABLE TRIGGER trg_financial_corrections_immutable;")
	_, _ = h.pool.Exec(ctx, "ALTER TABLE daily_settlement_balance_runs ENABLE TRIGGER trg_prevent_modification;")
}

// httpCall sends one request through the harness router.
func (h *liveHarness) httpCall(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// signedWebhook posts a Cashfree PAYMENT_SUCCESS_WEBHOOK with a valid HMAC.
func (h *liveHarness) signedWebhook(t *testing.T, orderID string, cfPaymentID int, amountRupees float64, utr string) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": map[string]any{
			"order":   map[string]any{"order_id": orderID},
			"payment": map[string]any{"cf_payment_id": cfPaymentID, "payment_amount": amountRupees, "bank_reference": utr},
		},
	})
	if err != nil {
		t.Fatalf("marshal webhook: %v", err)
	}
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(h.whSecret))
	mac.Write([]byte(ts + string(payload)))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// drainLedger runs the real outbox worker until no event is left to process.
func (h *liveHarness) drainLedger(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		n, err := h.ledger.ProcessBatch(ctx, 100)
		if err != nil {
			t.Fatalf("ledger worker: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatalf("ledger outbox still had events after 20 batches")
}

func istNow() time.Time {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return time.Now().UTC().Add(5*time.Hour + 30*time.Minute)
	}
	return time.Now().In(loc)
}

func TestJourney_OwnerTenantDueCheckoutWebhookLedgerVacate(t *testing.T) {
	h := newLiveHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	propID := uuid.New()
	ownerID := uuid.New()
	tenantUserID := uuid.New()
	now := istNow()
	ownerPhone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	tenantPhone := fmt.Sprintf("+918%09d", time.Now().UnixNano()%1000000000)

	h.scrub(propID)
	t.Cleanup(func() { h.scrub(propID) })

	// --- Seed: property and owner login (SQL; see package comment) ----------
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Journey Live PG', '1 Ledger Way', $2, 'journey@upi', 'Journey Owner', 'owner@journey.test', $3)`,
		propID, ownerPhone, uuid.New().String()[:8]); err != nil {
		t.Fatalf("seed property: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO users (id, phone, role, property_id) VALUES ($1, $2, 'owner', $3)`,
		ownerID, ownerPhone, propID); err != nil {
		t.Fatalf("seed owner user: %v", err)
	}
	owner := &domain.User{ID: ownerID, Role: domain.RoleOwner, Phone: ownerPhone, PropertyID: &propID}
	ownerTok, err := auth.IssueToken(h.secret, owner)
	if err != nil {
		t.Fatalf("issue owner token: %v", err)
	}

	// --- 1. Owner onboards a walk-in tenant over HTTP -----------------------
	code, body := h.httpCall(t, http.MethodPost, "/api/owner/tenants", ownerTok, map[string]any{
		"name": "Aditya Sharma", "phone": tenantPhone, "room_number": "101",
		"rent_amount": 1500000, "due_day": 5, "deposit_amount": 1500000,
	})
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("1. owner create tenant: status %d body %s", code, body)
	}
	var tenant struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(body, &tenant); err != nil || tenant.ID == uuid.Nil {
		t.Fatalf("1. create tenant returned no id: %s", body)
	}

	// Login user row for the tenant, linked to the tenant (SQL; see package comment).
	if _, err := h.pool.Exec(ctx, `INSERT INTO users (id, phone, role, property_id, tenant_id) VALUES ($1, $2, 'tenant', $3, $4)`,
		tenantUserID, tenantPhone, propID, tenant.ID); err != nil {
		t.Fatalf("seed tenant login user: %v", err)
	}
	tenantUser := &domain.User{ID: tenantUserID, Role: domain.RoleTenant, Phone: tenantPhone, PropertyID: &propID, TenantID: &tenant.ID}
	tenantTok, err := auth.IssueToken(h.secret, tenantUser)
	if err != nil {
		t.Fatalf("issue tenant token: %v", err)
	}

	// --- 2. Rent due for the current IST month (SQL; billing job in production)
	dueID := uuid.New()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	dueDate := periodStart.AddDate(0, 0, 4)
	cfOrderID := "cf_order_journey_" + uuid.New().String()[:12]
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
		VALUES ($1, $2, $3, $4, 'rent', 'pending', 1500000, 1500000, $5, $6, $7)`,
		dueID, propID, tenant.ID, "J"+uuid.New().String()[:7], periodStart, periodEnd, dueDate); err != nil {
		t.Fatalf("2. seed due: %v", err)
	}

	// --- 3. Owner and tenant both see the due through their own endpoints ---
	code, body = h.httpCall(t, http.MethodGet, "/api/owner/dues", ownerTok, nil)
	if code != http.StatusOK || !strings.Contains(string(body), dueID.String()) {
		t.Fatalf("3a. owner dues: status %d, due not listed: %s", code, body)
	}
	code, body = h.httpCall(t, http.MethodGet, "/api/tenant/dues", tenantTok, nil)
	if code != http.StatusOK || !strings.Contains(string(body), dueID.String()) {
		t.Fatalf("3b. tenant dues: status %d, due not listed: %s", code, body)
	}

	// --- 4. Checkout intent (SQL; see package comment) ----------------------
	exp := time.Now().Add(time.Hour)
	if err := h.intents.Create(ctx, &domain.PaymentIntent{
		ID: uuid.New(), DueID: dueID, Provider: "cashfree",
		ProviderOrderID: cfOrderID, AmountPaise: 1500000, Status: domain.IntentCreated, ExpiresAt: &exp,
	}); err != nil {
		t.Fatalf("4. create intent: %v", err)
	}

	// --- 5. Signed webhook settles the due ----------------------------------
	cfPaymentID := int(time.Now().UnixNano()%899999999 + 100000000)
	utr := fmt.Sprintf("UTR_JOURNEY_%d", cfPaymentID)
	code, body = h.signedWebhook(t, cfOrderID, cfPaymentID, 15000.0, utr)
	if code != http.StatusOK {
		t.Fatalf("5. signed webhook: status %d body %s", code, body)
	}
	var dueStatus string
	if err := h.pool.QueryRow(ctx, `SELECT status FROM dues WHERE id = $1`, dueID).Scan(&dueStatus); err != nil {
		t.Fatalf("5. read due: %v", err)
	}
	if dueStatus != string(domain.DueStatusPaid) {
		t.Fatalf("5. due status after webhook %q, want paid", dueStatus)
	}

	// --- 6. Replaying the identical webhook must not double-collect ---------
	code, body = h.signedWebhook(t, cfOrderID, cfPaymentID, 15000.0, utr)
	if code != http.StatusOK {
		t.Fatalf("6. replayed webhook: status %d body %s", code, body)
	}
	var payments int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE due_id = $1`, dueID).Scan(&payments); err != nil {
		t.Fatalf("6. count payments: %v", err)
	}
	if payments != 1 {
		t.Fatalf("6. payments recorded after replay = %d, want exactly 1", payments)
	}

	// --- 7. Ledger worker posts the collection; books must balance ----------
	h.drainLedger(t)
	var debits, credits int64
	if err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_paise),0), COALESCE(SUM(credit_paise),0)
		FROM financial_journal_entries WHERE property_id = $1`, propID).Scan(&debits, &credits); err != nil {
		t.Fatalf("7. ledger totals: %v", err)
	}
	if debits == 0 || debits != credits {
		t.Fatalf("7. ledger after worker: debits=%d credits=%d, want equal and non-zero", debits, credits)
	}

	// --- 8. Owner dashboard and payments reflect the collection -------------
	period := now.Format("2006-01")
	code, body = h.httpCall(t, http.MethodGet, "/api/owner/dashboard/summary?period="+period, ownerTok, nil)
	if code != http.StatusOK || !strings.Contains(string(body), `"total_collected_paise":1500000`) {
		t.Fatalf("8a. dashboard for %s: status %d, want total_collected_paise 1500000 (body %s)", period, code, body)
	}
	code, body = h.httpCall(t, http.MethodGet, "/api/owner/payments", ownerTok, nil)
	if code != http.StatusOK || !strings.Contains(string(body), utr) {
		t.Fatalf("8b. owner payments: status %d, UTR %s not listed (body %s)", code, utr, body)
	}

	// --- 9. Tenant can read their own profile while active ------------------
	code, _ = h.httpCall(t, http.MethodGet, "/api/tenant/me", tenantTok, nil)
	if code != http.StatusOK {
		t.Fatalf("9. active tenant /me: status %d, want 200", code)
	}

	// --- 10. Owner vacates the tenant; the tenant is cut off immediately ----
	code, body = h.httpCall(t, http.MethodPost, "/api/owner/tenants/"+tenant.ID.String()+"/vacate", ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("10a. vacate: status %d body %s", code, body)
	}
	code, body = h.httpCall(t, http.MethodGet, "/api/tenant/me", tenantTok, nil)
	if code != http.StatusForbidden || errCode(string(body)) != "auth.accessRevoked" {
		t.Fatalf("10b. vacated tenant /me: status %d code %q, want 403 auth.accessRevoked", code, errCode(string(body)))
	}
	code, body = h.httpCall(t, http.MethodGet, "/api/tenant/dues", tenantTok, nil)
	if code != http.StatusForbidden {
		t.Fatalf("10c. vacated tenant dues: status %d, want 403", code)
	}

	// --- 11. Final conservation check on the physical ledger ----------------
	h.drainLedger(t)
	var finalDiff int64
	if err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_paise) - SUM(credit_paise), 0)
		FROM financial_journal_entries WHERE property_id = $1`, propID).Scan(&finalDiff); err != nil {
		t.Fatalf("11. final ledger diff: %v", err)
	}
	if finalDiff != 0 {
		t.Fatalf("11. ledger imbalance at end of journey: %d paise", finalDiff)
	}
}
