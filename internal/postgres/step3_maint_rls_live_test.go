package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// TestStep3_RestrictedMaintenanceRLS verifies:
// 1. pgapp_maint and pgapp_worker roles exist and have NOBYPASSRLS.
// 2. Financial tables fail closed under pgapp_maint when app.current_property_id is unset.
// 3. Operational tables enforce property isolation under pgapp_maint when app.current_property_id is set.
// 4. WithinTx switches role to pgapp_app when property scope is present in context.
func TestStep3_RestrictedMaintenanceRLS(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	propA := uuid.New()
	propB := uuid.New()

	// Seed test properties
	err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
			VALUES ($1, 'Step 3 Prop Alpha', '1 Alpha St', '+919777700010', 's3alpha@upi', 'Owner A', 's3a@test.com', $3),
			       ($2, 'Step 3 Prop Beta', '2 Beta Ave', '+919777700020', 's3beta@upi', 'Owner B', 's3b@test.com', $4)
			ON CONFLICT (id) DO NOTHING;
		`, propA, propB, "S3A"+uuid.New().String()[:5], "S3B"+uuid.New().String()[:5])
		return err
	})
	if err != nil {
		t.Fatalf("failed seeding test properties: %v", err)
	}

	t.Cleanup(func() {
		_ = WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(context.Background(), "DELETE FROM financial_journal_entries WHERE property_id IN ($1, $2)", propA, propB)
			_, _ = tx.Exec(context.Background(), "DELETE FROM dues WHERE property_id IN ($1, $2)", propA, propB)
			_, _ = tx.Exec(context.Background(), "DELETE FROM tenants WHERE property_id IN ($1, $2)", propA, propB)
			_, _ = tx.Exec(context.Background(), "DELETE FROM properties WHERE id IN ($1, $2)", propA, propB)
			return nil
		})
	})

	tenantA := uuid.New()
	tenantB := uuid.New()
	dueA := uuid.New()
	dueB := uuid.New()
	phoneA := "+91" + uuid.New().String()[:10]
	phoneB := "+91" + uuid.New().String()[:10]

	// Seed tenants and dues
	err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err = tx.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Tenant A', $5, 1000000, 5, 'active'),
			       ($3, $4, 'Tenant B', $6, 2000000, 10, 'active')
		`, tenantA, propA, tenantB, propB, phoneA, phoneB)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
			VALUES ($1, $7, $2, $3, 'rent', 1000000, 1000000, CURRENT_DATE, CURRENT_DATE + 30, CURRENT_DATE + 5, 'pending'),
			       ($4, $8, $5, $6, 'rent', 2000000, 2000000, CURRENT_DATE, CURRENT_DATE + 30, CURRENT_DATE + 10, 'pending')
		`, dueA, tenantA, propA, dueB, tenantB, propB, "D3A"+uuid.New().String()[:4], "D3B"+uuid.New().String()[:4])
		return err
	})
	if err != nil {
		t.Fatalf("failed seeding tenants and dues: %v", err)
	}

	t.Run("Subtest 1: pgapp_worker exists with NOBYPASSRLS", func(t *testing.T) {
		var rolsuper, rolbypassrls bool
		err := pool.QueryRow(ctx, `
			SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = 'pgapp_worker'
		`).Scan(&rolsuper, &rolbypassrls)
		if err != nil {
			t.Fatalf("failed querying pg_roles for pgapp_worker: %v", err)
		}
		if rolsuper || rolbypassrls {
			t.Fatalf("pgapp_worker must have rolsuper=false and rolbypassrls=false, got super=%v bypass=%v", rolsuper, rolbypassrls)
		}
	})

	t.Run("Subtest 2: Financial table fails closed under pgapp_maint when app.current_property_id is unset", func(t *testing.T) {
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_maint"); err != nil {
				return err
			}
			// app.current_property_id is NOT set
			var count int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM financial_journal_entries WHERE property_id = $1", propA).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("expected 0 rows from financial_journal_entries when GUC is unset, got %d", count)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("subtest failed: %v", err)
		}
	})

	t.Run("Subtest 3: Operational table enforces scope under pgapp_maint when app.current_property_id is set", func(t *testing.T) {
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_maint"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propA.String()); err != nil {
				return err
			}

			// Under propA scope, reading propA due returns 1
			var countA int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM dues WHERE id = $1", dueA).Scan(&countA); err != nil {
				return err
			}
			if countA != 1 {
				t.Fatalf("expected 1 due for property A, got %d", countA)
			}

			// Reading propB due under propA scope must return 0
			var countB int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM dues WHERE id = $1", dueB).Scan(&countB); err != nil {
				return err
			}
			if countB != 0 {
				t.Fatalf("CROSS-PROPERTY LEAK: pgapp_maint under propA scope saw due from propB!")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("subtest failed: %v", err)
		}
	})

	t.Run("Subtest 4: WithinTx automatically switches role to pgapp_app when property scope is present", func(t *testing.T) {
		scopedCtx := requestscope.WithPropertyID(ctx, propA)
		err := WithinTx(scopedCtx, pool, func(tx pgx.Tx) error {
			var currentRole string
			if err := tx.QueryRow(scopedCtx, "SELECT current_user").Scan(&currentRole); err != nil {
				return err
			}
			if currentRole != "pgapp_app" {
				t.Fatalf("expected current_user to be pgapp_app within scoped WithinTx, got %s", currentRole)
			}

			// Reading propA due under propA scope succeeds
			var countA int
			if err := tx.QueryRow(scopedCtx, "SELECT count(*) FROM dues WHERE id = $1", dueA).Scan(&countA); err != nil {
				return err
			}
			if countA != 1 {
				t.Fatalf("expected 1 due for property A, got %d", countA)
			}

			// Reading propB due under propA scope must return 0
			var countB int
			if err := tx.QueryRow(scopedCtx, "SELECT count(*) FROM dues WHERE id = $1", dueB).Scan(&countB); err != nil {
				return err
			}
			if countB != 0 {
				t.Fatalf("CROSS-PROPERTY LEAK: pgapp_app under propA scope saw due from propB!")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scoped WithinTx read verification failed: %v", err)
		}

		// Verify cross-property write is blocked by pgapp_app RLS
		err = WithinTx(scopedCtx, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(scopedCtx, `
				INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
				VALUES ($1, 'ILL_S3', $2, $3, 'rent', 500000, 500000, CURRENT_DATE, CURRENT_DATE + 30, CURRENT_DATE + 5, 'pending')
			`, uuid.New(), tenantB, propB)
			return err
		})
		if err == nil {
			t.Fatalf("expected cross-property INSERT to fail under RLS, but it succeeded")
		}
		if !strings.Contains(err.Error(), "violates row-level security policy") {
			t.Fatalf("expected RLS policy violation error, got: %v", err)
		}
	})
}
