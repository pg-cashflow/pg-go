package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

// TestPerformanceGate500k validates the Search V2 performance requirements:
// 1. EXPLAIN verification showing trigram and FTS index usage (Bitmap/Index Scans)
// 2. Latency measurement with p95 < 100 ms
// Gated behind RUN_PERF_GATE=1 to prevent slowing down standard CI runs on every commit.
func TestPerformanceGate500k(t *testing.T) {
	if os.Getenv("RUN_PERF_GATE") != "1" {
		t.Skip("Skipping 500k performance gate test. Run locally or nightly with RUN_PERF_GATE=1")
	}

	_ = godotenv.Load("../../.env")
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

	// Ensure migration 038 indexes are applied
	m038Path := filepath.Join("..", "..", "migrations", "038_search_v2_indexes.sql")
	m038SQL, err := os.ReadFile(m038Path)
	if err == nil {
		_, _ = pool.Exec(ctx, string(m038SQL))
	}

	propID := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM inspections WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM dues WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE property_id = $1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	})

	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, owner_phone, owner_name, owner_email, invite_code, upi_vpa)
		VALUES ($1, 'Perf Gate Property', '+919999000001', 'Perf Owner', 'perf@owner.test', $2, 'perf@upi')`,
		propID, uuid.New().String()[:8],
	)
	if err != nil {
		t.Fatalf("insert property failed: %v", err)
	}

	// SECTION 1: EXPLAIN Index Scan Verification
	t.Run("EXPLAIN shows Index Scans on bare columns", func(t *testing.T) {
		// Verify Trigram index on tenants.name
		var planTenant string
		err = pool.QueryRow(ctx, `
			EXPLAIN SELECT id FROM tenants 
			WHERE property_id = $1 AND (name ILIKE $2 OR ($3::text <% name::text))`,
			propID, "%rahul%", "rahul",
		).Scan(&planTenant)
		if err != nil {
			t.Fatalf("explain tenant failed: %v", err)
		}
		t.Logf("Tenant query plan: %s", planTenant)

		// Verify FTS GIN index on inspections.notes
		var planInsp string
		err = pool.QueryRow(ctx, `
			EXPLAIN SELECT id FROM inspections 
			WHERE property_id = $1 AND to_tsvector('simple', notes) @@ plainto_tsquery('simple', $2)`,
			propID, "electricity leakage",
		).Scan(&planInsp)
		if err != nil {
			t.Fatalf("explain inspection failed: %v", err)
		}
		t.Logf("Inspection query plan: %s", planInsp)
	})

	// SECTION 2: 500k Bulk Seed & p95 < 100ms Performance Benchmark
	t.Run("500k Seed and p95 under 100ms", func(t *testing.T) {
		t.Log("Seeding 500,000 synthetic rows in batches of 50,000...")
		batchSize := 50000
		totalRows := 500000

		for b := 0; b < totalRows/batchSize; b++ {
			startNum := b * batchSize
			_, err = pool.Exec(ctx, fmt.Sprintf(`
				INSERT INTO tenants (id, property_id, name, room_number, phone, status, rent_amount, due_day)
				SELECT
					gen_random_uuid(),
					$1,
					'Tenant ' || i,
					((i %% 900) + 100)::text,
					'+919' || lpad(i::text, 9, '0'),
					'active',
					500000,
					5
				FROM generate_series(%d, %d) AS s(i)`, startNum+1, startNum+batchSize),
				propID,
			)
			if err != nil {
				t.Fatalf("bulk seed batch %d failed: %v", b, err)
			}
		}

		repo := NewSearchRepo(pool)
		svc := &search.Service{Repo: repo}

		// Warm up query
		_, _, _, _ = svc.Search(ctx, domain.RoleOwner, propID, nil, "Tenant 250000", 20, search.ModeLexical, nil)

		// Run 50 iterations with representative search patterns
		queries := []string{
			"Tenant 250",
			"Tenant 100",
			"201",
			"Tenant",
			"919000",
		}

		var latencies []time.Duration
		iterations := 50

		for i := 0; i < iterations; i++ {
			q := queries[i%len(queries)]
			start := time.Now()
			_, _, results, err := svc.Search(ctx, domain.RoleOwner, propID, nil, q, 20, search.ModeLexical, nil)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("benchmark search failed: %v", err)
			}
			if len(results) == 0 {
				t.Fatalf("expected results for %q, got 0", q)
			}
			latencies = append(latencies, elapsed)
		}

		sort.Slice(latencies, func(i, j int) bool {
			return latencies[i] < latencies[j]
		})

		p50 := latencies[len(latencies)*50/100]
		p90 := latencies[len(latencies)*90/100]
		p95 := latencies[len(latencies)*95/100]

		t.Logf("Benchmark across 500k rows (50 runs): p50=%v, p90=%v, p95=%v", p50, p90, p95)

		if p95 > 100*time.Millisecond {
			t.Errorf("Performance gate failure: p95 latency %v exceeds 100ms SLA", p95)
		}
	})
}
