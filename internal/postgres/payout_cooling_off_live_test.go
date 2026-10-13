package postgres

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// REQ-SEC-007 cooling-off guard, tested against live Postgres.
// CreateBatchFromUnbatchedItems must refuse any batch that includes a payee
// created less than 24 hours ago, and must allow a batch whose payees are all
// older than 24 hours. Each case uses its own property, so no item from one
// case can leak into another.

// coolingFixture creates a property and an owner user. It returns both IDs.
func coolingFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	propID := uuid.New()
	inviteCode := fmt.Sprintf("CLG%s", uuid.New().String()[:8])
	if _, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Cooling-off Test PG', '1 Test St', '+919999977777', 'owner@upi', 'Owner', 'owner@test.com', $2)`,
		propID, inviteCode,
	); err != nil {
		t.Fatalf("insert property: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM payout_items WHERE payee_id IN (SELECT id FROM payout_payees WHERE property_id = $1)`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM payout_batches WHERE property_id = $1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM payout_payees WHERE property_id = $1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	})

	ownerID := uuid.New()
	ownerPhone := fmt.Sprintf("+91%010d", time.Now().UnixNano()%10000000000)
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3)`, ownerID, ownerPhone, propID,
	); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, ownerID)
	})
	return propID, ownerID
}

// insertCoolingPayee inserts a payee with a created_at expression.
// createdAtSQL is a fixed string from this file, not user input.
func insertCoolingPayee(t *testing.T, ctx context.Context, pool *pgxpool.Pool, propID uuid.UUID, createdAtSQL string) uuid.UUID {
	t.Helper()
	payeeID := uuid.New()
	hash := domain.ComputeAccountHash([]byte("test_salt"), payeeID.String())
	upi := payeeID.String()[:8] + "@upi"
	query := fmt.Sprintf(`
		INSERT INTO payout_payees (id, property_id, payee_type, name, account_number_hash, upi_vpa, is_verified, created_at, updated_at)
		VALUES ($1, $2, 'tenant_deposit', 'Cooling Payee', $3, $4, true, %s, NOW())`, createdAtSQL)
	if _, err := pool.Exec(ctx, query, payeeID, propID, hash, upi); err != nil {
		t.Fatalf("insert payee (created_at=%s): %v", createdAtSQL, err)
	}
	return payeeID
}

// createPendingItem creates one unbatched pending item for the payee.
func createPendingItem(t *testing.T, ctx context.Context, repo *PayoutRepo, payeeID uuid.UUID) *domain.PayoutItem {
	t.Helper()
	it := &domain.PayoutItem{
		PayeeID:         payeeID,
		ReferenceNumber: uuid.New().String(),
		AmountPaise:     100000,
		Purpose:         "tenant_deposit_refund",
		PeriodLabel:     "2026-10",
	}
	if err := repo.CreatePayoutItem(ctx, it); err != nil {
		t.Fatalf("create payout item: %v", err)
	}
	return it
}

// batchCountForProperty returns the number of batches stored for the property.
func batchCountForProperty(t *testing.T, ctx context.Context, pool *pgxpool.Pool, propID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payout_batches WHERE property_id = $1`, propID).Scan(&n); err != nil {
		t.Fatalf("count batches: %v", err)
	}
	return n
}

func TestLivePayoutCoolingOff(t *testing.T) {
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("cannot connect to Postgres (%v), skipping live test", err))
	}
	defer pool.Close()
	_ = Migrate(ctx, pool, filepath.Join("..", "..", "migrations"))

	repo := NewPayoutRepo(pool)
	secret := []byte("test_hmac_secret_for_cooling_off")

	t.Run("fresh payee alone is refused and nothing is batched", func(t *testing.T) {
		propID, ownerID := coolingFixture(t, ctx, pool)
		payeeID := insertCoolingPayee(t, ctx, pool, propID, "NOW()")
		item := createPendingItem(t, ctx, repo, payeeID)

		batchNum := "BATCH-" + uuid.New().String()
		batch, _, err := repo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, batchNum, secret, nil)
		if !errors.Is(err, domain.ErrPayeeCoolingOff) {
			t.Fatalf("expected ErrPayeeCoolingOff for fresh payee, got batch=%v err=%v", batch, err)
		}
		if batch != nil {
			t.Fatalf("expected no batch returned, got %+v", batch)
		}

		got, err := repo.GetPayoutItemByID(ctx, item.ID)
		if err != nil {
			t.Fatalf("reload item: %v", err)
		}
		if got.BatchID != nil {
			t.Fatalf("refused item must stay unbatched, got batch_id=%v", *got.BatchID)
		}
		if n := batchCountForProperty(t, ctx, pool, propID); n != 0 {
			t.Fatalf("expected 0 batches after refusal, got %d", n)
		}
	})

	t.Run("one fresh payee blocks a mixed batch that also holds an aged payee", func(t *testing.T) {
		propID, ownerID := coolingFixture(t, ctx, pool)
		agedPayee := insertCoolingPayee(t, ctx, pool, propID, "NOW() - INTERVAL '25 hours'")
		freshPayee := insertCoolingPayee(t, ctx, pool, propID, "NOW()")
		agedItem := createPendingItem(t, ctx, repo, agedPayee)
		freshItem := createPendingItem(t, ctx, repo, freshPayee)

		batchNum := "BATCH-" + uuid.New().String()
		_, _, err := repo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, batchNum, secret, nil)
		if !errors.Is(err, domain.ErrPayeeCoolingOff) {
			t.Fatalf("expected ErrPayeeCoolingOff for mixed batch, got err=%v", err)
		}
		for _, id := range []uuid.UUID{agedItem.ID, freshItem.ID} {
			got, err := repo.GetPayoutItemByID(ctx, id)
			if err != nil {
				t.Fatalf("reload item %s: %v", id, err)
			}
			if got.BatchID != nil {
				t.Fatalf("item %s must stay unbatched after refusal", id)
			}
		}
		if n := batchCountForProperty(t, ctx, pool, propID); n != 0 {
			t.Fatalf("expected 0 batches after refusal, got %d", n)
		}
	})

	t.Run("aged payee is batched successfully", func(t *testing.T) {
		propID, ownerID := coolingFixture(t, ctx, pool)
		payeeID := insertCoolingPayee(t, ctx, pool, propID, "NOW() - INTERVAL '25 hours'")
		item := createPendingItem(t, ctx, repo, payeeID)

		batchNum := "BATCH-" + uuid.New().String()
		batch, items, err := repo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, batchNum, secret, nil)
		if err != nil {
			t.Fatalf("expected batch for aged payee, got err=%v", err)
		}
		if batch == nil || len(items) != 1 || items[0].ID != item.ID {
			t.Fatalf("expected one batch containing the item, got batch=%v items=%d", batch, len(items))
		}
		got, err := repo.GetPayoutItemByID(ctx, item.ID)
		if err != nil {
			t.Fatalf("reload item: %v", err)
		}
		if got.BatchID == nil || *got.BatchID != batch.ID {
			t.Fatalf("item must be attached to the new batch")
		}
	})
}
