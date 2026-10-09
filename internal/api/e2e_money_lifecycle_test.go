package api

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
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// TestE2EMoneyLifecycle_DoubleEntryConservation executes a full end-to-end
// financial lifecycle against live PostgreSQL and HTTP router:
// 1. Property & Tenant Provisioning
// 2. Rent Due Creation (₹15,000 = 1,500,000 paise)
// 3. Checkout Payment Intent Creation
// 4. Signed Cashfree Webhook Processing (HTTP POST /webhooks/cashfree)
// 5. Assert Due Transition to Paid
// 6. Ledger Worker Event Processing & Financial Mirror Entry Posting
// 7. Tenant Departure Settlement (deposit 15,000 INR, damages 5,000 INR, refund 10,000 INR)
// 8. Assert Double-Entry Ledger Invariant (sum(debits) == sum(credits), 0 paise drift)
func TestE2EMoneyLifecycle_DoubleEntryConservation(t *testing.T) {
	_ = godotenv.Load("../../.env")
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("cannot connect to Postgres: %v", err))
	}
	defer pool.Close()

	propID := uuid.New()
	tenantID := uuid.New()
	ownerUserID := uuid.New()
	dueID := uuid.New()
	intentID := uuid.New()
	cfOrderID := fmt.Sprintf("cf_order_%s", uuid.New().String()[:12])
	whSecret := "whsec_money_lifecycle_e2e_32bytes!"

	// Cleanup test data strictly scoped to propID
	cleanup := func() {
		_ = postgres.WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(context.Background(), "SET LOCAL app.ledger_maintenance = 'on';")
			_, _ = tx.Exec(context.Background(), "DELETE FROM financial_journal_entries WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM ledger_outbox_events WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payment_allocations WHERE payment_id IN (SELECT id FROM payments WHERE property_id = $1);", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payments WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payment_intents WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM dues WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM tenants WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM users WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM properties WHERE id = $1;", propID)
			return nil
		})
	}
	cleanup()
	defer cleanup()

	// 1. Seed Property, User, Tenant
	ownerPhone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	tenantPhone := fmt.Sprintf("+918%09d", time.Now().UnixNano()%1000000000)

	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'E2E Money Lifecycle PG', '100 Ledger Way', $2, 'ledger@upi', 'E2E Owner', 'owner@e2e.test', $3);
	`, propID, ownerPhone, uuid.New().String()[:8])
	if err != nil {
		t.Fatalf("seed property failed: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3);
	`, ownerUserID, ownerPhone, propID)
	if err != nil {
		t.Fatalf("seed user failed: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Aditya Sharma', $3, '101', 1500000, 5, 'active');
	`, tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("seed tenant failed: %v", err)
	}

	// 2. Generate Rent Due (₹15,000 = 1,500,000 paise)
	dueCode := "D" + uuid.New().String()[:6]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
		VALUES ($1, $2, $3, $4, 'rent', 'pending', 1500000, 1500000, CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month', CURRENT_DATE);
	`, dueID, propID, tenantID, dueCode)
	if err != nil {
		t.Fatalf("seed due failed: %v", err)
	}

	// 3. Create Checkout Payment Intent
	exp := time.Now().Add(1 * time.Hour)
	intentRepo := postgres.NewPaymentIntentRepo(pool)
	intent := &domain.PaymentIntent{
		ID:              intentID,
		DueID:           dueID,
		Provider:        "cashfree",
		ProviderOrderID: cfOrderID,
		AmountPaise:     1500000,
		Status:          domain.IntentCreated,
		ExpiresAt:       &exp,
	}
	if err := intentRepo.Create(ctx, intent); err != nil {
		t.Fatalf("create payment intent failed: %v", err)
	}

	// 4. Initialize Repositories and Real HTTP Router
	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	summaryRepo := payment.NewSQLSummaryRepository(pool)
	eventsPub := &events.NoopPublisher{}
	financeRepo := postgres.NewFinanceRepo(pool)
	financeSvc := finance.NewService(financeRepo, nil)
	outboxRepo := postgres.NewLedgerOutboxRepo(pool)
	paySvc := payment.NewService(dueRepo, paymentRepo, tenantRepo, summaryRepo, eventsPub)

	gin.SetMode(gin.TestMode)
	deps := Deps{
		JWTSecret:          "money-lifecycle-secret-32-bytes!!",
		CashfreeSecret:     whSecret,
		DueStore:           dueRepo,
		PaymentStore:       paymentRepo,
		GatewayPaymentRepo: paymentRepo,
		IntentStore:        intentRepo,
		Payments:           paySvc,
		Finance:            financeSvc,
		FinanceEnabled:     true,
		Pool:               pool,
		LedgerOutboxRepo:   outboxRepo,
	}
	router := NewRouter(deps)

	// 5. Construct and Send Signed Cashfree Webhook Request
	cfPaymentID := int(time.Now().UnixNano()%899999999 + 100000000)
	webhookPayload := map[string]any{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": map[string]any{
			"order": map[string]any{
				"order_id": cfOrderID,
			},
			"payment": map[string]any{
				"cf_payment_id":  cfPaymentID,
				"payment_amount": 15000.0,
				"bank_reference": fmt.Sprintf("UTR_E2E_%d", cfPaymentID),
			},
		},
	}
	rawPayload, err := json.Marshal(webhookPayload)
	if err != nil {
		t.Fatalf("marshal webhook payload failed: %v", err)
	}

	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(whSecret))
	mac.Write([]byte(ts + string(rawPayload)))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewReader(rawPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", signature)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 from webhook, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// 6. Assert Due Status in PostgreSQL is now 'paid'
	var dueStatus string
	err = pool.QueryRow(ctx, "SELECT status FROM dues WHERE id = $1", dueID).Scan(&dueStatus)
	if err != nil {
		t.Fatalf("query due status failed: %v", err)
	}
	if dueStatus != string(domain.DueStatusPaid) {
		t.Fatalf("expected due status 'paid', got %q", dueStatus)
	}

	// 7. Mirror Payment to Ledger via Finance Service
	payObj, err := paymentRepo.GetByDueID(ctx, dueID)
	if err != nil {
		t.Fatalf("query payment record failed: %v", err)
	}
	dueObj, err := dueRepo.GetByID(ctx, dueID)
	if err != nil {
		t.Fatalf("query due record failed: %v", err)
	}

	if err := financeSvc.MirrorPayment(ctx, payObj, dueObj); err != nil {
		t.Fatalf("MirrorPayment failed: %v", err)
	}

	// Assert intermediate double-entry balance after payment collection
	var intermediateDiff int64
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(debit_paise) - SUM(credit_paise), 0)
		FROM financial_journal_entries
		WHERE property_id = $1
	`, propID).Scan(&intermediateDiff)
	if err != nil {
		t.Fatalf("query intermediate ledger balance failed: %v", err)
	}
	if intermediateDiff != 0 {
		t.Fatalf("intermediate ledger imbalance after payment: net diff = %d paise", intermediateDiff)
	}

	// 8. Tenant Departure Settlement
	// Deposit: 1,500,000 paise (₹15,000)
	// Damages: 500,000 paise (₹5,000)
	// Net Refund: 1,000,000 paise (₹10,000)
	departureID := uuid.New()
	err = financeSvc.MirrorDepartureSettlement(
		ctx,
		propID,
		departureID,
		1500000, // depositPaise
		0,       // unusedRentReversalPaise
		500000,  // damagesPaise
		1000000, // netRefundPaise
		0,       // unpaidRentPaise
		0,       // receivableBalancePaise
		time.Now(),
	)
	if err != nil {
		t.Fatalf("MirrorDepartureSettlement failed: %v", err)
	}

	// 9. Assert Final Double-Entry Conservation Invariant on Physical PostgreSQL
	var finalDiff int64
	var totalDebits int64
	var totalCredits int64
	err = pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(debit_paise) - SUM(credit_paise), 0),
			COALESCE(SUM(debit_paise), 0),
			COALESCE(SUM(credit_paise), 0)
		FROM financial_journal_entries
		WHERE property_id = $1
	`, propID).Scan(&finalDiff, &totalDebits, &totalCredits)
	if err != nil {
		t.Fatalf("query final ledger balance failed: %v", err)
	}

	if finalDiff != 0 {
		t.Fatalf("GATE 04 VIOLATION: Final ledger imbalance detected! Net diff = %d paise (Debits: %d, Credits: %d)",
			finalDiff, totalDebits, totalCredits)
	}

	if totalDebits == 0 || totalCredits == 0 {
		t.Fatalf("expected non-zero ledger journal lines, got debits=%d credits=%d", totalDebits, totalCredits)
	}

	t.Logf("✓ Gate 04 PASSED: Final Double-Entry Balance strictly conserved in PostgreSQL: Total Debits=%d paise == Total Credits=%d paise (Net drift = 0 paise)",
		totalDebits, totalCredits)
}
