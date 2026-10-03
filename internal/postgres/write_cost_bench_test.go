package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
)

// TestWriteBenchmark measures the write performance of bank transactions, dues, and payments
// under the Search V2 indexing scheme (040, 041, 042).
func TestWriteBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping write benchmark in short mode")
	}

	_ = godotenv.Load("../../.env")
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	requireDisposableDB(t, dbURL)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load failed: %v", err)
	}

	ctx := context.Background()
	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer pool.Close()

	propID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM payments WHERE tenant_id IN (SELECT id FROM tenants WHERE property_id = $1)`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM dues WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM bank_transactions WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	})

	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
		VALUES ($1, 'Write Bench Property', '+919999999998', 'Bench Owner', 'bench@owner.test', $2, 'bench@upi')`,
		propID, uuid.New().String()[:8],
	)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}

	// Seed 100 tenants
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, room_number, phone, status, rent_amount, due_day)
		SELECT gen_random_uuid(), $1::uuid, 'Bench Tenant ' || i, (100 + i)::text, '+919' || lpad((abs(hashtext($1::text || i::text)) % 1000000000)::text, 9, '0'), 'active', 500000, 5
		FROM generate_series(1, 100) AS s(i)`,
		propID,
	)
	if err != nil {
		t.Fatalf("insert tenants: %v", err)
	}

	// 1. Bank transactions bulk insert (50,000 rows)
	const bankRows = 50000
	t.Logf("Timing bulk insert of %d bank_transactions with Migration 042 (lean set)...", bankRows)
	t0 := time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO bank_transactions (id, property_id, txn_id, amount_paise, row_type, txn_date, narration, dedup_hash, status)
		SELECT
			gen_random_uuid(),
			$1::uuid,
			'TXN-BENCH-' || i,
			500000,
			'credit',
			CURRENT_DATE,
			'NEFT CR RENT FROM TENANT ' || (i % 100) || ' REF ' || (100000000000 + i)::text,
			md5($1::text || i::text || clock_timestamp()::text),
			'unmatched'
		FROM generate_series(1, $2) AS s(i)`,
		propID, bankRows,
	)
	if err != nil {
		t.Fatalf("bulk insert bank transactions failed: %v", err)
	}
	bankDurationLean := time.Since(t0)
	t.Logf("--> %d bank_transactions insert (Lean set / Migration 042): %v (%.1f rows/sec)",
		bankRows, bankDurationLean, float64(bankRows)/bankDurationLean.Seconds())

	// 2. Dues bulk insert (20,000 rows)
	const duesRows = 20000
	t.Logf("Timing bulk insert of %d dues...", duesRows)
	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		WITH t_arr AS (
			SELECT array_agg(id) AS ids FROM tenants WHERE property_id = $1::uuid
		)
		INSERT INTO dues (id, tenant_id, property_id, due_code, kind, amount, original_amount, period_start, period_end, due_date, status)
		SELECT
			gen_random_uuid(),
			t_arr.ids[(i % cardinality(t_arr.ids)) + 1],
			$1::uuid,
			'D' || lpad(i::text, 7, '0'),
			'rent',
			500000,
			500000,
			CURRENT_DATE + ((i / 100) || ' months')::interval,
			CURRENT_DATE + ((i / 100 + 1) || ' months')::interval,
			CURRENT_DATE + ((i / 100) || ' months')::interval + interval '5 days',
			'pending'
		FROM t_arr, generate_series(1, $2) AS s(i)`,
		propID, duesRows,
	)
	if err != nil {
		t.Fatalf("bulk insert dues failed: %v", err)
	}
	duesDuration := time.Since(t0)
	t.Logf("--> %d dues insert: %v (%.1f rows/sec)", duesRows, duesDuration, float64(duesRows)/duesDuration.Seconds())

	// 3. Payments bulk insert (20,000 rows)
	const paymentsRows = 20000
	t.Logf("Timing bulk insert of %d payments...", paymentsRows)
	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		WITH d_arr AS (
			SELECT array_agg(id) AS d_ids, array_agg(tenant_id) AS t_ids FROM dues WHERE property_id = $1::uuid LIMIT 5000
		)
		INSERT INTO payments (id, due_id, tenant_id, upi_txn_id, amount, provider, matched_by, created_at)
		SELECT
			gen_random_uuid(),
			d_arr.d_ids[(i % cardinality(d_arr.d_ids)) + 1],
			d_arr.t_ids[(i % cardinality(d_arr.t_ids)) + 1],
			'412' || lpad(i::text, 9, '0'),
			500000,
			'manual',
			'manual',
			NOW()
		FROM d_arr, generate_series(1, $2) AS s(i)`,
		propID, paymentsRows,
	)
	if err != nil {
		t.Fatalf("bulk insert payments failed: %v", err)
	}
	paymentsDuration := time.Since(t0)
	t.Logf("--> %d payments insert: %v (%.1f rows/sec)", paymentsRows, paymentsDuration, float64(paymentsRows)/paymentsDuration.Seconds())

	// Compare with global GINs re-added (simulating pre-042 state)
	t.Log("Re-adding global GIN indexes to compare write cost before 042...")
	_, _ = pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_bank_transactions_narration_trgm ON bank_transactions USING gin (narration gin_trgm_ops);`)
	_, _ = pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_bank_transactions_txnid_trgm ON bank_transactions USING gin (txn_id gin_trgm_ops);`)

	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO bank_transactions (id, property_id, txn_id, amount_paise, row_type, txn_date, narration, dedup_hash, status)
		SELECT
			gen_random_uuid(),
			$1::uuid,
			'TXN-BENCH-GLOB-' || i,
			500000,
			'credit',
			CURRENT_DATE,
			'NEFT CR RENT FROM TENANT ' || (i % 100) || ' REF ' || (200000000000 + i)::text,
			md5($1::text || i::text || clock_timestamp()::text || 'g'),
			'unmatched'
		FROM generate_series(1, $2) AS s(i)`,
		propID, bankRows,
	)
	if err != nil {
		t.Fatalf("bulk insert bank transactions with globals failed: %v", err)
	}
	bankDurationWithGlobals := time.Since(t0)
	t.Logf("--> %d bank_transactions insert (with redundant globals, before 042): %v (%.1f rows/sec)",
		bankRows, bankDurationWithGlobals, float64(bankRows)/bankDurationWithGlobals.Seconds())
	t.Logf("Write speedup from dropping redundant global GINs (Migration 042): %.1f%% faster (from %v down to %v)",
		float64(bankDurationWithGlobals-bankDurationLean)/float64(bankDurationWithGlobals)*100.0,
		bankDurationWithGlobals, bankDurationLean)

	// Clean up global indexes so we leave the database in the 042 state
	_, _ = pool.Exec(ctx, `DROP INDEX IF EXISTS idx_bank_transactions_narration_trgm;`)
	_, _ = pool.Exec(ctx, `DROP INDEX IF EXISTS idx_bank_transactions_txnid_trgm;`)
}
