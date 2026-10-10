package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

func TestScopedDB_BatchedPipelining(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 15*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	scoped := NewScopedDB(pool)
	testPropID := uuid.New()
	scopedCtx := requestscope.WithPropertyID(ctx, testPropID)

	t.Run("QueryRow under scope verifies transaction-local setting", func(t *testing.T) {
		var gucValue string
		err := scoped.QueryRow(scopedCtx, "SELECT current_setting('app.current_property_id', true)").Scan(&gucValue)
		if err != nil {
			t.Fatalf("QueryRow failed: %v", err)
		}
		if gucValue != testPropID.String() {
			t.Fatalf("expected GUC %s, got %s", testPropID.String(), gucValue)
		}

		// Verify no leak outside transaction
		var unscopedGuc string
		err = pool.QueryRow(ctx, "SELECT COALESCE(current_setting('app.current_property_id', true), '')").Scan(&unscopedGuc)
		if err != nil {
			t.Fatalf("unscoped check failed: %v", err)
		}
		if unscopedGuc != "" {
			t.Fatalf("GUC leaked to pool connection: %s", unscopedGuc)
		}
	})

	t.Run("Exec under scope executes and cleans up", func(t *testing.T) {
		_, err := scoped.Exec(scopedCtx, "SELECT 1")
		if err != nil {
			t.Fatalf("Exec failed: %v", err)
		}

		var unscopedGuc string
		err = pool.QueryRow(ctx, "SELECT COALESCE(current_setting('app.current_property_id', true), '')").Scan(&unscopedGuc)
		if err != nil {
			t.Fatalf("unscoped check failed: %v", err)
		}
		if unscopedGuc != "" {
			t.Fatalf("GUC leaked to pool connection: %s", unscopedGuc)
		}
	})

	t.Run("Query under scope returns rows and cleans up on close", func(t *testing.T) {
		rows, err := scoped.Query(scopedCtx, "SELECT current_setting('app.current_property_id', true)")
		if err != nil {
			t.Fatalf("Query failed: %v", err)
		}
		var gucValue string
		if rows.Next() {
			if err := rows.Scan(&gucValue); err != nil {
				t.Fatalf("Scan failed: %v", err)
			}
		}
		rows.Close()
		if gucValue != testPropID.String() {
			t.Fatalf("expected GUC %s, got %s", testPropID.String(), gucValue)
		}

		var unscopedGuc string
		err = pool.QueryRow(ctx, "SELECT COALESCE(current_setting('app.current_property_id', true), '')").Scan(&unscopedGuc)
		if err != nil {
			t.Fatalf("unscoped check failed: %v", err)
		}
		if unscopedGuc != "" {
			t.Fatalf("GUC leaked to pool connection: %s", unscopedGuc)
		}
	})

	t.Run("Connection reuse across repeated scoped queries", func(t *testing.T) {
		var dummy int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&dummy); err != nil {
			t.Fatalf("warmup failed: %v", err)
		}

		initialNewConns := pool.Stat().NewConnsCount()

		const iterations = 200
		for i := 0; i < iterations; i++ {
			rows, err := scoped.Query(scopedCtx, "SELECT 1")
			if err != nil {
				t.Fatalf("Query failed at iteration %d: %v", i, err)
			}
			for rows.Next() {
				var val int
				if err := rows.Scan(&val); err != nil {
					t.Fatalf("Scan failed at iteration %d: %v", i, err)
				}
			}
			rows.Close()

			var qrVal int
			if err := scoped.QueryRow(scopedCtx, "SELECT 2").Scan(&qrVal); err != nil {
				t.Fatalf("QueryRow failed at iteration %d: %v", i, err)
			}
		}

		connsCreated := pool.Stat().NewConnsCount() - initialNewConns
		if connsCreated > 2 {
			t.Fatalf("pool connections leaked/destroyed: %d new connections created during %d scoped calls (expected connection reuse)", connsCreated, iterations)
		}
	})

	t.Run("QueryRow INSERT RETURNING commits and is visible across connections", func(t *testing.T) {
		tableName := "test_scoped_queryrow_write_visibility"
		_, err := pool.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS `+tableName+` (
				id UUID PRIMARY KEY,
				val TEXT NOT NULL,
				property_id UUID NOT NULL
			)
		`)
		if err != nil {
			t.Fatalf("failed to create test table: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tableName)
		})

		writeID := uuid.New()
		var returnedID uuid.UUID
		err = scoped.QueryRow(scopedCtx,
			"INSERT INTO "+tableName+" (id, val, property_id) VALUES ($1, $2, $3) RETURNING id",
			writeID, "visible-data", testPropID,
		).Scan(&returnedID)
		if err != nil {
			t.Fatalf("scoped QueryRow INSERT RETURNING failed: %v", err)
		}
		if returnedID != writeID {
			t.Fatalf("expected returned ID %s, got %s", writeID, returnedID)
		}

		var readVal string
		err = pool.QueryRow(ctx, "SELECT val FROM "+tableName+" WHERE id = $1", writeID).Scan(&readVal)
		if err != nil {
			t.Fatalf("row inserted via QueryRow was not visible to subsequent queries (silent rollback bug): %v", err)
		}
		if readVal != "visible-data" {
			t.Fatalf("expected 'visible-data', got %q", readVal)
		}
	})
}
