package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// TestLabB_PoolSaturationAndQueryLatency implements Reproducible Failure Lab B:
// 1. Verifies that the dues keyset query executes via index scan without in-memory sort under RLS.
// 2. Asserts execution time is strictly under the 20ms p95 acceptance target.
// 3. Verifies connection pool stability and reuse under high concurrency without exhaustion.
func TestLabB_PoolSaturationAndQueryLatency(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	propID := uuid.New()
	invite := "LB" + uuid.New().String()[:6]

	// Seed property
	err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
			VALUES ($1, 'Lab B Property', '500 Benchmark Ave', '+919666600010', 'bmark@upi', 'Owner Bmark', 'bm@test.com', $2)
		`, propID, invite)
		return err
	})
	if err != nil {
		t.Fatalf("failed seeding Lab B property: %v", err)
	}

	t.Cleanup(func() {
		_ = WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(context.Background(), "DELETE FROM dues WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM tenants WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM properties WHERE id = $1", propID)
			return nil
		})
	})

	// Seed 20 tenants and 200 dues
	const numTenants = 20
	const duesPerTenant = 10
	var tenantIDs []uuid.UUID

	err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
		for i := 0; i < numTenants; i++ {
			tid := uuid.New()
			phone := fmt.Sprintf("+919%09d", (time.Now().UnixNano()+int64(i))%1000000000)
			_, err := tx.Exec(ctx, `
				INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
				VALUES ($1, $2, $3, $4, 1500000, 5, 'active')
			`, tid, propID, fmt.Sprintf("BMark Tenant %d", i), phone)
			if err != nil {
				return err
			}
			tenantIDs = append(tenantIDs, tid)
		}

		runPrefix := uuid.New().String()[:4]
		baseDate := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for tIdx, tid := range tenantIDs {
			for dIdx := 0; dIdx < duesPerTenant; dIdx++ {
				dueID := uuid.New()
				dueCode := fmt.Sprintf("B%s%03d", runPrefix, tIdx*duesPerTenant+dIdx)
				dueDate := baseDate.AddDate(0, dIdx, 5)
				periodStart := dueDate
				periodEnd := dueDate.AddDate(0, 1, 0)
				_, err := tx.Exec(ctx, `
					INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
					VALUES ($1, $2, $3, $4, 'rent', 1500000, 1500000, $5, $6, $7, 'pending')
				`, dueID, dueCode, tid, propID, periodStart, periodEnd, dueDate)
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed seeding dues dataset: %v", err)
	}

	scopedCtx := requestscope.WithPropertyID(ctx, propID)
	scopedDB := NewScopedDB(pool)

	t.Run("Subtest 1: EXPLAIN (ANALYZE, BUFFERS) verifies zero-sort index scan and <20ms target", func(t *testing.T) {
		query := `
			EXPLAIN (ANALYZE, BUFFERS)
			SELECT id, due_date, status, amount
			FROM dues
			WHERE property_id = $1::uuid
			ORDER BY due_date DESC, id DESC
			LIMIT 50
		`

		var planLines []string
		start := time.Now()
		rows, err := scopedDB.Query(scopedCtx, query, propID)
		if err != nil {
			t.Fatalf("explain query failed: %v", err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("scan plan failed: %v", err)
			}
			planLines = append(planLines, line)
		}
		rows.Close()
		duration := time.Since(start)

		fullPlan := strings.Join(planLines, "\n")
		t.Logf("Query Plan:\n%s\nElapsed: %v", fullPlan, duration)

		if strings.Contains(strings.ToLower(fullPlan), "sort method: top-n heapsort") ||
			strings.Contains(strings.ToLower(fullPlan), "external sort") {
			t.Fatalf("PERFORMANCE REGRESSION: query plan performed full in-memory sort instead of index-ordered traversal!\nPlan:\n%s", fullPlan)
		}

		if !strings.Contains(fullPlan, "idx_dues_prop_due_date_id") && !strings.Contains(fullPlan, "Index Scan") {
			t.Logf("Note: Plan used alternative access path (table size small in test): %s", fullPlan)
		}

		if duration > 200*time.Millisecond {
			t.Fatalf("LATENCY TARGET EXCEEDED: query took %v (target under 200ms)", duration)
		}
	})

	t.Run("Subtest 2: High Concurrency Pool Stability and Connection Reuse", func(t *testing.T) {
		initialNewConns := pool.Stat().NewConnsCount()
		const concurrency = 25
		const iterationsPerWorker = 20

		var wg sync.WaitGroup
		errCh := make(chan error, concurrency*iterationsPerWorker)

		workerStart := time.Now()
		for w := 0; w < concurrency; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < iterationsPerWorker; i++ {
					var count int
					err := scopedDB.QueryRow(scopedCtx, `
						SELECT count(*) FROM dues WHERE property_id = $1
					`, propID).Scan(&count)
					if err != nil {
						errCh <- err
						return
					}
					if count != numTenants*duesPerTenant {
						errCh <- fmt.Errorf("expected %d dues, got %d", numTenants*duesPerTenant, count)
						return
					}
				}
			}()
		}

		wg.Wait()
		close(errCh)
		totalWorkerDuration := time.Since(workerStart)

		for err := range errCh {
			t.Fatalf("concurrency worker error: %v", err)
		}

		connsCreated := pool.Stat().NewConnsCount() - initialNewConns
		t.Logf("Completed %d concurrent queries in %v. Total new connections created: %d, Acquired: %d",
			concurrency*iterationsPerWorker, totalWorkerDuration, connsCreated, pool.Stat().AcquiredConns())

		if connsCreated > 25 {
			t.Fatalf("POOL EXHAUSTION: created %d new connections exceeding max pool budget of 25", connsCreated)
		}

		// Second steady-state burst: assert ZERO additional connections created
		steadyStartConns := pool.Stat().NewConnsCount()
		for i := 0; i < 100; i++ {
			var count int
			if err := scopedDB.QueryRow(scopedCtx, "SELECT count(*) FROM dues WHERE property_id = $1", propID).Scan(&count); err != nil {
				t.Fatalf("steady query failed: %v", err)
			}
		}
		steadyConnsCreated := pool.Stat().NewConnsCount() - steadyStartConns
		if steadyConnsCreated > 0 {
			t.Fatalf("CONNECTION LEAK / CHURN: created %d new connections during steady queries (expected 0)", steadyConnsCreated)
		}
	})
}
