package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

type mockTenantStoreForPayouts struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (m *mockTenantStoreForPayouts) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if t, ok := m.tenants[id]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("tenant not found")
}
func (m *mockTenantStoreForPayouts) ListByProperty(_ context.Context, _ uuid.UUID) ([]domain.Tenant, error) {
	return nil, nil
}
func (m *mockTenantStoreForPayouts) Update(_ context.Context, _ *domain.Tenant) error {
	return nil
}
func (m *mockTenantStoreForPayouts) GetIDPhoto(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return nil, nil
}

type mockDualControlUserStore struct {
	UserStore
	owners []domain.User
}

func (m *mockDualControlUserStore) GetByPropertyAndRole(_ context.Context, _ uuid.UUID, _ domain.Role) ([]domain.User, error) {
	return m.owners, nil
}

type mockSMSForPayouts struct {
	sent []string
}

func (m *mockSMSForPayouts) Send(_ context.Context, phone, message string) error {
	m.sent = append(m.sent, message)
	return nil
}

func TestLivePayoutsAndDeparturesHTTPFlow(t *testing.T) {
	if testing.Short() {
		testutil.FailOnSkipIfDBRequired(t, "skipping live postgres test in short mode")
	}
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	_, _ = pool.Exec(ctx, `ALTER TABLE users ADD COLUMN IF NOT EXISTS token_version INT NOT NULL DEFAULT 1`)
	_ = postgres.Migrate(ctx, pool, filepath.Join("..", "..", "migrations"))

	propID := uuid.New()
	inviteCode := fmt.Sprintf("E%s", uuid.New().String()[:7])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Departure HTTP PG', '456 Test St', '+919999977777', 'owner@upi', 'Owner', 'owner@test.com', $2)`,
		propID, inviteCode,
	)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	ownerID := uuid.New()
	ownerPhone := fmt.Sprintf("+91%010d", time.Now().UnixNano()%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3)`, ownerID, ownerPhone, propID,
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, ownerID)
	}()

	payoutRepo := postgres.NewPayoutRepo(pool)

	tenantID := uuid.New()
	tPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+7)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
		VALUES ($1, $2, 'HTTP Tenant', $3, 600000, 1, 'active')`, tenantID, propID, tPhone,
	)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	dueID := uuid.New()
	dCode := uuid.New().String()[:8]
	pStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	pEnd := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
	dueDate := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
		VALUES ($1, $2, $3, $4, 'rent', 600000, 600000, $5, $6, $7, 'pending')`,
		dueID, dCode, tenantID, propID, pStart, pEnd, dueDate,
	)
	if err != nil {
		t.Fatalf("insert due: %v", err)
	}

	tenantStore := &mockTenantStoreForPayouts{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: {
				ID:         tenantID,
				PropertyID: propID,
				Name:       "HTTP Tenant",
				RentAmount: 600000,
			},
		},
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	userRepo := postgres.NewUserRepo(pool)
	mockSMS := &mockSMSForPayouts{}
	otpRepo := postgres.NewOTPRepo(pool)
	authSvc := auth.NewService(otpRepo, userRepo, nil, nil, mockSMS, "test_otp_secret_32_characters_long", cfg.JWTSecret)
	h := &Handlers{
		Deps: Deps{
			Pool:                 pool,
			PayoutRepo:           payoutRepo,
			TenantStore:          tenantStore,
			UserStore:            userRepo,
			Auth:                 authSvc,
			PayoutChecksumSecret: "test_secret_for_http_export",
		},
	}

	// Mount routes with owner claims injection
	owner := router.Group("/owner", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     ownerID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
	})
	{
		owner.POST("/tenants/:id/departures", h.OwnerCreateDeparture)
		owner.POST("/departures/:id/inspect", h.OwnerInspectDeparture)
		owner.POST("/departures/:id/deductions", h.OwnerAddDepartureDeduction)
		owner.POST("/departures/:id/settle", h.OwnerSettleDeparture)

		owner.POST("/payouts/payees", h.OwnerCreatePayee)
		owner.GET("/payouts/payees", h.OwnerListPayees)
		owner.GET("/payouts/items/unbatched", h.OwnerListUnbatchedPayoutItems)
		owner.POST("/payouts/batches", h.OwnerCreatePayoutBatch)
		owner.POST("/payouts/batches/:id/approve", h.OwnerApprovePayoutBatch)
		owner.POST("/payouts/batches/:id/approve/request-otp", h.OwnerRequestPayoutBatchOTP)
		owner.GET("/payouts/batches/:id/export", h.OwnerExportPayoutBatch)
	}

	// 1. Create Payee
	var payeeID uuid.UUID
	t.Run("POST /owner/payouts/payees", func(t *testing.T) {
		body := map[string]interface{}{
			"payee_type": "tenant_deposit",
			"name":       "Tenant Payout Payee",
			"phone":      "9876543210",
			"upi_vpa":    "tenant@upi",
		}
		jsonBytes, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/owner/payouts/payees", bytes.NewReader(jsonBytes))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Payee domain.PayoutPayee `json:"payee"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		payeeID = resp.Payee.ID
		if payeeID == uuid.Nil {
			t.Fatalf("expected valid payee id, got nil")
		}
	})

	// 2. List Payees
	t.Run("GET /owner/payouts/payees", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/owner/payouts/payees", nil)
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Payees []domain.PayoutPayee `json:"payees"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Payees) == 0 {
			t.Fatalf("expected at least 1 payee, got 0")
		}
	})

	// 3. Create Departure
	var depID uuid.UUID
	t.Run("POST /owner/tenants/:id/departures", func(t *testing.T) {
		depAmount := int64(1200000) // ₹12,000 deposit
		notes := "Tenant moving out for semester break"
		body := map[string]interface{}{
			"planned_vacate_date":  "2026-09-15",
			"deposit_amount_paise": depAmount,
			"notes":                notes,
		}
		jsonBytes, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/tenants/%s/departures", tenantID), bytes.NewReader(jsonBytes))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Departure domain.TenantDeparture `json:"departure"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		depID = resp.Departure.ID
		if depID == uuid.Nil {
			t.Fatalf("expected valid departure id, got nil")
		}
		if resp.Departure.DepositAmountPaise != 1200000 {
			t.Errorf("expected deposit 1200000, got %d", resp.Departure.DepositAmountPaise)
		}
	})

	// 4. Inspect Departure (starts 24h SLA)
	t.Run("POST /owner/departures/:id/inspect", func(t *testing.T) {
		body := map[string]interface{}{
			"notes": "Keys received, minor wall scuffs",
		}
		jsonBytes, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/departures/%s/inspect", depID), bytes.NewReader(jsonBytes))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 5. Add Departure Deduction
	t.Run("POST /owner/departures/:id/deductions", func(t *testing.T) {
		body := map[string]interface{}{
			"description":  "Wall touch-up paint",
			"amount_paise": 150000, // ₹1,500
			"status":       "agreed",
		}
		jsonBytes, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/departures/%s/deductions", depID), bytes.NewReader(jsonBytes))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 6. Settle Departure: Prorated rent ₹3,000 (300000 paise) for 15 days
	// Deposit: ₹12,000 (1200000). Deductions: ₹1,500 (150000). Prorated rent: ₹3,000 (300000).
	// Net refund: 1200000 - 300000 - 150000 = 750000 (₹7,500).
	t.Run("POST /owner/departures/:id/settle", func(t *testing.T) {
		body := map[string]interface{}{
			"actual_vacate_date":  "2026-09-15",
			"prorated_rent_paise": 300000,
			"payee_id":            payeeID,
		}
		jsonBytes, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/departures/%s/settle", depID), bytes.NewReader(jsonBytes))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			NetRefundPaise int64              `json:"net_refund_paise"`
			PayoutItem     *domain.PayoutItem `json:"payout_item"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.NetRefundPaise != 750000 {
			t.Errorf("expected net refund 750000, got %d", resp.NetRefundPaise)
		}
		if resp.PayoutItem == nil || resp.PayoutItem.AmountPaise != 750000 {
			t.Errorf("expected payout item 750000, got %v", resp.PayoutItem)
		}
	})

	// 7. List Unbatched Payout Items
	t.Run("GET /owner/payouts/items/unbatched", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/owner/payouts/items/unbatched", nil)
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Items []domain.PayoutItem `json:"items"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if len(resp.Items) == 0 {
			t.Fatalf("expected unbatched items, got 0")
		}
	})

	// 8. Create Payout Batch
	var batchID uuid.UUID
	t.Run("POST /owner/payouts/batches", func(t *testing.T) {
		notes := "Approved September Disbursals"
		body := map[string]interface{}{
			"notes": notes,
		}
		jsonBytes, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/owner/payouts/batches", bytes.NewReader(jsonBytes))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Batch domain.PayoutBatch  `json:"batch"`
			Items []domain.PayoutItem `json:"items"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		batchID = resp.Batch.ID
		if batchID == uuid.Nil {
			t.Fatalf("expected valid batch id, got nil")
		}
		if resp.Batch.FileChecksum == nil || *resp.Batch.FileChecksum == "" {
			t.Fatalf("expected batch file checksum to be populated")
		}
	})

	// 9. Draft Export Gating & Dual-Control Approval Flow
	t.Run("Draft export gate and approval flow", func(t *testing.T) {
		var logBuf bytes.Buffer
		captureLogger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		prevLogger := slog.Default()
		slog.SetDefault(captureLogger)
		defer slog.SetDefault(prevLogger)

		// Attempt export while in draft - must be blocked with 409 Conflict
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/owner/payouts/batches/%s/export", batchID), nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict exporting draft batch, got %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(logBuf.String(), "audit=payout_export") {
			t.Fatalf("refused draft export must not emit audit line, got: %s", logBuf.String())
		}
		logBuf.Reset()

		// Affirmative mismatch rejection (mismatched paise)
		mismatchBody := map[string]interface{}{
			"expected_item_count":  1,
			"expected_total_paise": 999999,
			"reauth_confirmation":  "CONFIRM_TEST",
		}
		mismatchJSON, _ := json.Marshal(mismatchBody)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", batchID), bytes.NewReader(mismatchJSON))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request on affirmative mismatch, got %d: %s", w.Code, w.Body.String())
		}

		// Arbitrary placeholder string ("CONFIRM_TEST") is strictly rejected (401 Unauthorized)
		fakeReauthBody := map[string]interface{}{
			"expected_item_count":  1,
			"expected_total_paise": 750000,
			"reauth_confirmation":  "CONFIRM_TEST",
		}
		fakeJSON, _ := json.Marshal(fakeReauthBody)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", batchID), bytes.NewReader(fakeJSON))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized with arbitrary placeholder string, got %d: %s", w.Code, w.Body.String())
		}

		// Missing credentials entirely
		noReauthBody := map[string]interface{}{
			"expected_item_count":  1,
			"expected_total_paise": 750000,
		}
		noReauthJSON, _ := json.Marshal(noReauthBody)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", batchID), bytes.NewReader(noReauthJSON))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized without credentials, got %d: %s", w.Code, w.Body.String())
		}

		// Request OTP trigger endpoint
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve/request-otp", batchID), nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK requesting OTP, got %d: %s", w.Code, w.Body.String())
		}
		if len(mockSMS.sent) == 0 {
			t.Fatalf("expected SMS to be sent")
		}
		smsMsg := mockSMS.sent[len(mockSMS.sent)-1]
		if !strings.Contains(smsMsg, "Your payout approval code is") {
			t.Fatalf("expected payout approval copy in SMS, got: %s", smsMsg)
		}
		smsParts := strings.Split(smsMsg, " ")
		approvalCode := strings.TrimSuffix(smsParts[5], ".")

		// Wrong OTP rejection
		wrongBody := map[string]interface{}{
			"expected_item_count":  1,
			"expected_total_paise": 750000,
			"otp":                  "000000",
		}
		wrongJSON, _ := json.Marshal(wrongBody)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", batchID), bytes.NewReader(wrongJSON))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized with wrong OTP, got %d: %s", w.Code, w.Body.String())
		}

		// Valid approval with affirmative matching and real OTP
		validBody := map[string]interface{}{
			"expected_item_count":  1,
			"expected_total_paise": 750000,
			"otp":                  approvalCode,
		}
		validJSON, _ := json.Marshal(validBody)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", batchID), bytes.NewReader(validJSON))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK approving batch with valid OTP, got %d: %s", w.Code, w.Body.String())
		}

		// Replay attack with same OTP must be rejected (401 Unauthorized or 409 Conflict)
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", batchID), bytes.NewReader(validJSON))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusConflict && w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized or 409 Conflict on replay, got %d: %s", w.Code, w.Body.String())
		}

		// Export Payout Batch CSV after approval - must succeed
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, fmt.Sprintf("/owner/payouts/batches/%s/export", batchID), nil)
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK after approval, got %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
			t.Errorf("expected Content-Type text/csv, got %s", ct)
		}
		if chk := w.Header().Get("X-Batch-Checksum"); chk == "" {
			t.Errorf("expected non-empty X-Batch-Checksum header")
		}
		csvContent := w.Body.String()
		if !strings.Contains(csvContent, "Reference Number") || !strings.Contains(csvContent, "Amount (INR)") {
			t.Errorf("expected CSV header columns, got:\n%s", csvContent)
		}
		if !strings.Contains(csvContent, "7500.00") {
			t.Errorf("expected ₹7500.00 in exported CSV, got:\n%s", csvContent)
		}

		auditOutput := logBuf.String()
		if !strings.Contains(auditOutput, "audit=payout_export") {
			t.Errorf("expected audit=payout_export in log, got: %s", auditOutput)
		}
		if !strings.Contains(auditOutput, batchID.String()) {
			t.Errorf("expected batch ID %s in audit log, got: %s", batchID, auditOutput)
		}
		if !strings.Contains(auditOutput, ownerID.String()) {
			t.Errorf("expected actor ID %s in audit log, got: %s", ownerID, auditOutput)
		}
	})

	// 9b. Multi-Owner Dual Control Maker-Checker Gating
	t.Run("Multi-owner dual control enforcement and fallback", func(t *testing.T) {
		// Provision a second owner for the property
		owner2ID := uuid.New()
		owner2Phone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+33)%10000000000)
		_, err := pool.Exec(ctx, `INSERT INTO users (id, phone, role, property_id) VALUES ($1, $2, 'owner', $3)`, owner2ID, owner2Phone, propID)
		if err != nil {
			t.Fatalf("insert second owner: %v", err)
		}
		defer func() {
			_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, owner2ID)
		}()

		// Create a new batch as ownerID (maker)
		piID := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO payout_items (id, payee_id, reference_number, amount_paise, purpose, period_label, status)
			VALUES ($1, $2, 'REF-DUAL-01', 250000, 'Vendor Service', '2026-09', 'pending')`, piID, payeeID)
		if err != nil {
			t.Fatalf("insert payout item: %v", err)
		}
		defer func() {
			_, _ = pool.Exec(ctx, `DELETE FROM payout_items WHERE id = $1`, piID)
		}()

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/owner/payouts/batches", strings.NewReader(`{"notes":"Dual control test"}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("create batch as owner1: expected 201, got %d: %s", w.Code, w.Body.String())
		}
		var dualResp struct {
			Batch domain.PayoutBatch `json:"batch"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &dualResp)
		dualBatchID := dualResp.Batch.ID
		defer func() {
			_, _ = pool.Exec(ctx, `DELETE FROM payout_batches WHERE id = $1`, dualBatchID)
		}()

		// 1. Maker (ownerID) tries to approve their own batch -> must fail 403 Forbidden
		w = httptest.NewRecorder()
		approvePayload := `{"expected_item_count":1,"expected_total_paise":250000,"reauth_confirmation":"CONFIRM"}`
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", dualBatchID), strings.NewReader(approvePayload))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden when maker tries to approve own batch, got %d: %s", w.Code, w.Body.String())
		}

		// 2. Second owner (owner2ID) approves -> must succeed 200 OK
		owner2Router := gin.New()
		owner2Group := owner2Router.Group("/owner", func(c *gin.Context) {
			c.Set(auth.ContextClaimsKey, &auth.Claims{
				UserID:     owner2ID,
				PropertyID: &propID,
				Role:       domain.RoleOwner,
			})
		})
		owner2Group.POST("/payouts/batches/:id/approve", h.OwnerApprovePayoutBatch)

		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", dualBatchID), strings.NewReader(approvePayload))
		req.Header.Set("Content-Type", "application/json")
		owner2Router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK when checker approves batch, got %d: %s", w.Code, w.Body.String())
		}

		// 3. Test Edge Case: Second owner removed fallback
		piID2 := uuid.New()
		_, _ = pool.Exec(ctx, `
			INSERT INTO payout_items (id, payee_id, reference_number, amount_paise, purpose, period_label, status)
			VALUES ($1, $2, 'REF-DUAL-02', 150000, 'Vendor Supply', '2026-09', 'pending')`, piID2, payeeID)
		defer func() { _, _ = pool.Exec(ctx, `DELETE FROM payout_items WHERE id = $1`, piID2) }()

		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, "/owner/payouts/batches", strings.NewReader(`{"notes":"Fallback test"}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		_ = json.Unmarshal(w.Body.Bytes(), &dualResp)
		fallbackBatchID := dualResp.Batch.ID
		defer func() { _, _ = pool.Exec(ctx, `DELETE FROM payout_batches WHERE id = $1`, fallbackBatchID) }()

		// Remove second owner from property (cleanup earlier batch referencing owner2 first)
		_, _ = pool.Exec(ctx, `DELETE FROM payout_batches WHERE id = $1`, dualBatchID)
		_, err = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, owner2ID)
		if err != nil {
			t.Fatalf("delete second owner: %v", err)
		}

		// Request OTP for solo fallback
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve/request-otp", fallbackBatchID), nil)
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK requesting OTP for fallback batch, got %d: %s", w.Code, w.Body.String())
		}
		fbMsg := mockSMS.sent[len(mockSMS.sent)-1]
		fbParts := strings.Split(fbMsg, " ")
		fbCode := strings.TrimSuffix(fbParts[5], ".")

		// Now creator tries to approve with step-up reauth OTP -> gracefully succeeds (doesn't brick)
		w = httptest.NewRecorder()
		approveBody := map[string]interface{}{
			"expected_item_count":  1,
			"expected_total_paise": 150000,
			"otp":                  fbCode,
		}
		approveJSON, _ := json.Marshal(approveBody)
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", fallbackBatchID), bytes.NewReader(approveJSON))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK via graceful solo fallback when second owner removed, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 10. IDOR Guards: another owner with a different property ID cannot access these endpoints
	t.Run("IDOR ownership guards", func(t *testing.T) {
		otherPropID := uuid.New()
		otherRouter := gin.New()
		otherOwner := otherRouter.Group("/owner", func(c *gin.Context) {
			c.Set(auth.ContextClaimsKey, &auth.Claims{
				UserID:     uuid.New(),
				PropertyID: &otherPropID,
				Role:       domain.RoleOwner,
			})
		})
		{
			otherOwner.POST("/departures/:id/inspect", h.OwnerInspectDeparture)
			otherOwner.POST("/departures/:id/deductions", h.OwnerAddDepartureDeduction)
			otherOwner.POST("/departures/:id/settle", h.OwnerSettleDeparture)
			otherOwner.GET("/payouts/batches/:id/export", h.OwnerExportPayoutBatch)
		}

		// Inspect IDOR check
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/departures/%s/inspect", depID), strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		otherRouter.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404 Not Found on inspect IDOR, got %d", w.Code)
		}

		// Add Deduction IDOR check
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/departures/%s/deductions", depID), strings.NewReader(`{"description":"test","amount_paise":100}`))
		req.Header.Set("Content-Type", "application/json")
		otherRouter.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404 Not Found on deductions IDOR, got %d", w.Code)
		}

		// Settle IDOR check
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/departures/%s/settle", depID), strings.NewReader(`{"actual_vacate_date":"2026-09-15"}`))
		req.Header.Set("Content-Type", "application/json")
		otherRouter.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404 Not Found on settle IDOR, got %d", w.Code)
		}

		// Export IDOR check
		w = httptest.NewRecorder()
		req, _ = http.NewRequest(http.MethodGet, fmt.Sprintf("/owner/payouts/batches/%s/export", batchID), nil)
		otherRouter.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404 Not Found on export IDOR, got %d", w.Code)
		}
	})

	// 11. Checksum Mismatch Tamper Gate:
	t.Run("GET /owner/payouts/batches/:id/export with tampered checksum", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE payout_batches SET file_checksum='tampered_bad_checksum' WHERE id=$1`, batchID)
		if err != nil {
			t.Fatalf("tamper batch: %v", err)
		}
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/owner/payouts/batches/%s/export", batchID), nil)
		router.ServeHTTP(w, req)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict on tampered checksum, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestCSVExportSanitizationAndFormatting(t *testing.T) {
	// Formula injection triggers must be escaped with a leading single quote
	cases := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"NORMAL_REF", "NORMAL_REF"},
		{"=SUM(A1:B10)", "'=SUM(A1:B10)"},
		{"+cmd|' /C calc'!A0", "'+cmd|' /C calc'!A0"},
		{"-12345", "'-12345"},
		{"@SUM(A1:A5)", "'@SUM(A1:A5)"},
		{"\tTAB_CMD", "'\tTAB_CMD"},
		{"\rCR_CMD", "'\rCR_CMD"},
	}

	for _, tc := range cases {
		got := sanitizeCSVCell(tc.input)
		if got != tc.expected {
			t.Errorf("sanitizeCSVCell(%q) = %q; want %q", tc.input, got, tc.expected)
		}
	}

	// Exact 2-decimal formatting without float precision loss
	inrCases := []struct {
		paise    int64
		expected string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{50, "0.50"},
		{100, "1.00"},
		{12345, "123.45"},
		{750000, "7500.00"},
		{-50000, "-500.00"},
	}

	for _, tc := range inrCases {
		got := formatPaiseToINR(tc.paise)
		if got != tc.expected {
			t.Errorf("formatPaiseToINR(%d) = %q; want %q", tc.paise, got, tc.expected)
		}
	}
}

func TestPayoutAutoDispatchOnApproval(t *testing.T) {
	if testing.Short() {
		testutil.FailOnSkipIfDBRequired(t, "skipping live postgres test in short mode")
	}
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	_ = postgres.Migrate(ctx, pool, filepath.Join("..", "..", "migrations"))

	propID := uuid.New()
	ownerPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+77)%10000000000)
	inviteCode := fmt.Sprintf("E%s", uuid.New().String()[:7])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Auto-Dispatch Test PG', '789 Auto St', $2, 'owner@upi', 'Owner', 'owner@auto.com', $3)`,
		propID, ownerPhone, inviteCode,
	)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID) }()

	ownerID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3)`, ownerID, ownerPhone, propID)
	if err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, ownerID) }()

	payoutRepo := postgres.NewPayoutRepo(pool)

	// Mock Cashfree server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/beneficiary" {
			_ = json.NewEncoder(w).Encode(cashfree.BeneficiaryResponse{Status: "ACTIVE"})
			return
		}
		if r.URL.Path == "/transfers/batch" {
			var req cashfree.BatchTransferRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(cashfree.BatchTransferResponse{
				BatchTransferID:   req.BatchTransferID,
				CFBatchTransferID: "cf_mock_auto_batch",
				Status:            "RECEIVED",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfPayoutClient := cashfree.NewPayoutClient(cashfree.PayoutConfig{
		ClientID:     "mock_cid",
		ClientSecret: "mock_csec",
		FundsourceID: "fund_01",
	})
	cfPayoutClient.SetBaseOverride(srv.URL)
	dispatcher := finance.NewPayoutDispatcher(payoutRepo, cfPayoutClient, "fund_01")

	// Insert Payee directly
	payeeID := uuid.New()
	vpa := "vendor@upi"
	hash := "mock_hash_1234"
	last4 := "1234"
	_, err = pool.Exec(ctx, `
		INSERT INTO payout_payees (id, property_id, payee_type, name, account_number_hash, account_number_last4, upi_vpa)
		VALUES ($1, $2, 'vendor', 'Auto Vendor', $3, $4, $5)`, payeeID, propID, hash, last4, vpa)
	if err != nil {
		t.Fatalf("insert payee: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM payout_payees WHERE id = $1`, payeeID) }()

	// Insert Payout Item
	piID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO payout_items (id, payee_id, reference_number, amount_paise, purpose, period_label, status)
		VALUES ($1, $2, 'REF-AUTODISP-01', 500000, 'Vendor Supply', '2026-09', 'pending')`, piID, payeeID)
	if err != nil {
		t.Fatalf("insert payout item: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM payout_items WHERE id = $1`, piID) }()

	// Create Batch via HTTP
	checkerOwnerID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, '+919999988882', 'owner', $2)`, checkerOwnerID, propID)
	if err != nil {
		t.Fatalf("insert checker owner: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, checkerOwnerID) }()

	currentUserID := ownerID
	h := &Handlers{
		Deps: Deps{
			Pool:                 pool,
			PayoutRepo:           payoutRepo,
			PayoutDispatcher:     dispatcher,
			PayoutChecksumSecret: "test_secret",
			UserStore: &mockDualControlUserStore{
				owners: []domain.User{{ID: ownerID}, {ID: checkerOwnerID}},
			},
		},
	}

	router := gin.New()
	ownerGroup := router.Group("/owner", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     currentUserID,
			PropertyID: &propID,
			Role:       domain.RoleOwner,
		})
	})
	ownerGroup.POST("/payouts/batches", h.OwnerCreatePayoutBatch)
	ownerGroup.POST("/payouts/batches/:id/approve", h.OwnerApprovePayoutBatch)
	ownerGroup.POST("/payouts/batches/:id/dispatch", h.OwnerDispatchPayoutBatch)

	// Subtest 1: With PayoutAutoDispatch DISABLED (default in PropertySettings),
	// approval MUST keep the batch in 'approved' status (dual control halt, requiring manual dispatch).
	_, _ = pool.Exec(ctx, `
		INSERT INTO property_settings (property_id, payout_auto_dispatch)
		VALUES ($1, false)
		ON CONFLICT (property_id) DO UPDATE SET payout_auto_dispatch = false`, propID)

	w := httptest.NewRecorder()
	createReq, _ := http.NewRequest(http.MethodPost, "/owner/payouts/batches", strings.NewReader(`{"notes":"Auto test 1"}`))
	createReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, createReq)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created creating batch, got %d: %s", w.Code, w.Body.String())
	}
	var createResp struct {
		Batch domain.PayoutBatch `json:"batch"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	batchID := createResp.Batch.ID
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM payout_batches WHERE id = $1`, batchID) }()

	// Approve batch with affirmative match
	currentUserID = checkerOwnerID
	approveBody := map[string]interface{}{
		"expected_item_count":  1,
		"expected_total_paise": 500000,
	}
	bodyJSON, _ := json.Marshal(approveBody)
	w = httptest.NewRecorder()
	approveReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", batchID), bytes.NewReader(bodyJSON))
	approveReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, approveReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK approving batch with auto-dispatch disabled, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Batch        domain.PayoutBatch `json:"batch"`
		ApprovalMode string             `json:"approval_mode"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal approve response: %v", err)
	}

	// Batch MUST halt at 'approved' because payout_auto_dispatch is false
	if resp.Batch.Status != domain.BatchApproved {
		t.Errorf("expected batch status 'approved' when auto-dispatch disabled, got '%s'", resp.Batch.Status)
	}

	// Manual dispatch then succeeds
	w = httptest.NewRecorder()
	dispReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/dispatch", batchID), nil)
	router.ServeHTTP(w, dispReq)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK manual dispatching batch, got %d: %s", w.Code, w.Body.String())
	}

	// Subtest 2: With PayoutAutoDispatch ENABLED in property_settings,
	// approval MUST auto-initiate batch transfer to 'processing'.
	_, err = pool.Exec(ctx, `
		INSERT INTO property_settings (property_id, payout_auto_dispatch)
		VALUES ($1, true)
		ON CONFLICT (property_id) DO UPDATE SET payout_auto_dispatch = true`, propID)
	if err != nil {
		t.Fatalf("update property_settings: %v", err)
	}

	// Create a second payout item & batch
	piID2 := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO payout_items (id, payee_id, reference_number, amount_paise, purpose, period_label, status)
		VALUES ($1, $2, 'REF-AUTODISP-02', 300000, 'Vendor Supply 2', '2026-09', 'pending')`, piID2, payeeID)
	if err != nil {
		t.Fatalf("insert payout item 2: %v", err)
	}
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM payout_items WHERE id = $1`, piID2) }()

	currentUserID = ownerID
	w = httptest.NewRecorder()
	createReq2, _ := http.NewRequest(http.MethodPost, "/owner/payouts/batches", strings.NewReader(`{"notes":"Auto test 2"}`))
	createReq2.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, createReq2)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created creating batch 2, got %d: %s", w.Code, w.Body.String())
	}
	var createResp2 struct {
		Batch domain.PayoutBatch `json:"batch"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp2)
	batchID2 := createResp2.Batch.ID
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM payout_batches WHERE id = $1`, batchID2) }()

	currentUserID = checkerOwnerID
	approveBody2 := map[string]interface{}{
		"expected_item_count":  1,
		"expected_total_paise": 300000,
	}
	bodyJSON2, _ := json.Marshal(approveBody2)
	w = httptest.NewRecorder()
	approveReq2, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/owner/payouts/batches/%s/approve", batchID2), bytes.NewReader(bodyJSON2))
	approveReq2.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, approveReq2)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK approving batch 2 with auto-dispatch enabled, got %d: %s", w.Code, w.Body.String())
	}

	var resp2 struct {
		Batch domain.PayoutBatch `json:"batch"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal approve response 2: %v", err)
	}

	// Batch in response MUST be in 'processing' status because dispatcher auto-initiated transfer!
	if resp2.Batch.Status != domain.BatchProcessing {
		t.Errorf("expected batch status 'processing' on auto-dispatch, got '%s'", resp2.Batch.Status)
	}

	// Verify DB state
	dbBatch2, err := payoutRepo.GetBatchByID(ctx, batchID2)
	if err != nil {
		t.Fatalf("get batch from db: %v", err)
	}
	if dbBatch2.Status != domain.BatchProcessing {
		t.Errorf("expected DB batch status 'processing', got '%s'", dbBatch2.Status)
	}
}
