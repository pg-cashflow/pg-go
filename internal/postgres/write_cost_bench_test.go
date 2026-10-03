package postgres

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/csv"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// TestWriteBenchmark_FairAB measures the write performance difference between the
// pre-042 index set (with 2 redundant global GINs) and the post-042 lean index set
// under fair A/B conditions: identical 100k data, fresh tables, alternating order, 3 trials.
func TestWriteBenchmark_FairAB(t *testing.T) {
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

	// Ensure extensions
	_, _ = pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS "uuid-ossp"; CREATE EXTENSION IF NOT EXISTS pg_trgm;`)

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS bench_bank_lean CASCADE;`)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS bench_bank_global CASCADE;`)
	})

	propID := uuid.New()

	createTables := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS bench_bank_lean CASCADE;`)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS bench_bank_global CASCADE;`)

		// 1. Lean table (042 state: property-scoped GINs + prefix btree; NO redundant global GINs)
		_, err := pool.Exec(ctx, `
			CREATE TABLE bench_bank_lean (
				id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
				property_id UUID NOT NULL,
				txn_id TEXT NOT NULL,
				amount_paise BIGINT NOT NULL,
				row_type TEXT NOT NULL DEFAULT 'credit',
				txn_date DATE NOT NULL,
				narration TEXT,
				dedup_hash TEXT NOT NULL,
				status TEXT NOT NULL DEFAULT 'unmatched',
				CONSTRAINT uq_bench_bank_lean_prop_dedup UNIQUE (property_id, dedup_hash)
			);
			CREATE INDEX idx_bench_lean_prop_date ON bench_bank_lean(property_id, txn_date DESC);
			CREATE INDEX idx_bench_lean_prop_status ON bench_bank_lean(property_id, status);
			CREATE INDEX idx_bench_lean_txnid_prefix ON bench_bank_lean(property_id, lower(txn_id) text_pattern_ops);
			CREATE INDEX idx_bench_lean_prop_narr_trgm ON bench_bank_lean USING gin (property_id, narration gin_trgm_ops);
			CREATE INDEX idx_bench_lean_prop_txn_trgm ON bench_bank_lean USING gin (property_id, txn_id gin_trgm_ops);
		`)
		if err != nil {
			t.Fatalf("create bench_bank_lean failed: %v", err)
		}

		// 2. Global table (pre-042 state: identical indexes PLUS the 2 redundant global GINs)
		_, err = pool.Exec(ctx, `
			CREATE TABLE bench_bank_global (
				id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
				property_id UUID NOT NULL,
				txn_id TEXT NOT NULL,
				amount_paise BIGINT NOT NULL,
				row_type TEXT NOT NULL DEFAULT 'credit',
				txn_date DATE NOT NULL,
				narration TEXT,
				dedup_hash TEXT NOT NULL,
				status TEXT NOT NULL DEFAULT 'unmatched',
				CONSTRAINT uq_bench_bank_global_prop_dedup UNIQUE (property_id, dedup_hash)
			);
			CREATE INDEX idx_bench_glob_prop_date ON bench_bank_global(property_id, txn_date DESC);
			CREATE INDEX idx_bench_glob_prop_status ON bench_bank_global(property_id, status);
			CREATE INDEX idx_bench_glob_txnid_prefix ON bench_bank_global(property_id, lower(txn_id) text_pattern_ops);
			CREATE INDEX idx_bench_glob_prop_narr_trgm ON bench_bank_global USING gin (property_id, narration gin_trgm_ops);
			CREATE INDEX idx_bench_glob_prop_txn_trgm ON bench_bank_global USING gin (property_id, txn_id gin_trgm_ops);
			-- Redundant global GIN indexes present before 042:
			CREATE INDEX idx_bench_glob_narr_trgm ON bench_bank_global USING gin (narration gin_trgm_ops);
			CREATE INDEX idx_bench_glob_txn_trgm ON bench_bank_global USING gin (txn_id gin_trgm_ops);
		`)
		if err != nil {
			t.Fatalf("create bench_bank_global failed: %v", err)
		}
	}

	const rowCount = 100000
	const trials = 3

	type trialResult struct {
		leanDur   time.Duration
		globalDur time.Duration
	}
	results := make([]trialResult, trials)

	insertTable := func(tableName string, seedOffset int) time.Duration {
		t0 := time.Now()
		_, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (id, property_id, txn_id, amount_paise, row_type, txn_date, narration, dedup_hash, status)
			SELECT
				gen_random_uuid(),
				$1::uuid,
				'TXN-BENCH-' || i,
				500000,
				'credit',
				CURRENT_DATE - (i %% 90),
				'NEFT CR RENT FROM TENANT ' || (i %% 100) || ' REF ' || (500000000000 + i),
				md5($1::text || (i + $2)::text),
				'unmatched'
			FROM generate_series(1, %d) AS s(i)`, tableName, rowCount),
			propID, seedOffset,
		)
		if err != nil {
			t.Fatalf("insert into %s failed: %v", tableName, err)
		}
		return time.Since(t0)
	}

	for trial := 0; trial < trials; trial++ {
		createTables()
		_, _ = pool.Exec(ctx, `CHECKPOINT;`)

		var dLean, dGlobal time.Duration
		// Alternating order: Trial 0: Lean then Global; Trial 1: Global then Lean; Trial 2: Lean then Global
		if trial%2 == 0 {
			t.Logf("Trial %d: Inserting Lean first, then Global...", trial+1)
			dLean = insertTable("bench_bank_lean", trial*rowCount)
			_, _ = pool.Exec(ctx, `CHECKPOINT;`)
			dGlobal = insertTable("bench_bank_global", trial*rowCount)
		} else {
			t.Logf("Trial %d: Inserting Global first, then Lean...", trial+1)
			dGlobal = insertTable("bench_bank_global", trial*rowCount)
			_, _ = pool.Exec(ctx, `CHECKPOINT;`)
			dLean = insertTable("bench_bank_lean", trial*rowCount)
		}

		results[trial] = trialResult{leanDur: dLean, globalDur: dGlobal}
		speedup := float64(dGlobal-dLean) / float64(dGlobal) * 100.0
		t.Logf("Trial %d: Lean (042)=%v (%.0f rows/s) | Global (pre-042)=%v (%.0f rows/s) -> %.1f%% faster",
			trial+1, dLean, float64(rowCount)/dLean.Seconds(), dGlobal, float64(rowCount)/dGlobal.Seconds(), speedup)
	}

	leanDurs := make([]time.Duration, trials)
	globalDurs := make([]time.Duration, trials)
	for i, r := range results {
		leanDurs[i] = r.leanDur
		globalDurs[i] = r.globalDur
	}
	sort.Slice(leanDurs, func(i, j int) bool { return leanDurs[i] < leanDurs[j] })
	sort.Slice(globalDurs, func(i, j int) bool { return globalDurs[i] < globalDurs[j] })

	medianLean := leanDurs[trials/2]
	medianGlobal := globalDurs[trials/2]
	fairSpeedup := float64(medianGlobal-medianLean) / float64(medianGlobal) * 100.0

	t.Logf("================================================================================")
	t.Logf("FAIR A/B WRITE BENCHMARK SUMMARY (%d identical rows, fresh tables, alternating order, 3 trials):", rowCount)
	t.Logf("  Lean Set (042 State) Median:        %v (%.0f rows/sec)", medianLean, float64(rowCount)/medianLean.Seconds())
	t.Logf("  Global Set (Pre-042 State) Median:  %v (%.0f rows/sec)", medianGlobal, float64(rowCount)/medianGlobal.Seconds())
	t.Logf("  Fair Write Speedup (Median):        %.1f%% faster", fairSpeedup)
	t.Logf("================================================================================")

	// Baselines for dues and payments
	t.Log("Measuring baseline write cost for dues and payments (20,000 rows)...")
	t0 := time.Now()
	_, err = pool.Exec(ctx, `
		WITH t_arr AS (
			SELECT array_agg(id) AS ids FROM (SELECT gen_random_uuid() AS id FROM generate_series(1, 100)) s
		)
		INSERT INTO dues (id, tenant_id, property_id, due_code, kind, amount, original_amount, period_start, period_end, due_date, status)
		SELECT
			gen_random_uuid(),
			t_arr.ids[(i % cardinality(t_arr.ids)) + 1],
			$1::uuid,
			'D-BENCH-' || lpad(i::text, 6, '0'),
			'rent',
			500000,
			500000,
			CURRENT_DATE,
			CURRENT_DATE + interval '1 month',
			CURRENT_DATE + interval '5 days',
			'pending'
		FROM t_arr, generate_series(1, 20000) AS s(i)`,
		propID,
	)
	if err == nil {
		duesDur := time.Since(t0)
		t.Logf("  20,000 dues insert duration: %v (%.0f rows/sec)", duesDur, 20000.0/duesDur.Seconds())
		_, _ = pool.Exec(ctx, `DELETE FROM dues WHERE property_id = $1`, propID)
	}
}

