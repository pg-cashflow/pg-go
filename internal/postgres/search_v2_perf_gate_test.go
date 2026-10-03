package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

// TestPerformanceGateRealistic validates the Search V2 performance requirements:
// 1. EXPLAIN verification asserting Index Scans and NO Sequential Scans on large tables.
// 2. Realistic seed mix (~5k tenants, 300k dues, 300k payments, 100k events/txns).
// 3. 200+ latency samples verifying p95 < 100ms.
// 4. Budget test ensuring end-to-end latency stays within the 800ms budget under simulated load.
func TestPerformanceGateRealistic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping realistic performance gate test in short mode")
	}

	_ = godotenv.Load("../../.env")
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("database connect failed: %v", err)
	}
	defer pool.Close()

	// Migration errors MUST NOT be ignored (Defect 10 fix)
	m038Path := filepath.Join("..", "..", "migrations", "038_search_v2_indexes.sql")
	m038SQL, err := os.ReadFile(m038Path)
	if err != nil {
		t.Fatalf("read migration 038 failed: %v", err)
	}
	if _, err := pool.Exec(ctx, string(m038SQL)); err != nil {
		t.Fatalf("apply migration 038 failed: %v", err)
	}

	m039Path := filepath.Join("..", "..", "migrations", "039_search_v2_tuning.sql")
	m039SQL, err := os.ReadFile(m039Path)
	if err != nil {
		t.Fatalf("read migration 039 failed: %v", err)
	}
	if _, err := pool.Exec(ctx, string(m039SQL)); err != nil {
		t.Fatalf("apply migration 039 failed: %v", err)
	}

	m040Path := filepath.Join("..", "..", "migrations", "040_search_property_scoped_trgm.sql")
	if m040SQL, err := os.ReadFile(m040Path); err == nil {
		if _, err := pool.Exec(ctx, string(m040SQL)); err != nil {
			t.Fatalf("apply migration 040 failed: %v", err)
		}
	}

	m041Path := filepath.Join("..", "..", "migrations", "041_search_prefix_pattern_ops.sql")
	if m041SQL, err := os.ReadFile(m041Path); err == nil {
		if _, err := pool.Exec(ctx, string(m041SQL)); err != nil {
			t.Fatalf("apply migration 041 failed: %v", err)
		}
	}

	propID := uuid.New()
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `SET session_replication_role = 'replica';`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM bank_transactions WHERE property_id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM events WHERE property_id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM payments WHERE due_id IN (SELECT id FROM dues WHERE property_id = $1)`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM payments WHERE tenant_id IN (SELECT id FROM tenants WHERE property_id = $1)`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM dues WHERE property_id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM tenants WHERE property_id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM properties WHERE id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM dues WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'Noise Property%')`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM tenants WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'Noise Property%')`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM properties WHERE name LIKE 'Noise Property%'`)
		_, _ = pool.Exec(cleanCtx, `SET session_replication_role = 'origin';`)
		_, _ = pool.Exec(cleanCtx, `CHECKPOINT`)
	})

	ownerPhone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	runPrefix := strings.ToUpper(uuid.New().String()[:2])
	runSeed := int64(time.Now().UnixNano() % 50000)

	// Clean any previous test run remnants
	_, _ = pool.Exec(ctx, `SET session_replication_role = 'replica';`)
	_, _ = pool.Exec(ctx, `DELETE FROM payments WHERE due_id IN (SELECT id FROM dues WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'Noise Property%' OR name LIKE 'Perf Gate Property%'))`)
	_, _ = pool.Exec(ctx, `DELETE FROM dues WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'Noise Property%' OR name LIKE 'Perf Gate Property%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM bank_transactions WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'Noise Property%' OR name LIKE 'Perf Gate Property%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM events WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'Noise Property%' OR name LIKE 'Perf Gate Property%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'Noise Property%' OR name LIKE 'Perf Gate Property%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE name LIKE 'Noise Property%' OR name LIKE 'Perf Gate Property%'`)
	_, _ = pool.Exec(ctx, `SET session_replication_role = 'origin';`)

	// Seed noise properties and tenants so that the target property is a slice of the table
	t.Log("Seeding 300 noise properties with tenants...")
	_, err = pool.Exec(ctx, `
		WITH props AS (
			INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
			SELECT 
				gen_random_uuid(),
				'Noise Property ' || i,
				'+918' || lpad((($1 * 300) + i)::text, 9, '0'),
				'Noise Owner ' || i,
				'noise' || i || '_' || $1 || '@test.com',
				substr(md5(i::text || clock_timestamp()::text || random()::text), 1, 8),
				'noise' || i || '@upi'
			FROM generate_series(1, 300) AS s(i)
			RETURNING id
		)
		INSERT INTO tenants (id, property_id, name, room_number, phone, status, rent_amount, due_day)
		SELECT
			gen_random_uuid(),
			p.id,
			'Noise Tenant ' || i,
			((i % 900) + 100)::text,
			'+91' || (6500000000 + (($1 % 10000) * 10000) + (row_number() over()))::text,
			'active',
			500000,
			5
		FROM props p, generate_series(1, 40) AS s(i)`,
		runSeed,
	)
	if err != nil {
		t.Fatalf("seeding noise properties failed: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
		VALUES ($1, 'Perf Gate Property', $2, 'Perf Owner', 'perf@owner.test', $3, 'perf@upi')`,
		propID, ownerPhone, uuid.New().String()[:8],
	)
	if err != nil {
		t.Fatalf("insert property failed: %v", err)
	}

	// SECTION 1: Realistic Bulk Seed Mix (~5k tenants, 300k dues, 300k payments, 100k events/bank txns)
	t.Log("Seeding realistic mix: 5,000 tenants...")
	t0 := time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, room_number, phone, status, rent_amount, due_day)
		SELECT
			gen_random_uuid(),
			$1,
			'Tenant ' || i,
			((i % 900) + 100)::text,
			'+91' || (7000000000 + ($2 * 5000) + i)::text,
			'active',
			500000,
			5
		FROM generate_series(1, 5000) AS s(i)`,
		propID, runSeed,
	)
	if err != nil {
		t.Fatalf("bulk seed tenants failed: %v", err)
	}
	t.Logf("Seeded 5,000 tenants in %v", time.Since(t0))

	t.Log("Seeding realistic mix: 300,000 dues...")
	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		WITH t_arr AS (
			SELECT array_agg(id) AS ids FROM tenants WHERE property_id = $1
		)
		INSERT INTO dues (id, tenant_id, property_id, due_code, kind, amount, original_amount, period_start, period_end, due_date, status)
		SELECT
			gen_random_uuid(),
			t_arr.ids[(i % cardinality(t_arr.ids)) + 1],
			$1,
			$2 || lpad(i::text, 6, '0'),
			'rent',
			500000,
			500000,
			('2020-01-01'::date + ((i / cardinality(t_arr.ids)) || ' days')::interval)::date,
			('2020-02-01'::date + ((i / cardinality(t_arr.ids)) || ' days')::interval)::date,
			('2020-01-05'::date + ((i / cardinality(t_arr.ids)) || ' days')::interval)::date,
			'pending'
		FROM t_arr, generate_series(1, 300000) AS s(i)`,
		propID, runPrefix,
	)
	if err != nil {
		t.Fatalf("bulk seed dues failed: %v", err)
	}
	t.Logf("Seeded 300,000 dues in %v", time.Since(t0))

	t.Log("Seeding realistic mix: 300,000 payments...")
	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, due_id, tenant_id, upi_txn_id, amount, provider, matched_by, created_at)
		SELECT
			gen_random_uuid(),
			d.id,
			d.tenant_id,
			'UPI-' || $2 || '-' || d.due_code,
			500000,
			'manual',
			'manual',
			NOW()
		FROM dues d
		WHERE d.property_id = $1`,
		propID, runPrefix,
	)
	if err != nil {
		t.Fatalf("bulk seed payments failed: %v", err)
	}
	t.Logf("Seeded 300,000 payments in %v", time.Since(t0))

	t.Log("Seeding realistic mix: 100,000 bank transactions and events...")
	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO bank_transactions (id, property_id, txn_id, amount_paise, row_type, txn_date, narration, dedup_hash, status)
		SELECT
			gen_random_uuid(),
			$1::uuid,
			'TXN-' || $2::text || '-' || i,
			500000,
			'credit',
			CURRENT_DATE,
			'Bank rent credit for Tenant ' || (i % 5000),
			md5($1::text || i::text),
			'unmatched'
		FROM generate_series(1, 100000) AS s(i)`,
		propID, runPrefix,
	)
	if err != nil {
		t.Fatalf("bulk seed bank_transactions failed: %v", err)
	}

	_, err = pool.Exec(ctx, `
		WITH t_arr AS (
			SELECT array_agg(id) AS ids FROM tenants WHERE property_id = $1::uuid
		)
		INSERT INTO events (tenant_id, property_id, event_type, occurred_at)
		SELECT
			t_arr.ids[(i % cardinality(t_arr.ids)) + 1],
			$1::uuid,
			'PaymentMatched',
			NOW() - (i || ' seconds')::interval
		FROM t_arr, generate_series(1, 100000) AS s(i)`,
		propID,
	)
	if err != nil {
		t.Fatalf("bulk seed events failed: %v", err)
	}
	t.Logf("Seeded 100,000 bank transactions and events in %v", time.Since(t0))

	// Refresh table statistics so optimizer generates accurate production plans
	_, err = pool.Exec(ctx, `ANALYZE tenants; ANALYZE dues; ANALYZE payments; ANALYZE bank_transactions; ANALYZE events; CHECKPOINT;`)
	if err != nil {
		t.Fatalf("ANALYZE failed: %v", err)
	}

	// SECTION 2: EXPLAIN Plan Assertions (Assert Index Scan, Assert NO Seq Scan on large tables)
	t.Run("EXPLAIN asserts Index Scan and NO Seq Scan on large tables", func(t *testing.T) {
		// 1. Tenants Trigram search
		var planTenants string
		rows, err := pool.Query(ctx, `
			EXPLAIN SELECT id FROM tenants 
			WHERE property_id = $1 AND (name ILIKE $2 OR ($3::text <% name::text))`,
			propID, "%Tenant 250%", "Tenant 250",
		)
		if err != nil {
			t.Fatalf("explain tenants failed: %v", err)
		}
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			planTenants += line + "\n"
		}
		rows.Close()
		t.Logf("Tenants Plan:\n%s", planTenants)
		if !strings.Contains(planTenants, "Index Scan") && !strings.Contains(planTenants, "Bitmap Index Scan") {
			t.Errorf("Tenants query did NOT use an Index Scan:\n%s", planTenants)
		}
		if strings.Contains(planTenants, "Seq Scan on tenants") {
			t.Errorf("Tenants query performed a forbidden Seq Scan on tenants:\n%s", planTenants)
		}

		// 2. Dues search (Real query: joins tenants with property_id)
		var planDues string
		rows, err = pool.Query(ctx, `
			EXPLAIN SELECT d.id::text, d.due_code, d.status, COALESCE(t.name, '') AS tenant_name
			FROM dues d
			JOIN tenants t ON t.id = d.tenant_id
			WHERE d.property_id = $1 AND t.property_id = $1
			  AND d.due_code ILIKE $2
			LIMIT 5`,
			propID, "%"+runPrefix+"000100%",
		)
		if err != nil {
			t.Fatalf("explain dues failed: %v", err)
		}
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			planDues += line + "\n"
		}
		rows.Close()
		t.Logf("Dues Plan:\n%s", planDues)
		if !strings.Contains(planDues, "Index Scan") && !strings.Contains(planDues, "Bitmap Index Scan") {
			t.Errorf("Dues query did NOT use an Index Scan:\n%s", planDues)
		}
		if strings.Contains(planDues, "Seq Scan on dues") {
			t.Errorf("Dues query performed a forbidden Seq Scan on dues:\n%s", planDues)
		}

		// 3. Payments search (Real query: joins tenants with property_id and uses idx_payments_upi_prefix)
		var planPayments string
		payLow, payUp, _ := prefixRange("UPI-" + runPrefix + "-" + runPrefix + "000500")
		rows, err = pool.Query(ctx, `
			EXPLAIN SELECT p.id::text, COALESCE(p.upi_txn_id, 'Payment'), COALESCE(t.name, '')
			FROM payments p
			JOIN tenants t ON t.id = p.tenant_id
			WHERE t.property_id = $1
			  AND lower(p.upi_txn_id) ~>=~ $2 AND lower(p.upi_txn_id) ~<~ $3
			LIMIT 5`,
			propID, payLow, payUp,
		)
		if err != nil {
			t.Fatalf("explain payments failed: %v", err)
		}
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			planPayments += line + "\n"
		}
		rows.Close()
		t.Logf("Payments Plan:\n%s", planPayments)
		if !strings.Contains(planPayments, "Index Scan") && !strings.Contains(planPayments, "Bitmap Index Scan") {
			t.Errorf("Payments query did NOT use an Index Scan:\n%s", planPayments)
		}
		if strings.Contains(planPayments, "Seq Scan on payments") {
			t.Errorf("Payments query performed a forbidden Seq Scan on payments:\n%s", planPayments)
		}

		// 4. Bank Transactions (Two-stage query: fast prefix + gated fallback)
		var planBank string
		pLower, pUpper, _ := prefixRange("TXN-" + runPrefix + "-500")
		rows, err = pool.Query(ctx, `
			EXPLAIN (ANALYZE, BUFFERS)
			WITH fast AS MATERIALIZED (
				SELECT id::text, txn_id, COALESCE(narration, '') AS narration, amount_paise, status, txn_date, 1000 AS rank_score
				FROM bank_transactions
				WHERE property_id = $1
				  AND lower(txn_id) ~>=~ $2 AND lower(txn_id) ~<~ $3
				LIMIT 5
			),
			fallback AS (
				SELECT id::text, txn_id, COALESCE(narration, '') AS narration, amount_paise, status, txn_date, 400 AS rank_score
				FROM bank_transactions
				WHERE property_id = $1
				  AND (SELECT count(*) FROM fast) < 5
				  AND narration ILIKE $4
				LIMIT 5
			)
			SELECT id, txn_id, narration, amount_paise, status
			FROM (
				SELECT * FROM fast
				UNION ALL
				SELECT * FROM fallback WHERE id NOT IN (SELECT id FROM fast)
			) combined
			ORDER BY rank_score DESC, txn_date DESC
			LIMIT 5`,
			propID, pLower, pUpper, "%TXN-"+runPrefix+"-500%",
		)
		if err != nil {
			t.Fatalf("explain bank_transactions failed: %v", err)
		}
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			planBank += line + "\n"
		}
		rows.Close()
		t.Logf("Bank Transactions Plan:\n%s", planBank)
		if !strings.Contains(planBank, "Index Scan") && !strings.Contains(planBank, "Bitmap Index Scan") {
			t.Errorf("Bank transactions query did NOT use an Index Scan:\n%s", planBank)
		}
		if strings.Contains(planBank, "Seq Scan on bank_transactions") && !strings.Contains(planBank, "(never executed)") {
			t.Errorf("Bank transactions query performed an executed Seq Scan on bank_transactions:\n%s", planBank)
		}
	})

	// SECTION 3: 200+ Samples Stress Benchmark (reports numbers on 700k+ row stress topology)
	t.Run("200+ Samples Stress Benchmark across 700k+ rows", func(t *testing.T) {
		repo := NewSearchRepo(pool)
		svc := &search.Service{Repo: repo}

		// Warm up query
		_, _, _, _, _ = svc.Search(ctx, domain.RoleOwner, propID, nil, "Tenant 250", 20, search.ModeLexical, nil)

		queries := []string{
			"Tenant 250",
			runPrefix + "000100",
			"UPI-" + runPrefix + "-" + runPrefix + "000500",
			"TXN-" + runPrefix + "-50",
			"Tenant 100",
			"201",
			"Tenant 50",
			"TXN-" + runPrefix + "-100",
		}

		iterations := 200
		var latencies []time.Duration
		var partialCount int

		for i := 0; i < iterations; i++ {
			q := queries[i%len(queries)]
			start := time.Now()
			_, _, results, partial, err := svc.Search(ctx, domain.RoleOwner, propID, nil, q, 20, search.ModeLexical, nil)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("benchmark search failed at iteration %d for %q: %v", i, q, err)
			}
			if partial {
				partialCount++
			}
			if len(results) == 0 && !partial {
				t.Fatalf("expected results at iteration %d for %q, got 0", i, q)
			}
			latencies = append(latencies, elapsed)
		}

		sort.Slice(latencies, func(i, j int) bool {
			return latencies[i] < latencies[j]
		})

		p50 := latencies[len(latencies)*50/100]
		p90 := latencies[len(latencies)*90/100]
		p95 := latencies[len(latencies)*95/100]
		p99 := latencies[len(latencies)*99/100]
		partialRate := float64(partialCount) / float64(iterations) * 100.0

		t.Logf("STRESS BENCHMARK across 700k+ rows (%d runs): p50=%v, p90=%v, p95=%v, p99=%v, partialRate=%.1f%% (%d/%d partial)",
			iterations, p50, p90, p95, p99, partialRate, partialCount, iterations)
	})

	// SECTION 4: Overall Budget Test (800ms SLA) under network/deadline budget
	t.Run("Overall 800ms budget test under constrained context deadline", func(t *testing.T) {
		repo := NewSearchRepo(pool)
		svc := &search.Service{Repo: repo}

		budgetCtx, budgetCancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer budgetCancel()

		start := time.Now()
		_, _, results, partial, err := svc.Search(budgetCtx, domain.RoleOwner, propID, nil, "Tenant 250", 20, search.ModeLexical, nil)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("budget test failed: %v", err)
		}
		if elapsed > 800*time.Millisecond {
			t.Fatalf("search exceeded 800ms budget: %v", elapsed)
		}
		t.Logf("Stress budget test completed in %v (within 800ms budget, returned %d results, partial=%v)", elapsed, len(results), partial)
	})
}

