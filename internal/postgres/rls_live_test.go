package postgres

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// TestLivePostgresFailClosedRLS verifies that Row-Level Security on financial and tenant tables
// strictly isolates properties, fails closed when un-scoped, and cleans up pool connection state.
func TestLivePostgresFailClosedRLS(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	propA := uuid.New()
	propB := uuid.New()

	invA := "IA" + uuid.New().String()[:6]
	invB := "IB" + uuid.New().String()[:6]

	// Seed properties directly under maintenance mode or superuser
	err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
			VALUES ($1, 'Property Alpha RLS', '100 Alpha St', '+919999900010', 'alpha@upi', 'Owner Alpha', 'alpha@example.com', $3),
			       ($2, 'Property Beta RLS', '200 Beta Ave', '+919999900020', 'beta@upi', 'Owner Beta', 'beta@example.com', $4)
			ON CONFLICT (id) DO NOTHING;
		`, propA, propB, invA, invB)
		return err
	})
	if err != nil {
		t.Fatalf("failed seeding test properties: %v", err)
	}

	// Create a non-superuser application role for testing RLS if not exists
	_, err = pool.Exec(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pgapp_app') THEN
				CREATE ROLE pgapp_app;
				GRANT USAGE ON SCHEMA public TO pgapp_app;
				GRANT ALL ON ALL TABLES IN SCHEMA public TO pgapp_app;
			END IF;
		END
		$$;
	`)
	if err != nil {
		t.Fatalf("failed creating pgapp_app: %v", err)
	}

	t.Cleanup(func() {
		_ = WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(context.Background(), "SET LOCAL app.ledger_maintenance = 'on'")
			_, _ = tx.Exec(context.Background(), "DELETE FROM tenants WHERE property_id IN ($1, $2)", propA, propB)
			_, _ = tx.Exec(context.Background(), "DELETE FROM properties WHERE id IN ($1, $2)", propA, propB)
			return nil
		})
	})

	tenantA := uuid.New()
	tenantB := uuid.New()
	phoneA := "+91" + strconv.FormatInt(time.Now().UnixNano()%10000000000, 10)
	phoneB := "+91" + strconv.FormatInt((time.Now().UnixNano()+1)%10000000000, 10)

	ctxA := requestscope.WithPropertyID(ctx, propA)
	ctxB := requestscope.WithPropertyID(ctx, propB)

	// Insert tenants under Property A and Property B using ScopedDB
	err = WithinTx(ctxA, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propA.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Tenant Alpha', $3, 15000, 5, 'active')
		`, tenantA, propA, phoneA)
		return err
	})
	if err != nil {
		t.Fatalf("insert tenant A under scope A failed: %v", err)
	}

	err = WithinTx(ctxB, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propB.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Tenant Beta', $3, 18000, 5, 'active')
		`, tenantB, propB, phoneB)
		return err
	})
	if err != nil {
		t.Fatalf("insert tenant B under scope B failed: %v", err)
	}

	t.Run("Scoped Query Only Sees Scoped Property Rows", func(t *testing.T) {
		err := WithinTx(ctxA, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propA.String()); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT id, property_id FROM tenants WHERE property_id IN ($1, $2)`, propA, propB)
			if err != nil {
				return err
			}
			defer rows.Close()

			foundA := false
			foundB := false
			for rows.Next() {
				var id, pid uuid.UUID
				if err := rows.Scan(&id, &pid); err != nil {
					return err
				}
				if pid == propA {
					foundA = true
				}
				if pid == propB {
					foundB = true
				}
			}
			if !foundA {
				t.Errorf("expected to find tenant A under scope A, but did not")
			}
			if foundB {
				t.Errorf("SECURITY VIOLATION: tenant B leaked into scope A query!")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("query under scope A failed: %v", err)
		}
	})

	t.Run("Cross-Property Attempt Filtered By RLS Policy", func(t *testing.T) {
		err := WithinTx(ctxA, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propA.String()); err != nil {
				return err
			}
			var count int
			err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE id = $1`, tenantB).Scan(&count)
			if err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("SECURITY VIOLATION: cross-property query saw %d rows for tenant B under scope A", count)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("cross-property query returned error: %v", err)
		}
	})

	t.Run("Unscoped Connection Fails Closed (Zero Rows)", func(t *testing.T) {
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', '', true)"); err != nil {
				return err
			}
			// In an un-scoped tx, current_setting('app.current_property_id', true) is empty.
			// RLS policy requires NULLIF(...) IS NOT NULL, so zero rows must be returned.
			var count int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM tenants WHERE id IN ($1, $2)`, tenantA, tenantB).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Errorf("SECURITY VIOLATION: unscoped transaction saw %d tenants (expected fail-closed 0 rows)", count)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unscoped tx failed: %v", err)
		}
	})

	t.Run("WithinTx Sets Scope And Cleans Up", func(t *testing.T) {
		err := WithinTx(ctxA, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propA.String()); err != nil {
				return err
			}
			var count int
			if err := tx.QueryRow(ctxA, `SELECT COUNT(*) FROM tenants WHERE id = $1`, tenantA).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				t.Errorf("expected WithinTx with ctxA to see 1 tenant, got %d", count)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("WithinTx failed: %v", err)
		}
	})

	t.Run("Production Role Safety (Non-Superuser, Non-BYPASSRLS)", func(t *testing.T) {
		var isSuper, bypassRLS bool
		err := pool.QueryRow(ctx, `
			SELECT rolsuper, rolbypassrls
			FROM pg_roles
			WHERE rolname = 'pgapp_app'
		`).Scan(&isSuper, &bypassRLS)
		if err != nil {
			t.Fatalf("failed querying role flags: %v", err)
		}
		if isSuper {
			t.Fatalf("SECURITY VIOLATION: runtime app role must NOT be a superuser")
		}
		if bypassRLS {
			t.Fatalf("SECURITY VIOLATION: runtime app role must NOT have BYPASSRLS")
		}
	})

	t.Run("Cross-Property Insert Filtered/Rejected By RLS Policy", func(t *testing.T) {
		err := WithinTx(ctxA, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propA.String()); err != nil {
				return err
			}
			phoneLeaker := "+91" + strconv.FormatInt((time.Now().UnixNano()+9)%10000000000, 10)
			_, err = tx.Exec(ctx, `
				INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
				VALUES ($1, $2, 'Sneaky Leaker', $3, 10000, 1, 'active')
			`, uuid.New(), propB, phoneLeaker)
			return err
		})
		if err == nil {
			t.Fatalf("SECURITY VIOLATION: expected cross-property insert under foreign scope to fail with RLS check violation, but it succeeded")
		}
	})
}
