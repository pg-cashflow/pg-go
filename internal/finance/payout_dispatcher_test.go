package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

func TestLivePayoutDispatcher(t *testing.T) {
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
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
		IsVerified:             true,
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
		IsVerified:        true,
	}
	if err := payoutRepo.CreatePayee(ctx, payee2); err != nil {
		t.Fatalf("create payee 2: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payout_payees SET created_at = NOW() - INTERVAL '25 hours' WHERE id IN ($1, $2)`, payee1.ID, payee2.ID); err != nil {
		t.Fatalf("age payees: %v", err)
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
				if r.Method == http.MethodGet {
					http.NotFound(w, r)
					return
				}
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
			amountMap[tr.TransferID] = tr.TransferAmount.String()
		}
		if amountMap[fmt.Sprintf("pgo_%s", strings.ReplaceAll(item1.ID.String(), "-", ""))] != "5000.00" {
			t.Errorf("expected 5000.00 for item 1, got %s", amountMap[fmt.Sprintf("pgo_%s", strings.ReplaceAll(item1.ID.String(), "-", ""))])
		}
		if amountMap[fmt.Sprintf("pgo_%s", strings.ReplaceAll(item2.ID.String(), "-", ""))] != "2500.00" {
			t.Errorf("expected 2500.00 for item 2, got %s", amountMap[fmt.Sprintf("pgo_%s", strings.ReplaceAll(item2.ID.String(), "-", ""))])
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
				if r.Method == http.MethodGet {
					http.NotFound(w, r)
					return
				}
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

	t.Run("DispatchBatch blocks unverified payee", func(t *testing.T) {
		// Mark payee1 unverified
		if _, err := pool.Exec(ctx, `UPDATE payout_payees SET is_verified = false WHERE id = $1`, payee1.ID); err != nil {
			t.Fatalf("unverify payee: %v", err)
		}
		defer func() {
			_, _ = pool.Exec(ctx, `UPDATE payout_payees SET is_verified = true WHERE id = $1`, payee1.ID)
		}()

		client := cashfree.NewPayoutClient(cashfree.PayoutConfig{
			ClientID:     "cid",
			ClientSecret: "csec",
			FundsourceID: "fund_01",
		})
		dispatcher := NewPayoutDispatcher(payoutRepo, client, "fund_01")
		err := dispatcher.DispatchBatch(ctx, batch.ID)
		if err == nil || !strings.Contains(err.Error(), "not verified") {
			t.Fatalf("expected error containing 'not verified', got: %v", err)
		}
	})

	t.Run("DispatchBatch_ApprovalIntegrity_And_TOCTOU", func(t *testing.T) {
		signingKey := []byte("this-is-a-valid-signing-key-32ch-min")
		approvalKeys := map[string][]byte{"v1": signingKey}

		itemA := &domain.PayoutItem{
			PayeeID:         payee1.ID,
			ReferenceNumber: "REF-SNAP-01",
			AmountPaise:     300000,
			Purpose:         "Vendor Snapshot Bill",
			Status:          domain.PayoutPending,
		}
		if err := payoutRepo.CreatePayoutItem(ctx, itemA); err != nil {
			t.Fatalf("create itemA: %v", err)
		}
		itemB := &domain.PayoutItem{
			PayeeID:         payee2.ID,
			ReferenceNumber: "REF-SNAP-02",
			AmountPaise:     150000,
			Purpose:         "Staff Snapshot Salary",
			Status:          domain.PayoutPending,
		}
		if err := payoutRepo.CreatePayoutItem(ctx, itemB); err != nil {
			t.Fatalf("create itemB: %v", err)
		}

		batchNum2 := fmt.Sprintf("BATCH-SNAP-%s", uuid.New().String()[:8])
		batch2, _, err := payoutRepo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, batchNum2, []byte("secret"), nil)
		if err != nil {
			t.Fatalf("create batch2: %v", err)
		}

		// Approve batch using signed transactional approval with snapshot creation
		approvedBatch2, snapshot2, err := payoutRepo.ApproveBatchSigned(ctx, propID, batch2.ID, ownerID, signingKey, "v1")
		if err != nil {
			t.Fatalf("approve signed batch: %v", err)
		}
		if approvedBatch2.Status != domain.BatchApproved {
			t.Fatalf("expected approved status, got: %s", approvedBatch2.Status)
		}
		if snapshot2 == nil || len(snapshot2.Items) != 2 {
			t.Fatalf("expected snapshot with 2 items, got: %+v", snapshot2)
		}

		// Verify that VerifyBatchApproval succeeds
		verifiedSnapshot, err := payoutRepo.VerifyBatchApproval(ctx, batch2.ID, approvalKeys)
		if err != nil {
			t.Fatalf("VerifyBatchApproval failed: %v", err)
		}
		if verifiedSnapshot.BatchID != batch2.ID || verifiedSnapshot.TotalAmount != 450000 {
			t.Fatalf("snapshot verification mismatch: %+v", verifiedSnapshot)
		}

		// Transition batch to processing
		batch2, _, err = payoutRepo.InitiateBatchTransferTx(ctx, batch2.ID)
		if err != nil {
			t.Fatalf("initiate transfer tx: %v", err)
		}

		// Setup mock server
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/beneficiary" {
				_ = json.NewEncoder(w).Encode(cashfree.BeneficiaryResponse{Status: "ACTIVE"})
				return
			}
			if r.URL.Path == "/transfers/batch" {
				if r.Method == http.MethodGet {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(cashfree.BatchTransferResponse{
					BatchTransferID:   "pgo_test",
					CFBatchTransferID: "cf_mock_batch_snap",
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

		// Case 1: Tampered snapshot signature fails closed
		if _, err := pool.Exec(ctx, `UPDATE payout_approval_snapshots SET signature = 'tampered_signature_hex' WHERE batch_id = $1`, batch2.ID); err != nil {
			t.Fatalf("tamper signature: %v", err)
		}
		dispatcher := NewPayoutDispatcher(payoutRepo, client, "fund_01")
		dispatcher.SetApprovalSigningKeys(approvalKeys)
		err = dispatcher.DispatchBatch(ctx, batch2.ID)
		if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
			t.Fatalf("expected signature verification failure, got: %v", err)
		}

		// Case 2: Unknown key ID fails closed
		origSig, err := domain.SignApprovalSnapshot(signingKey, snapshot2)
		if err != nil {
			t.Fatalf("sign snapshot: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE payout_approval_snapshots SET signature = $1, key_id = 'v99' WHERE batch_id = $2`, origSig, batch2.ID); err != nil {
			t.Fatalf("update key_id: %v", err)
		}
		err = dispatcher.DispatchBatch(ctx, batch2.ID)
		if err == nil || !strings.Contains(err.Error(), "unknown or invalid approval signing key_id") {
			t.Fatalf("expected unknown key ID error, got: %v", err)
		}

		// Restore key_id
		if _, err := pool.Exec(ctx, `UPDATE payout_approval_snapshots SET key_id = 'v1' WHERE batch_id = $1`, batch2.ID); err != nil {
			t.Fatalf("restore key_id: %v", err)
		}

		// Case 3: TOCTOU detected - Payee bank account hash changed since approval
		origHash := payee1.AccountNumberHash
		if _, err := pool.Exec(ctx, `UPDATE payout_payees SET account_number_hash = 'tampered_hash_after_approval' WHERE id = $1`, payee1.ID); err != nil {
			t.Fatalf("alter payee hash: %v", err)
		}
		err = dispatcher.DispatchBatch(ctx, batch2.ID)
		if err == nil || !strings.Contains(err.Error(), "TOCTOU detected: payee") || !strings.Contains(err.Error(), "account hash changed") {
			t.Fatalf("expected TOCTOU account hash failure, got: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE payout_payees SET account_number_hash = $1 WHERE id = $2`, origHash, payee1.ID); err != nil {
			t.Fatalf("restore payee hash: %v", err)
		}

		// Case 4: TOCTOU detected - Payee IFSC changed since approval
		origIFSC := payee1.IFSC
		if _, err := pool.Exec(ctx, `UPDATE payout_payees SET ifsc = 'SBIN0009999' WHERE id = $1`, payee1.ID); err != nil {
			t.Fatalf("alter payee ifsc: %v", err)
		}
		err = dispatcher.DispatchBatch(ctx, batch2.ID)
		if err == nil || !strings.Contains(err.Error(), "TOCTOU detected: payee") || !strings.Contains(err.Error(), "IFSC changed") {
			t.Fatalf("expected TOCTOU IFSC failure, got: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE payout_payees SET ifsc = $1 WHERE id = $2`, origIFSC, payee1.ID); err != nil {
			t.Fatalf("restore payee ifsc: %v", err)
		}

		// Case 5: TOCTOU detected - Payee UPI VPA changed since approval
		origVPA := payee2.UPIVPA
		if _, err := pool.Exec(ctx, `UPDATE payout_payees SET upi_vpa = 'attacker@upi' WHERE id = $1`, payee2.ID); err != nil {
			t.Fatalf("alter payee vpa: %v", err)
		}
		err = dispatcher.DispatchBatch(ctx, batch2.ID)
		if err == nil || !strings.Contains(err.Error(), "TOCTOU detected: payee") || !strings.Contains(err.Error(), "UPI VPA changed") {
			t.Fatalf("expected TOCTOU UPI VPA failure, got: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE payout_payees SET upi_vpa = $1 WHERE id = $2`, origVPA, payee2.ID); err != nil {
			t.Fatalf("restore payee vpa: %v", err)
		}

		// Case 6: Legitimate signed batch with verified instruments dispatches cleanly
		err = dispatcher.DispatchBatch(ctx, batch2.ID)
		if err != nil {
			t.Fatalf("expected clean dispatch of signed batch, got error: %v", err)
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