// TestWriteBenchmark_RealBankCSVImport times a real bank statement CSV import through
// the repository import path (csv.ParseWithMeta -> BankTransactionRepo.InsertTransaction)
// before and after Migration 042.
func TestWriteBenchmark_RealBankCSVImport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bank CSV import benchmark in short mode")
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
	_, _ = pool.Exec(ctx, `
		INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
		VALUES ($1, 'CSV Import Bench Prop', '+919999999997', 'CSV Owner', 'csv@owner.test', 'csvtest1', 'csv@upi')
		ON CONFLICT (id) DO NOTHING`,
		propID,
	)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM bank_transactions WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
		// Ensure global indexes are dropped after test
		_, _ = pool.Exec(ctx, `DROP INDEX IF EXISTS idx_bank_transactions_narration_trgm;`)
		_, _ = pool.Exec(ctx, `DROP INDEX IF EXISTS idx_bank_transactions_txnid_trgm;`)
	})

	// Generate realistic SBI bank statement CSV
	const csvRows = 5000
	var sb strings.Builder
	sb.WriteString("Txn Date,Value Date,Description,Ref No./Cheque No.,Debit,Credit,Balance\n")
	for i := 1; i <= csvRows; i++ {
		sb.WriteString(fmt.Sprintf("01/08/2026,01/08/2026,NEFT CR RENT TENANT %d REF %d,TXN%08d,,5000.00,%d.00\n",
			i%500, 700000000000+i, i, 100000+i*5000))
	}

	csvData := sb.String()
	parseRes, err := csv.ParseWithMeta(strings.NewReader(csvData))
	if err != nil {
		t.Fatalf("parse CSV failed: %v", err)
	}
	t.Logf("Parsed %d CSV rows successfully", len(parseRes.Rows))

	repo := NewBankTransactionRepo(pool)

	// Run A: Pre-042 state (with the 2 redundant global GINs added)
	_, _ = pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_bank_transactions_narration_trgm ON bank_transactions USING gin (narration gin_trgm_ops);`)
	_, _ = pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_bank_transactions_txnid_trgm ON bank_transactions USING gin (txn_id gin_trgm_ops);`)
	_, _ = pool.Exec(ctx, `DELETE FROM bank_transactions WHERE property_id = $1`, propID)
	_, _ = pool.Exec(ctx, `CHECKPOINT;`)

	t0 := time.Now()
	for _, row := range parseRes.Rows {
		txn := &domain.BankTransaction{
			PropertyID:  propID,
			TxnID:       row.TxnID,
			AmountPaise: int64(row.AmountPaise),
			RowType:     "credit",
			TxnDate:     row.Date,
			Narration:   row.Note,
			Status:      domain.BankTxnUnmatched,
		}
		if _, err := repo.InsertTransaction(ctx, nil, txn); err != nil {
			t.Fatalf("insert pre-042 failed: %v", err)
		}
	}
	durPre042 := time.Since(t0)
	t.Logf("Bank CSV Import (Pre-042 with global GINs, %d rows): %v (%.0f rows/sec)",
		csvRows, durPre042, float64(csvRows)/durPre042.Seconds())

	// Run B: Post-042 state (lean set: global GINs dropped)
	_, _ = pool.Exec(ctx, `DROP INDEX IF EXISTS idx_bank_transactions_narration_trgm;`)
	_, _ = pool.Exec(ctx, `DROP INDEX IF EXISTS idx_bank_transactions_txnid_trgm;`)
	_, _ = pool.Exec(ctx, `DELETE FROM bank_transactions WHERE property_id = $1`, propID)
	_, _ = pool.Exec(ctx, `CHECKPOINT;`)

	t0 = time.Now()
	for _, row := range parseRes.Rows {
		txn := &domain.BankTransaction{
			PropertyID:  propID,
			TxnID:       row.TxnID,
			AmountPaise: int64(row.AmountPaise),
			RowType:     "credit",
			TxnDate:     row.Date,
			Narration:   row.Note,
			Status:      domain.BankTxnUnmatched,
		}
		if _, err := repo.InsertTransaction(ctx, nil, txn); err != nil {
			t.Fatalf("insert post-042 failed: %v", err)
		}
	}
	durPost042 := time.Since(t0)
	t.Logf("Bank CSV Import (Post-042 lean set, %d rows): %v (%.0f rows/sec)",
		csvRows, durPost042, float64(csvRows)/durPost042.Seconds())

	importSpeedup := float64(durPre042-durPost042) / float64(durPre042) * 100.0
	t.Logf("================================================================================")
	t.Logf("REAL BANK CSV IMPORT REPO PATH SUMMARY (%d rows):", csvRows)
	t.Logf("  Pre-042 State:  %v (%.0f rows/sec)", durPre042, float64(csvRows)/durPre042.Seconds())
	t.Logf("  Post-042 State: %v (%.0f rows/sec)", durPost042, float64(csvRows)/durPost042.Seconds())
	t.Logf("  Speedup:        %.1f%% faster", importSpeedup)
	t.Logf("================================================================================")
}
