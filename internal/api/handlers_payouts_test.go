package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
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

func TestLivePayoutsAndDeparturesHTTPFlow(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

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
	dCode := fmt.Sprintf("H%05d", time.Now().UnixNano()%100000)
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
	h := &Handlers{
		Deps: Deps{
			Pool:                 pool,
			PayoutRepo:           payoutRepo,
			TenantStore:          tenantStore,
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

	// 9. Export Payout Batch CSV
	t.Run("GET /owner/payouts/batches/:id/export", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/owner/payouts/batches/%s/export", batchID), nil)
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
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
	})
}
