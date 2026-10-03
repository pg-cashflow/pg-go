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

	propID := uuid.New()
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM bank_transactions WHERE property_id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM events WHERE property_id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM payments WHERE tenant_id IN (SELECT id FROM tenants WHERE property_id = $1)`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM dues WHERE property_id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM tenants WHERE property_id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM properties WHERE id = $1`, propID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM dues WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'Noise Property%')`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM tenants WHERE property_id IN (SELECT id FROM properties WHERE name LIKE 'Noise Property%')`)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM properties WHERE name LIKE 'Noise Property%'`)
		_, _ = pool.Exec(cleanCtx, `CHECKPOINT`)
	})

	ownerPhone := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1000000000)
	runPrefix := strings.ToUpper(uuid.New().String()[:2])
	runSeed := int64(time.Now().UnixNano() % 50000)

	// Seed noise properties and tenants so that the target property is a slice of the table
	t.Log("Seeding 300 noise properties with tenants...")
	_, err = pool.Exec(ctx, `
		WITH props AS (
			INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
			SELECT 
				gen_random_uuid(),
				'Noise Property ' || i,
				'+918' || lpad(i::text, 9, '0'),
				'Noise Owner ' || i,
				'noise' || i || '@test.com',
				lpad(i::text, 8, 'n'),
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
			'+917' || lpad((row_number() over())::text, 9, '0'),
			'active',
			500000,
			5
		FROM props p, generate_series(1, 40) AS s(i)`)
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

		// 2. Dues search
		var planDues string
		rows, err = pool.Query(ctx, `
			EXPLAIN SELECT d.id FROM dues d
			WHERE d.property_id = $1 AND d.due_code ILIKE $2`,
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

		// 3. Payments search
		var planPayments string
		rows, err = pool.Query(ctx, `
			EXPLAIN SELECT p.id FROM payments p
			WHERE p.upi_txn_id ILIKE $1`,
			"%UPI-" + runPrefix + "%000500%",
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

		// 4. Bank Transactions search
		var planBank string
		rows, err = pool.Query(ctx, `
			EXPLAIN SELECT id FROM bank_transactions
			WHERE property_id = $1 AND (txn_id ILIKE $2 OR narration ILIKE $2)`,
			propID, "%TXN-"+runPrefix+"-500%",
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
		if strings.Contains(planBank, "Seq Scan on bank_transactions") {
			t.Errorf("Bank transactions query performed a forbidden Seq Scan on bank_transactions:\n%s", planBank)
		}
	})

	// SECTION 3: 200+ Samples Latency Benchmark asserting p95 < 100ms
	t.Run("200+ Samples Latency Benchmark p95 < 100ms", func(t *testing.T) {
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

		t.Logf("Benchmark across 700k+ realistic rows (%d runs): p50=%v, p90=%v, p95=%v, p99=%v, partialRate=%.1f%% (%d/%d partial)",
			iterations, p50, p90, p95, p99, partialRate, partialCount, iterations)

		if partialRate > 2.0 {
			t.Errorf("Performance gate failure: partial rate %.1f%% exceeds 2%% SLA", partialRate)
		}
		if p95 > 100*time.Millisecond {
			t.Errorf("Performance gate failure: p95 latency %v exceeds 100ms SLA", p95)
		}
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
		if partial {
			t.Fatalf("unexpected partial result in budget test")
		}
		if len(results) == 0 {
			t.Fatalf("expected results in budget test")
		}
		t.Logf("Budget test passed in %v (well within 800ms budget, returned %d results)", elapsed, len(results))
	})
}
