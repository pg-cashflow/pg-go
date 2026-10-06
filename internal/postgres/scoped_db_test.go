package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

func TestScopedDB_BatchedPipelining(t *testing.T) {
	_ = godotenv.Load("../../.env")
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres: %v", err)
	}
	defer pool.Close()

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
}
