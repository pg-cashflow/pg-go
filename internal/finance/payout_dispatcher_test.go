package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func TestLivePayoutDispatcher(t *testing.T) {
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

	_ = postgres.Migrate(ctx, pool, filepath.Join("..", "..", "migrations"))

	propID := uuid.New()
	inviteCode := fmtInviteCode("DISP")
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Dispatcher Test PG', '456 Test St', '+919999966666', 'owner@upi', 'Owner', 'owner@test.com', $2)`,
		propID, inviteCode,
	)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	ownerID := uuid.New()
	ownerPhone := fmtPhone()
	_, err = pool.Exec(ctx, `INSERT INTO users (id, phone, role, property_id) VALUES ($1, $2, 'owner', $3)`, ownerID, ownerPhone, propID)
	if err != nil {
		t.Fatalf("insert owner: %v", err)
	}

	payoutRepo := postgres.NewPayoutRepo(pool)

	// Create Payee 1 (Bank) and Payee 2 (UPI)
	payee1 := &domain.PayoutPayee{
		PropertyID:             propID,
		PayeeType:              domain.PayeeTypeVendor,
		Name:                   "Vendor One",
		AccountNumberEncrypted: []byte("111222333444"),
		AccountNumberHash:      "hash111111111111111111111111111111111111111111111111111111111111",
		IFSC:                   ptr("HDFC0001234"),
	}
	if err := payoutRepo.CreatePayee(ctx, payee1); err != nil {
		t.Fatalf("create payee 1: %v", err)
	}

	payee2 := &domain.PayoutPayee{
		PropertyID:        propID,
		PayeeType:         domain.PayeeTypeStaff,
		Name:              "Staff Two",
		AccountNumberHash: "hash222222222222222222222222222222222222222222222222222222222222",
		UPIVPA:            ptr("staff@upi"),
	}
	if err := payoutRepo.CreatePayee(ctx, payee2); err != nil {
		t.Fatalf("create payee 2: %v", err)
	}

	// Create 2 unbatched items
	item1 := &domain.PayoutItem{
		PayeeID:         payee1.ID,
		ReferenceNumber: "REF-001",
		AmountPaise:     500000, // Rs 5000.00
		Purpose:         "Vendor Bill",
		Status:          domain.PayoutPending,
	}
	if err := payoutRepo.CreatePayoutItem(ctx, item1); err != nil {
		t.Fatalf("create item 1: %v", err)
	}

	item2 := &domain.PayoutItem{
		PayeeID:         payee2.ID,
		ReferenceNumber: "REF-002",
		AmountPaise:     250000, // Rs 2500.00
		Purpose:         "Staff Salary",
		Status:          domain.PayoutPending,
	}
	if err := payoutRepo.CreatePayoutItem(ctx, item2); err != nil {
		t.Fatalf("create item 2: %v", err)
	}

	// Create and approve batch
	batchNum := fmt.Sprintf("BATCH-DISP-%s", uuid.New().String()[:8])
	batch, _, err := payoutRepo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, batchNum, []byte("secret"), nil)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	batch, err = payoutRepo.ApproveBatch(ctx, batch.ID, ownerID)
	if err != nil {
		t.Fatalf("approve batch: %v", err)
	}

	// Transitions to processing
	batch, _, err = payoutRepo.InitiateBatchTransferTx(ctx, batch.ID)
	if err != nil {
		t.Fatalf("initiate batch transfer: %v", err)
	}

	t.Run("DispatchBatch_Success", func(t *testing.T) {
		var receivedTransfers []cashfree.BatchTransferEntry
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/beneficiary" {
				_ = json.NewEncoder(w).Encode(cashfree.BeneficiaryResponse{Status: "ACTIVE"})
				return
			}
			if r.URL.Path == "/transfers/batch" {
				var req cashfree.BatchTransferRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				receivedTransfers = req.Transfers
				_ = json.NewEncoder(w).Encode(cashfree.BatchTransferResponse{
					BatchTransferID:   req.BatchTransferID,
					CFBatchTransferID: "cf_mock_batch_123",
					Status:            "RECEIVED",
				})
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		client := cashfree.NewPayoutClient(cashfree.PayoutConfig{
			ClientID:     "cid",
			ClientSecret: "csec",
			FundsourceID: "fund_01",
		})
		client.SetBaseOverride(srv.URL)

		dispatcher := NewPayoutDispatcher(payoutRepo, client, "fund_01")
		if err := dispatcher.DispatchBatch(ctx, batch.ID); err != nil {
			t.Fatalf("dispatch batch: %v", err)
		}

		if len(receivedTransfers) != 2 {
			t.Fatalf("expected 2 transfers sent to cashfree, got %d", len(receivedTransfers))
		}
		amountMap := make(map[string]string)
		for _, tr := range receivedTransfers {
			amountMap[tr.TransferID] = tr.TransferAmount
		}
		if amountMap[fmt.Sprintf("pgo_%s", item1.ID)] != "5000.00" {
			t.Errorf("expected 5000.00 for item 1, got %s", amountMap[fmt.Sprintf("pgo_%s", item1.ID)])
		}
		if amountMap[fmt.Sprintf("pgo_%s", item2.ID)] != "2500.00" {
			t.Errorf("expected 2500.00 for item 2, got %s", amountMap[fmt.Sprintf("pgo_%s", item2.ID)])
		}
	})

	t.Run("DispatchBatch_5xx_Transitions_To_dispatch_unknown", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/beneficiary" {
				_ = json.NewEncoder(w).Encode(cashfree.BeneficiaryResponse{Status: "ACTIVE"})
				return
			}
			if r.URL.Path == "/transfers/batch" {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		client := cashfree.NewPayoutClient(cashfree.PayoutConfig{
			ClientID:     "cid",
			ClientSecret: "csec",
			FundsourceID: "fund_01",
		})
		client.SetBaseOverride(srv.URL)

		dispatcher := NewPayoutDispatcher(payoutRepo, client, "fund_01")
		err := dispatcher.DispatchBatch(ctx, batch.ID)
		if err == nil {
			t.Fatal("expected error on 5xx, got nil")
		}

		reloadedBatch, err := payoutRepo.GetBatchByID(ctx, batch.ID)
		if err != nil {
			t.Fatalf("get batch: %v", err)
		}
		if reloadedBatch.Status != domain.BatchDispatchUnknown {
			t.Fatalf("expected batch status 'dispatch_unknown', got '%s'", reloadedBatch.Status)
		}
	})
}

func fmtInviteCode(prefix string) string {
	return prefix + uuid.New().String()[:6]
}

func fmtPhone() string {
	return "+91" + uuid.New().String()[:10]
}

func ptr(s string) *string {
	return &s
}
