package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/crypto"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

func TestLivePayoutFencing_And_500ItemCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in -short mode")
	}
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

	if err := postgres.Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("apply project migrations: %v", err)
	}
	mig130, err := os.ReadFile(filepath.Join("..", "..", "migrations", "130_payout_dispatch_fence_generation.sql"))
	if err != nil {
		t.Fatalf("read migration 130: %v", err)
	}
	if _, err := pool.Exec(ctx, string(mig130)); err != nil {
		t.Fatalf("apply migration 130: %v", err)
	}

	propID := uuid.New()
	inviteCode := fmtInviteCode("FNCE")
	if _, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Fencing Test PG', '789 Fence St', '+919999977777', 'owner@upi', 'Owner', 'fence@test.com', $2)`,
		propID, inviteCode,
	); err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	ownerID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, phone, role, property_id) VALUES ($1, $2, 'owner', $3)`, ownerID, fmtPhone(), propID); err != nil {
		t.Fatalf("insert owner: %v", err)
	}

	payoutRepo := postgres.NewPayoutRepo(pool)
	signingKey := []byte("approval_signing_key_32_bytes_ok!")
	checksumSecret := []byte("checksum_secret")
	encKey := []byte("01234567890123456789012345678901")

	payee := &domain.PayoutPayee{
		PropertyID:         propID,
		PayeeType:          domain.PayeeTypeStaff,
		Name:               "Fence Staff",
		AccountNumberHash:  strings.Repeat("a", 64),
		UPIVPA:             ptr("fence@upi"),
		IsVerified:         true,
		VerificationStatus: domain.PayeeVerificationProviderVerified,
	}
	if err := payoutRepo.CreatePayee(ctx, payee); err != nil {
		t.Fatalf("create payee: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payout_payees SET created_at = NOW() - INTERVAL '25 hours' WHERE id = $1`, payee.ID); err != nil {
		t.Fatalf("age payee: %v", err)
	}

	t.Run("501_Item_Backlog_Creates_Bounded_500_And_1_Batches", func(t *testing.T) {
		for i := 0; i < 501; i++ {
			it := &domain.PayoutItem{
				PayeeID:         payee.ID,
				ReferenceNumber: fmt.Sprintf("CAP-%04d-%d", i, time.Now().UnixNano()),
				AmountPaise:     10000,
				Purpose:         "Cap Test",
				Status:          domain.PayoutPending,
			}
			if err := payoutRepo.CreatePayoutItem(ctx, it); err != nil {
				t.Fatalf("create item %d: %v", i, err)
			}
		}

		b1, items1, err := payoutRepo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, fmt.Sprintf("CAP-1-%d", time.Now().UnixNano()), checksumSecret, nil)
		if err != nil {
			t.Fatalf("create batch 1: %v", err)
		}
		if b1.ItemCount != domain.MaxPayoutBatchItems || len(items1) != domain.MaxPayoutBatchItems {
			t.Fatalf("expected batch 1 to have %d items, got ItemCount=%d len(items)=%d", domain.MaxPayoutBatchItems, b1.ItemCount, len(items1))
		}
		if _, _, err := payoutRepo.ApproveBatchSigned(ctx, propID, b1.ID, ownerID, signingKey, "v1"); err != nil {
			t.Fatalf("approve batch 1: %v", err)
		}

		b2, items2, err := payoutRepo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, fmt.Sprintf("CAP-2-%d", time.Now().UnixNano()), checksumSecret, nil)
		if err != nil {
			t.Fatalf("create batch 2: %v", err)
		}
		if b2.ItemCount != 1 || len(items2) != 1 {
			t.Fatalf("expected batch 2 to have 1 remaining item, got ItemCount=%d len(items)=%d", b2.ItemCount, len(items2))
		}
	})

	t.Run("Stale_Worker_Blocked_After_Lease_Loss_And_WorkerB_Queries_Original_Batch", func(t *testing.T) {
		item := &domain.PayoutItem{
			PayeeID:         payee.ID,
			ReferenceNumber: fmt.Sprintf("FENCE-%d", time.Now().UnixNano()),
			AmountPaise:     25000,
			Purpose:         "Fence Test",
			Status:          domain.PayoutPending,
		}
		if err := payoutRepo.CreatePayoutItem(ctx, item); err != nil {
			t.Fatalf("create item: %v", err)
		}
		batch, _, err := payoutRepo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, fmt.Sprintf("FENCE-B-%d", time.Now().UnixNano()), checksumSecret, nil)
		if err != nil {
			t.Fatalf("create batch: %v", err)
		}
		if _, _, err := payoutRepo.ApproveBatchSigned(ctx, propID, batch.ID, ownerID, signingKey, "v1"); err != nil {
			t.Fatalf("approve batch: %v", err)
		}
		if _, _, err := payoutRepo.InitiateBatchTransferTx(ctx, batch.ID); err != nil {
			t.Fatalf("initiate batch transfer: %v", err)
		}

		repoA := postgres.NewPayoutRepo(pool)
		repoA.SetDispatchLeaseTimings(1500*time.Millisecond, 2500*time.Millisecond)
		repoB := postgres.NewPayoutRepo(pool)
		repoB.SetDispatchLeaseTimings(10*time.Second, 4*time.Second)

		leaseA, acqA, err := repoA.TryLockBatchDispatch(ctx, batch.ID)
		if err != nil || !acqA {
			t.Fatalf("worker A acquire failed: acq=%v err=%v", acqA, err)
		}
		if leaseA.Generation != 1 {
			t.Fatalf("expected generation 1, got %d", leaseA.Generation)
		}

		// Concurrent Worker B cannot acquire while Worker A's lease is active.
		_, acqB, err := repoB.TryLockBatchDispatch(ctx, batch.ID)
		if err != nil {
			t.Fatalf("worker B concurrent try failed: %v", err)
		}
		if acqB {
			t.Fatalf("expected worker B to be blocked while worker A holds lease")
		}

		// Simulate lease expiry without renewal (e.g., Worker A lost DB connectivity),
		// then Worker B acquires the expired lease and increments generation to 2.
		time.Sleep(1800 * time.Millisecond)

		leaseB, acqB2, err := repoB.TryLockBatchDispatch(ctx, batch.ID)
		if err != nil || !acqB2 {
			t.Fatalf("worker B takeover failed: acq=%v err=%v", acqB2, err)
		}
		defer leaseB.Unlock()
		if leaseB.Generation != 2 {
			t.Fatalf("expected generation 2 after takeover, got %d", leaseB.Generation)
		}

		// Give Worker A's heartbeat a moment to observe ownership loss and cancel leaseA.Ctx.
		select {
		case <-leaseA.Ctx.Done():
		case <-time.After(4 * time.Second):
			t.Fatalf("expected Worker A lease context to be cancelled after takeover")
		}

		// Even if Worker A receives a delayed Cashfree success response, every fenced mutation fails.
		if err := repoA.SetBatchDispatchUnknownFenced(ctx, leaseA, "delayed 5xx"); !errors.Is(err, domain.ErrStaleDispatchLease) {
			t.Fatalf("expected ErrStaleDispatchLease from stale SetBatchDispatchUnknownFenced, got %v", err)
		}
		if err := repoA.ReconcilePayoutItemWithOutboxFenced(ctx, leaseA, propID, item.ID, item.AmountPaise, domain.PayoutSucceeded, nil, nil, nil, nil); !errors.Is(err, domain.ErrStaleDispatchLease) {
			t.Fatalf("expected ErrStaleDispatchLease from stale ReconcilePayoutItemWithOutboxFenced, got %v", err)
		}
		if _, err := repoA.UpdateBatchStatusFromItemsFenced(ctx, leaseA); !errors.Is(err, domain.ErrStaleDispatchLease) {
			t.Fatalf("expected ErrStaleDispatchLease from stale UpdateBatchStatusFromItemsFenced, got %v", err)
		}
		if err := repoA.RecordBatchIncidentFenced(ctx, leaseA, "UNKNOWN_TRANSFER_ID", "stale"); !errors.Is(err, domain.ErrStaleDispatchLease) {
			t.Fatalf("expected ErrStaleDispatchLease from stale RecordBatchIncidentFenced, got %v", err)
		}
		if err := repoA.RecordItemIncidentFenced(ctx, leaseA, item.ID, "TRANSFER_ID_MISMATCH", "exp", "got", "stale"); !errors.Is(err, domain.ErrStaleDispatchLease) {
			t.Fatalf("expected ErrStaleDispatchLease from stale RecordItemIncidentFenced, got %v", err)
		}

		// Release leaseB so Worker B's PayoutDispatcher can run DispatchBatch and query the original batch status.
		leaseB.Unlock()

		var batchStatusCalls atomic.Int32
		var batchCreateCalls atomic.Int32
		expectedBatchTransferID := fmt.Sprintf("pgo_%s", strings.ReplaceAll(batch.ID.String(), "-", ""))
		expectedTransferID := DerivePayoutTransferID(*item)

		mockCF := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/transfers/batch") {
				batchStatusCalls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"cf_batch_transfer_id": "cf_batch_recovered",
					"batch_transfer_id":    expectedBatchTransferID,
					"status":               "PROCESSED",
					"transfers": []map[string]any{
						{
							"transfer_id":     expectedTransferID,
							"cf_transfer_id":  "cf_tr_recovered",
							"status":          "SUCCESS",
							"status_code":     "COMPLETED",
							"transfer_amount": 250.00,
						},
					},
				})
				return
			}
			if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/transfers/batch") {
				batchCreateCalls.Add(1)
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer mockCF.Close()

		cfClient := cashfree.NewPayoutClient(cashfree.PayoutConfig{
			ClientID:     "cid",
			ClientSecret: "sec",
			FundsourceID: "fund_01",
		})
		cfClient.SetBaseOverride(mockCF.URL)
		approvalRing, err := crypto.NewApprovalKeyRing("v1", map[string][]byte{"v1": signingKey})
		if err != nil {
			t.Fatalf("new approval keyring: %v", err)
		}
		dispatcher := NewPayoutDispatcher(repoB, cfClient, "fund_01", encKey)
		dispatcher.SetApprovalKeyRing(approvalRing)

		if err := dispatcher.DispatchBatch(ctx, batch.ID); err != nil {
			t.Fatalf("Worker B DispatchBatch failed: %v", err)
		}
		if batchStatusCalls.Load() == 0 {
			t.Fatalf("expected Worker B to query original Cashfree batch status")
		}
		if batchCreateCalls.Load() != 0 {
			t.Fatalf("expected 0 duplicate batch submissions, got %d", batchCreateCalls.Load())
		}

		finalBatch, err := repoB.GetBatchByID(ctx, batch.ID)
		if err != nil {
			t.Fatalf("get final batch: %v", err)
		}
		if finalBatch.Status != domain.BatchCompleted {
			t.Fatalf("expected batch completed, got %s", finalBatch.Status)
		}
		if finalBatch.DispatchFenceGeneration < 3 {
			t.Fatalf("expected dispatch_fence_generation >= 3, got %d", finalBatch.DispatchFenceGeneration)
		}
		leaseA.Unlock()
	})
}
