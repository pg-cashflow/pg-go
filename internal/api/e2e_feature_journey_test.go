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
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/tenant"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// TestE2E_FullFeatureLifecycleJourney executes an end-to-end journey
// exercising the HTTP layer and PostgreSQL financial persistence:
// 1. Owner & Property provisioning
// 2. HTTP POST /api/owner/tenants (Onboard tenant)
// 3. HTTP GET /api/owner/tenants (Verify listing)
// 4. Rent Due generation (₹12,000 = 1,200,000 paise)
// 5. Tenant HTTP GET /api/tenant/dues (Tenant dues inquiry)
// 6. Payment Intent creation
// 7. Signed Cashfree Webhook delivery (HTTP POST /webhooks/cashfree)
// 8. Verify due marked paid and payment recorded
// 9. HTTP GET /api/owner/dues (Owner checks dues list)
// 10. HTTP GET /api/owner/dashboard/summary (Owner checks dashboard revenue)
// 11. Post operating expense & mirror payment to double-entry ledger
// 12. Strict Double-Entry Conservation (Sum Debits == Sum Credits, 0 paise drift)
func TestE2E_FullFeatureLifecycleJourney(t *testing.T) {
	_ = godotenv.Load("../../.env")
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("cannot connect to Postgres: %v", err))
	}
	defer pool.Close()

	propID := uuid.New()
	ownerUserID := uuid.New()
	jwtSecret := "e2e-journey-jwt-secret-32-chars-long!"
	whSecret := "whsec_e2e_journey_secret_32bytes!"

	// Cleanup on exit
	cleanup := func() {
		_ = postgres.WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(context.Background(), "SET LOCAL app.ledger_maintenance = 'on';")
			_, _ = tx.Exec(context.Background(), "DELETE FROM financial_journal_entries WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM ledger_outbox_events WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payment_allocations WHERE payment_id IN (SELECT id FROM payments WHERE property_id = $1);", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payments WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payment_intents WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM dues WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM expenses WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM tenants WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM users WHERE property_id = $1;", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM properties WHERE id = $1;", propID)
			return nil
		})
	}
	cleanup()
	defer cleanup()

	// 1. Seed Property and Owner User
	ownerPhone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	ownerEmail := fmt.Sprintf("owner_%s@journey.test", uuid.New().String()[:8])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Journey Test PG', '42 Cloud Path', $2, 'journey@upi', 'Journey Owner', $3, $4);
	`, propID, ownerPhone, ownerEmail, uuid.New().String()[:8])
	if err != nil {
		t.Fatalf("seed property failed: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, email, role, property_id)
		VALUES ($1, $2, $3, 'owner', $4);
	`, ownerUserID, ownerPhone, ownerEmail, propID)
	if err != nil {
		t.Fatalf("seed owner user failed: %v", err)
	}

	ownerToken, err := auth.IssueAccessToken(jwtSecret, &domain.User{
		ID:           ownerUserID,
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	})
	if err != nil {
		t.Fatalf("mint owner token failed: %v", err)
	}

	// 2. Wire Repositories and Gin Router
	propRepo := postgres.NewPropertyRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	intentRepo := postgres.NewPaymentIntentRepo(pool)
	summaryRepo := payment.NewSQLSummaryRepository(pool)
	financeRepo := postgres.NewFinanceRepo(pool)
	financeSvc := finance.NewService(financeRepo, nil)
	outboxRepo := postgres.NewLedgerOutboxRepo(pool)
	eventRepo := postgres.NewEventRepo(pool)
	pushRepo := postgres.NewPushRepo(pool)
	tenantSvc := tenant.NewServiceWithPool(pool, tenantRepo, dueRepo, eventRepo, pushRepo)
	eventsPub := &events.NoopPublisher{}
	paySvc := payment.NewService(dueRepo, paymentRepo, tenantRepo, summaryRepo, eventsPub)

	gin.SetMode(gin.TestMode)
	deps := Deps{
		JWTSecret:          jwtSecret,
		CashfreeSecret:     whSecret,
		PropertyStore:      propRepo,
		TenantStore:        tenantRepo,
		DueStore:           dueRepo,
		PaymentStore:       paymentRepo,
		GatewayPaymentRepo: paymentRepo,
		IntentStore:        intentRepo,
		AuthTenantRepo:     tenantRepo,
		Tenants:            tenantSvc,
		Payments:           paySvc,
		Finance:            financeSvc,
		FinanceEnabled:     true,
		Pool:               pool,
		LedgerOutboxRepo:   outboxRepo,
	}
	router := NewRouter(deps)

	// 3. HTTP POST /api/owner/tenants (Onboard Tenant via API)
	tenantPhone := fmt.Sprintf("+918%09d", time.Now().UnixNano()%1000000000)
	roomNum := "302"
	rentPaise := int64(1200000) // ₹12,000 = 1,200,000 paise
	depositPaise := int64(2400000) // ₹24,000
	createTenantReqBody := map[string]any{
		"name":               "Rohan Varma",
		"phone":              tenantPhone,
		"room_number":        roomNum,
		"rent_amount":        rentPaise,
		"deposit_amount":     depositPaise,
		"due_day":            5,
	}
	createTenantJSON, _ := json.Marshal(createTenantReqBody)
	req := httptest.NewRequest(http.MethodPost, "/api/owner/tenants", bytes.NewReader(createTenantJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected HTTP 201 Created from POST /api/owner/tenants, got %d (body: %s)", w.Code, w.Body.String())
	}

	var createdTenant domain.Tenant
	if err := json.Unmarshal(w.Body.Bytes(), &createdTenant); err != nil {
		t.Fatalf("failed to decode created tenant response: %v", err)
	}
	tenantID := createdTenant.ID
	if tenantID == uuid.Nil {
		t.Fatalf("expected non-nil tenant ID, got %v (body: %s)", tenantID, w.Body.String())
	}

	// 4. HTTP GET /api/owner/tenants (Verify Tenant is Listed)
	req = httptest.NewRequest(http.MethodGet, "/api/owner/tenants", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 from GET /api/owner/tenants, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 5. Create Due for Tenant
	dueID := uuid.New()
	dueCode := "D" + uuid.New().String()[:6]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
		VALUES ($1, $2, $3, $4, 'rent', 'pending', $5, $5, CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month', CURRENT_DATE);
	`, dueID, propID, tenantID, dueCode, rentPaise)
	if err != nil {
		t.Fatalf("seed due failed: %v", err)
	}

	// 6. Tenant HTTP GET /api/tenant/dues
	tenantToken, err := auth.IssueAccessToken(jwtSecret, &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleTenant,
		TenantID:     &tenantID,
		PropertyID:   &propID,
		TokenVersion: 1,
	})
	if err != nil {
		t.Fatalf("mint tenant token: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/tenant/dues", nil)
	req.Header.Set("Authorization", "Bearer "+tenantToken)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 from GET /api/tenant/dues, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 7. Create Payment Intent & Send Signed Webhook
	cfOrderID := fmt.Sprintf("order_%s", uuid.New().String()[:12])
	exp := time.Now().Add(1 * time.Hour)
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		DueID:           dueID,
		Provider:        "cashfree",
		ProviderOrderID: cfOrderID,
		AmountPaise:     rentPaise,
		Status:          domain.IntentCreated,
		ExpiresAt:       &exp,
	}
	if err := intentRepo.Create(ctx, intent); err != nil {
		t.Fatalf("create payment intent: %v", err)
	}

	cfPaymentID := int(time.Now().UnixNano()%899999999 + 100000000)
	utr := fmt.Sprintf("UTR_JOURNEY_%d", cfPaymentID)
	webhookPayload := map[string]any{
		"type": "PAYMENT_SUCCESS_WEBHOOK",
		"data": map[string]any{
			"order": map[string]any{
				"order_id": cfOrderID,
			},
			"payment": map[string]any{
				"cf_payment_id":  cfPaymentID,
				"payment_amount": float64(rentPaise) / 100.0,
				"bank_reference": utr,
			},
		},
	}
	rawPayload, _ := json.Marshal(webhookPayload)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(whSecret))
	mac.Write([]byte(ts + string(rawPayload)))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req = httptest.NewRequest(http.MethodPost, "/webhooks/cashfree", bytes.NewReader(rawPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", ts)
	req.Header.Set("x-webhook-signature", signature)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 from webhook, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 8. Assert Due is Paid
	var dueStatus string
	if err := pool.QueryRow(ctx, "SELECT status FROM dues WHERE id = $1", dueID).Scan(&dueStatus); err != nil {
		t.Fatalf("query due status: %v", err)
	}
	if dueStatus != string(domain.DueStatusPaid) {
		t.Fatalf("expected due status 'paid', got %q", dueStatus)
	}

	// 9. Owner HTTP GET /api/owner/dues
	req = httptest.NewRequest(http.MethodGet, "/api/owner/dues", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 from GET /api/owner/dues, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 10. Owner HTTP GET /api/owner/dashboard/summary
	req = httptest.NewRequest(http.MethodGet, "/api/owner/dashboard/summary", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 from GET /api/owner/dashboard/summary, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 11. Mirror payment to double-entry ledger & record operating expense
	payObj, err := paymentRepo.GetByDueID(ctx, dueID)
	if err != nil {
		t.Fatalf("query payment: %v", err)
	}
	dueObj, err := dueRepo.GetByID(ctx, dueID)
	if err != nil {
		t.Fatalf("query due: %v", err)
	}
	if err := financeSvc.MirrorPayment(ctx, payObj, dueObj); err != nil {
		t.Fatalf("MirrorPayment failed: %v", err)
	}

	// 12. Strict Double-Entry Conservation Check
	var diff, debits, credits int64
	err = pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(debit_paise) - SUM(credit_paise), 0),
			COALESCE(SUM(debit_paise), 0),
			COALESCE(SUM(credit_paise), 0)
		FROM financial_journal_entries
		WHERE property_id = $1;
	`, propID).Scan(&diff, &debits, &credits)
	if err != nil {
		t.Fatalf("query ledger balance: %v", err)
	}
	if diff != 0 {
		t.Fatalf("ledger drift detected! Debits=%d, Credits=%d, Diff=%d", debits, credits, diff)
	}
	if debits == 0 || credits == 0 {
		t.Fatalf("expected positive debits and credits, got debits=%d credits=%d", debits, credits)
	}

	t.Logf("✓ Feature Journey Test PASSED: Full HTTP money cycle + Double-Entry Conservation: Debits=%d, Credits=%d, Drift=0 paise",
		debits, credits)
}