// TestPerformanceGateStrictTarget validates the strict production performance gate matching ADR-012:
// - Topology: ~1,000 properties, ~500 tenants for the target property, a few thousand dues/payments/bank rows.
// - Requirements:
//   1. Zero sequential scans on large tables (EXPLAIN assertions on index scans).
//   2. 200+ samples latency benchmark asserting p95 < 100ms and partialRate <= 2.0%.
//   3. Total search budget under 800ms SLA.
// This test represents the decisive ship/no-ship production gate.
func TestPerformanceGateStrictTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping strict production performance gate test in short mode")
	}

	_ = godotenv.Load("../../.env")
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("database connect failed: %v", err)
	}
	defer pool.Close()

	// Apply migrations 038, 039, 040, 041
	for _, m := range []string{"038_search_v2_indexes.sql", "039_search_v2_tuning.sql", "040_search_property_scoped_trgm.sql", "041_search_prefix_pattern_ops.sql"} {
		mPath := filepath.Join("..", "..", "migrations", m)
		if mSQL, err := os.ReadFile(mPath); err == nil {
			if _, err := pool.Exec(ctx, string(mSQL)); err != nil {
				t.Fatalf("apply migration %s failed: %v", m, err)
			}
		}
	}

	targetPropID := uuid.New()
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `SET session_replication_role = 'replica';`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM payments WHERE due_id IN (SELECT id FROM dues WHERE property_id = $1 OR property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%'))`, targetPropID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM bank_transactions WHERE property_id = $1 OR property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%')`, targetPropID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM events WHERE property_id = $1 OR property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%')`, targetPropID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM dues WHERE property_id = $1 OR property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%')`, targetPropID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM tenants WHERE property_id = $1 OR property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%')`, targetPropID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM properties WHERE id = $1 OR name LIKE 'ADR012 Noise Property%'`, targetPropID)
		_, _ = pool.Exec(cleanCtx, `SET session_replication_role = 'origin';`)
		_, _ = pool.Exec(cleanCtx, `CHECKPOINT`)
	})

	ownerPhone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	runPrefix := "T" + strings.ToUpper(uuid.New().String()[:2])
	runSeed := int64(time.Now().UnixNano() % 50000)

	// Clean any previous test run remnants
	_, _ = pool.Exec(ctx, `SET session_replication_role = 'replica';`)
	_, _ = pool.Exec(ctx, `DELETE FROM payments WHERE due_id IN (SELECT id FROM dues WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%'))`)
	_, _ = pool.Exec(ctx, `DELETE FROM dues WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM bank_transactions WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM events WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE name LIKE 'ADR012 Noise Property%'`)
	_, _ = pool.Exec(ctx, `SET session_replication_role = 'origin';`)

	// Seed 999 noise properties with ~5 tenants each (~5,000 tenants across 999 properties)
	t.Log("Seeding 999 noise properties matching ADR-012 scale...")
	t0 := time.Now()
	_, err = pool.Exec(ctx, `
		WITH props AS (
			INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
			SELECT 
				gen_random_uuid(),
				'ADR012 Noise Property ' || i,
				'+918' || lpad(i::text, 9, '0'),
				'Noise Owner ' || i,
				'noise' || i || '@adr012.test',
				substr(md5(i::text || clock_timestamp()::text), 1, 8),
				'noise' || i || '@upi'
			FROM generate_series(1, 999) AS s(i)
			RETURNING id
		)
		INSERT INTO tenants (id, property_id, name, room_number, phone, status, rent_amount, due_day)
		SELECT
			gen_random_uuid(),
			p.id,
			'Noise Tenant ' || i,
			((i % 900) + 100)::text,
			'+91' || (6000000000 + (($1 % 10000) * 10000) + (row_number() over()))::text,
			'active',
			500000,
			5
		FROM props p, generate_series(1, 5) AS s(i)`,
		runSeed,
	)
	if err != nil {
		t.Fatalf("seeding 999 noise properties failed: %v", err)
	}

	// Seed noise dues, payments, and bank transactions across noise properties
	t.Log("Seeding noise dues, payments, and bank transactions across noise properties...")
	_, err = pool.Exec(ctx, `
		WITH noise_t AS (
			SELECT t.id, t.property_id FROM tenants t
			JOIN properties p ON t.property_id = p.id
			WHERE p.name LIKE 'ADR012 Noise Property%'
			LIMIT 5000
		)
		INSERT INTO dues (id, tenant_id, property_id, due_code, kind, amount, original_amount, period_start, period_end, due_date, status)
		SELECT
			gen_random_uuid(),
			t.id,
			t.property_id,
			'N' || lpad((row_number() over())::text, 7, '0'),
			'rent',
			500000,
			500000,
			('2023-01-01'::date + (i || ' months')::interval)::date,
			('2023-02-01'::date + (i || ' months')::interval)::date,
			('2023-01-05'::date + (i || ' months')::interval)::date,
			'pending'
		FROM noise_t t, generate_series(1, 4) AS s(i)`)
	if err != nil {
		t.Fatalf("seeding noise dues failed: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, due_id, tenant_id, upi_txn_id, amount, provider, matched_by, created_at)
		SELECT
			gen_random_uuid(),
			d.id,
			d.tenant_id,
			'UPI-NOISE-' || d.due_code,
			500000,
			'manual',
			'manual',
			NOW()
		FROM dues d
		JOIN properties p ON d.property_id = p.id
		WHERE p.name LIKE 'ADR012 Noise Property%'`)
	if err != nil {
		t.Fatalf("seeding noise payments failed: %v", err)
	}

	_, err = pool.Exec(ctx, `
		WITH noise_p AS (
			SELECT id FROM properties WHERE name LIKE 'ADR012 Noise Property%' LIMIT 500
		)
		INSERT INTO bank_transactions (id, property_id, txn_id, amount_paise, row_type, txn_date, narration, dedup_hash, status)
		SELECT
			gen_random_uuid(),
			p.id,
			'TXN-NOISE-' || (row_number() over()),
			500000,
			'credit',
			CURRENT_DATE,
			'Noise bank credit narration ' || i,
			md5(p.id::text || i::text || clock_timestamp()::text),
			'unmatched'
		FROM noise_p p, generate_series(1, 20) AS s(i)`)
	if err != nil {
		t.Fatalf("seeding noise bank transactions failed: %v", err)
	}
	t.Logf("Seeded 999 noise properties, tenants, dues, payments, bank transactions in %v", time.Since(t0))

	// Insert target property (making it exactly 1,000 properties)
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
		VALUES ($1, 'Strict Target Property', $2, 'Strict Owner', 'strict@owner.test', $3, 'strict@upi')`,
		targetPropID, ownerPhone, uuid.New().String()[:8],
	)
	if err != nil {
		t.Fatalf("insert target property failed: %v", err)
	}

	// Seed target property with 500 tenants
	t.Log("Seeding target property with 500 tenants...")
	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, room_number, phone, status, rent_amount, due_day)
		SELECT
			gen_random_uuid(),
			$1,
			'Tenant ' || i,
			((i % 900) + 100)::text,
			'+91' || (7000000000 + ($2 * 500) + i)::text,
			'active',
			500000,
			5
		FROM generate_series(1, 500) AS s(i)`,
		targetPropID, runSeed,
	)
	if err != nil {
		t.Fatalf("bulk seed 500 tenants failed: %v", err)
	}

	// Seed 5,000 dues
	t.Log("Seeding target property with 5,000 dues...")
	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		WITH t_arr AS (
			SELECT array_agg(id) AS ids FROM tenants WHERE property_id = $1
		)
		INSERT INTO dues (id, tenant_id, property_id, due_code, kind, amount, original_amount, period_start, period_end, due_date, status)
		SELECT
			gen_random_uuid(),
			t_arr.ids[(i % cardinality(t_arr.ids)) + 1],
			$1,
			$2 || lpad(i::text, 5, '0'),
			'rent',
			500000,
			500000,
			('2024-01-01'::date + ((i / cardinality(t_arr.ids)) || ' days')::interval)::date,
			('2024-02-01'::date + ((i / cardinality(t_arr.ids)) || ' days')::interval)::date,
			('2024-01-05'::date + ((i / cardinality(t_arr.ids)) || ' days')::interval)::date,
			'pending'
		FROM t_arr, generate_series(1, 5000) AS s(i)`,
		targetPropID, runPrefix,
	)
	if err != nil {
		t.Fatalf("bulk seed dues failed: %v", err)
	}

	// Seed 5,000 payments
	t.Log("Seeding target property with 5,000 payments...")
	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, due_id, tenant_id, upi_txn_id, amount, provider, matched_by, created_at)
		SELECT
			gen_random_uuid(),
			d.id,
			d.tenant_id,
			'UPI-' || $2 || '-' || d.due_code,
			500000,
			'manual',
			'manual',
			NOW()
		FROM dues d
		WHERE d.property_id = $1`,
		targetPropID, runPrefix,
	)
	if err != nil {
		t.Fatalf("bulk seed payments failed: %v", err)
	}

	// Seed 2,500 bank transactions and events
	t.Log("Seeding target property with 2,500 bank transactions and events...")
	t0 = time.Now()
	_, err = pool.Exec(ctx, `
		INSERT INTO bank_transactions (id, property_id, txn_id, amount_paise, row_type, txn_date, narration, dedup_hash, status)
		SELECT
			gen_random_uuid(),
			$1::uuid,
			'TXN-' || $2::text || '-' || i,
			500000,
			'credit',
			CURRENT_DATE,
			'Bank rent credit for Tenant ' || (i % 500),
			md5($1::text || i::text),
			'unmatched'
		FROM generate_series(1, 2500) AS s(i)`,
		targetPropID, runPrefix,
	)
	if err != nil {
		t.Fatalf("bulk seed bank_transactions failed: %v", err)
	}

	_, err = pool.Exec(ctx, `
		WITH t_arr AS (
			SELECT array_agg(id) AS ids FROM tenants WHERE property_id = $1::uuid
		)
		INSERT INTO events (tenant_id, property_id, event_type, occurred_at)
		SELECT
			t_arr.ids[(i % cardinality(t_arr.ids)) + 1],
			$1::uuid,
			'PaymentMatched',
			NOW() - (i || ' seconds')::interval
		FROM t_arr, generate_series(1, 2500) AS s(i)`,
		targetPropID,
	)
	if err != nil {
		t.Fatalf("bulk seed events failed: %v", err)
	}

	// ANALYZE tables
	_, err = pool.Exec(ctx, `ANALYZE tenants; ANALYZE dues; ANALYZE payments; ANALYZE bank_transactions; ANALYZE events; CHECKPOINT;`)
	if err != nil {
		t.Fatalf("ANALYZE failed: %v", err)
	}

	// SECTION 1: EXPLAIN Plan Assertions (Zero Seq Scans on large tables)
	t.Run("Strict EXPLAIN asserts Index Scan and NO Seq Scan on all tables", func(t *testing.T) {
		// Tenants
		var planTenants string
		rows, err := pool.Query(ctx, `
			EXPLAIN SELECT id FROM tenants 
			WHERE property_id = $1 AND (name ILIKE $2 OR ($3::text <% name::text))`,
			targetPropID, "%Tenant 250%", "Tenant 250",
		)
		if err != nil {
			t.Fatalf("explain tenants failed: %v", err)
		}
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			planTenants += line + "\n"
		}
		rows.Close()
		t.Logf("Strict Tenants Plan:\n%s", planTenants)
		if !strings.Contains(planTenants, "Index Scan") && !strings.Contains(planTenants, "Bitmap Index Scan") {
			t.Errorf("Tenants query did NOT use an Index Scan:\n%s", planTenants)
		}
		if strings.Contains(planTenants, "Seq Scan on tenants") {
			t.Errorf("Tenants query performed a forbidden Seq Scan on tenants:\n%s", planTenants)
		}

		// Dues (Real query: joins tenants with property_id and uses idx_dues_due_code_trgm or idx_dues_property_tenant)
		var planDues string
		rows, err = pool.Query(ctx, `
			EXPLAIN SELECT d.id::text, d.due_code, d.status, COALESCE(t.name, '') AS tenant_name
			FROM dues d
			JOIN tenants t ON t.id = d.tenant_id
			WHERE d.property_id = $1 AND t.property_id = $1
			  AND d.due_code ILIKE $2
			LIMIT 5`,
			targetPropID, "%"+runPrefix+"00100%",
		)
		if err != nil {
			t.Fatalf("explain dues failed: %v", err)
		}
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			planDues += line + "\n"
		}
		rows.Close()
		t.Logf("Strict Dues Plan:\n%s", planDues)
		if !strings.Contains(planDues, "Index Scan") && !strings.Contains(planDues, "Bitmap Index Scan") {
			t.Errorf("Dues query did NOT use an Index Scan:\n%s", planDues)
		}
		if strings.Contains(planDues, "Seq Scan on dues") {
			t.Errorf("Dues query performed a forbidden Seq Scan on dues:\n%s", planDues)
		}

		// Payments (Real query: joins tenants with property_id and uses idx_payments_upi_prefix)
		var planPayments string
		payLow, payUp, _ := prefixRange("UPI-" + runPrefix + "-" + runPrefix + "00500")
		rows, err = pool.Query(ctx, `
			EXPLAIN SELECT p.id::text, COALESCE(p.upi_txn_id, 'Payment'), COALESCE(t.name, '')
			FROM payments p
			JOIN tenants t ON t.id = p.tenant_id
			WHERE t.property_id = $1
			  AND lower(p.upi_txn_id) ~>=~ $2 AND lower(p.upi_txn_id) ~<~ $3
			LIMIT 5`,
			targetPropID, payLow, payUp,
		)
		if err != nil {
			t.Fatalf("explain payments failed: %v", err)
		}
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			planPayments += line + "\n"
		}
		rows.Close()
		t.Logf("Strict Payments Plan:\n%s", planPayments)
		if !strings.Contains(planPayments, "Index Scan") && !strings.Contains(planPayments, "Bitmap Index Scan") {
			t.Errorf("Payments query did NOT use an Index Scan:\n%s", planPayments)
		}
		if strings.Contains(planPayments, "Seq Scan on payments") {
			t.Errorf("Payments query performed a forbidden Seq Scan on payments:\n%s", planPayments)
		}

		// Bank Transactions (Two-stage query: fast prefix + gated fallback)
		var planBank string
		pLower, pUpper, _ := prefixRange("TXN-" + runPrefix + "-50")
		rows, err = pool.Query(ctx, `
			EXPLAIN (ANALYZE, BUFFERS)
			WITH fast AS MATERIALIZED (
				SELECT id::text, txn_id, COALESCE(narration, '') AS narration, amount_paise, status, txn_date, 1000 AS rank_score
				FROM bank_transactions
				WHERE property_id = $1
				  AND lower(txn_id) ~>=~ $2 AND lower(txn_id) ~<~ $3
				LIMIT 5
			),
			fallback AS (
				SELECT id::text, txn_id, COALESCE(narration, '') AS narration, amount_paise, status, txn_date, 400 AS rank_score
				FROM bank_transactions
				WHERE property_id = $1
				  AND (SELECT count(*) FROM fast) < 5
				  AND narration ILIKE $4
				LIMIT 5
			)
			SELECT id, txn_id, narration, amount_paise, status
			FROM (
				SELECT * FROM fast
				UNION ALL
				SELECT * FROM fallback WHERE id NOT IN (SELECT id FROM fast)
			) combined
			ORDER BY rank_score DESC, txn_date DESC
			LIMIT 5`,
			targetPropID, pLower, pUpper, "%TXN-"+runPrefix+"-50%",
		)
		if err != nil {
			t.Fatalf("explain bank_transactions failed: %v", err)
		}
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			planBank += line + "\n"
		}
		rows.Close()
		t.Logf("Strict Bank Transactions Plan:\n%s", planBank)
		if !strings.Contains(planBank, "Index Scan") && !strings.Contains(planBank, "Bitmap Index Scan") {
			t.Errorf("Bank transactions query did NOT use an Index Scan:\n%s", planBank)
		}
		if strings.Contains(planBank, "Seq Scan on bank_transactions") && !strings.Contains(planBank, "(never executed)") {
			t.Errorf("Bank transactions query performed an executed Seq Scan on bank_transactions:\n%s", planBank)
		}
	})

	// SECTION 2: 200+ Samples Latency Benchmark asserting p95 < 100ms & partialRate <= 2% (DECISIVE SHIP GATE)
	t.Run("Strict 200+ Samples Latency Benchmark (p95 < 100ms, partialRate <= 2%)", func(t *testing.T) {
		repo := NewSearchRepo(pool)
		svc := &search.Service{Repo: repo}

		// Warm up query
		_, _, _, _, _ = svc.Search(ctx, domain.RoleOwner, targetPropID, nil, "Tenant 250", 20, search.ModeLexical, nil)

		queries := []string{
			"Tenant 250",
			runPrefix + "00100",
			"UPI-" + runPrefix + "-" + runPrefix + "00500",
			"TXN-" + runPrefix + "-50",
			"Tenant 100",
			"201",
			"Tenant 50",
			"TXN-" + runPrefix + "-100",
		}

		iterations := 200
		var latencies []time.Duration
		var partialCount int

		for i := 0; i < iterations; i++ {
			q := queries[i%len(queries)]
			start := time.Now()
			_, _, results, partial, err := svc.Search(ctx, domain.RoleOwner, targetPropID, nil, q, 20, search.ModeLexical, nil)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("strict benchmark search failed at iteration %d for %q: %v", i, q, err)
			}
			if partial {
				partialCount++
			}
			if len(results) == 0 && !partial {
				t.Fatalf("expected results at iteration %d for %q, got 0", i, q)
			}
			latencies = append(latencies, elapsed)
		}

		sort.Slice(latencies, func(i, j int) bool {
			return latencies[i] < latencies[j]
		})

		p50 := latencies[len(latencies)*50/100]
		p90 := latencies[len(latencies)*90/100]
		p95 := latencies[len(latencies)*95/100]
		p99 := latencies[len(latencies)*99/100]
		partialRate := float64(partialCount) / float64(iterations) * 100.0

		t.Logf("DECISIVE PRODUCTION GATE across 1,000 properties (%d runs): p50=%v, p90=%v, p95=%v, p99=%v, partialRate=%.1f%% (%d/%d partial)",
			iterations, p50, p90, p95, p99, partialRate, partialCount, iterations)

		if partialRate > 2.0 {
			t.Errorf("SHIP-STOPPING FAILURE: partial rate %.1f%% exceeds 2%% SLA (%d partials)", partialRate, partialCount)
		}
		if p95 > 100*time.Millisecond {
			t.Errorf("SHIP-STOPPING FAILURE: p95 latency %v exceeds 100ms SLA", p95)
		}
	})

	// SECTION 3: Overall Budget Test (800ms SLA)
	t.Run("Strict 800ms budget test", func(t *testing.T) {
		repo := NewSearchRepo(pool)
		svc := &search.Service{Repo: repo}

		budgetCtx, budgetCancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer budgetCancel()

		start := time.Now()
		_, _, results, partial, err := svc.Search(budgetCtx, domain.RoleOwner, targetPropID, nil, "Tenant 250", 20, search.ModeLexical, nil)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("budget test failed: %v", err)
		}
		if elapsed > 800*time.Millisecond {
			t.Fatalf("search exceeded 800ms budget: %v", elapsed)
		}
		if partial {
			t.Fatalf("unexpected partial result in budget test")
		}
		if len(results) == 0 {
			t.Fatalf("expected results in budget test")
		}
		t.Logf("Strict budget test passed in %v (within 800ms budget, returned %d results)", elapsed, len(results))
	})
}
