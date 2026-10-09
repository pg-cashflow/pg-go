package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// TestLabA_RLSIsolationFailure implements Reproducible Failure Lab A:
// Proves that property boundaries hold under the real application role (pgapp_app),
// un-scoped queries fail closed, cross-property reads and writes are blocked,
// and the ordinary application role cannot bypass RLS by manipulating session settings.
func TestLabA_RLSIsolationFailure(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	propA := uuid.New()
	propB := uuid.New()

	// Seed properties as superuser/maintenance
	err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
			VALUES ($1, 'Lab A Prop Alpha', '1 Alpha Way', '+919888800010', 'alpha@upi', 'Owner A', 'a@test.com', $3),
			       ($2, 'Lab A Prop Beta', '2 Beta Way', '+919888800020', 'beta@upi', 'Owner B', 'b@test.com', $4)
			ON CONFLICT (id) DO NOTHING;
		`, propA, propB, "LA"+uuid.New().String()[:6], "LB"+uuid.New().String()[:6])
		return err
	})
	if err != nil {
		t.Fatalf("failed seeding test properties: %v", err)
	}

	t.Cleanup(func() {
		_ = WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
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

	// Seed initial tenants and dues using maintenance role or admin connection
	err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err = tx.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Tenant A', $5, 1500000, 5, 'active'),
			       ($3, $4, 'Tenant B', $6, 2000000, 10, 'active')
		`, tenantA, propA, tenantB, propB, phoneA, phoneB)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
			VALUES ($1, $7, $2, $3, 'rent', 1500000, 1500000, CURRENT_DATE, CURRENT_DATE + 30, CURRENT_DATE + 5, 'pending'),
			       ($4, $8, $5, $6, 'rent', 2000000, 2000000, CURRENT_DATE, CURRENT_DATE + 30, CURRENT_DATE + 10, 'pending')
		`, dueA, tenantA, propA, dueB, tenantB, propB, "D1"+uuid.New().String()[:4], "D2"+uuid.New().String()[:4])
		return err
	})
	if err != nil {
		t.Fatalf("failed seeding tenants and dues: %v", err)
	}

	t.Run("Subtest 1: Role Verification (pgapp_app has NOSUPERUSER and NOBYPASSRLS)", func(t *testing.T) {
		var rolsuper, rolbypassrls bool
		err := pool.QueryRow(ctx, `
			SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = 'pgapp_app'
		`).Scan(&rolsuper, &rolbypassrls)
		if err != nil {
			t.Fatalf("failed querying pg_roles for pgapp_app: %v", err)
		}
		if rolsuper || rolbypassrls {
			t.Fatalf("CRITICAL SECURITY VIOLATION: pgapp_app has rolsuper=%v, rolbypassrls=%v", rolsuper, rolbypassrls)
		}
	})

	t.Run("Subtest 2: Scoped Query under pgapp_app returns only scoped property", func(t *testing.T) {
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propA.String()); err != nil {
				return err
			}

			var count int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM dues WHERE property_id = $1", propA).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				t.Fatalf("expected 1 due for property A, got %d", count)
			}

			var crossCount int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM dues WHERE property_id = $1", propB).Scan(&crossCount); err != nil {
				return err
			}
			if crossCount != 0 {
				t.Fatalf("CROSS-PROPERTY LEAK: property A scope saw %d dues from property B", crossCount)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scoped query failed: %v", err)
		}
	})

	t.Run("Subtest 3: Un-scoped connection under pgapp_app fails closed (zero rows)", func(t *testing.T) {
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			// app.current_property_id is NOT set
			var count int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM dues WHERE id IN ($1, $2)", dueA, dueB).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("FAIL-CLOSED VIOLATION: unscoped query returned %d rows (expected 0)", count)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unscoped check failed: %v", err)
		}
	})

	t.Run("Subtest 4: Cross-property INSERT rejected under pgapp_app", func(t *testing.T) {
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propA.String()); err != nil {
				return err
			}

			if _, err := tx.Exec(ctx, "SAVEPOINT sp_illegal"); err != nil {
				return err
			}
			illegalDueID := uuid.New()
			_, err = tx.Exec(ctx, `
				INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
				VALUES ($1, 'ILL1', $2, $3, 'rent', 500000, 500000, CURRENT_DATE, CURRENT_DATE + 30, CURRENT_DATE + 5, 'pending')
			`, illegalDueID, tenantB, propB)
			if err == nil {
				t.Fatalf("CRITICAL SECURITY VIOLATION: cross-property INSERT into property B succeeded under property A scope")
			}
			_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sp_illegal")
			return nil
		})
		if err != nil {
			t.Fatalf("tx error: %v", err)
		}
	})

	t.Run("Subtest 5: Ordinary role cannot bypass RLS by setting app.ledger_maintenance", func(t *testing.T) {
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			// Attempt GUC manipulation bypass
			_, _ = tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'")

			var count int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM dues WHERE id IN ($1, $2)", dueA, dueB).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("CRITICAL AUTHORIZATION BYPASS: pgapp_app accessed %d dues after setting app.ledger_maintenance='on'!", count)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("GUC bypass test failed: %v", err)
		}
	})

	t.Run("Subtest 6: End-to-End ScopedDB Repositories Integration", func(t *testing.T) {
		scopedDB := NewScopedDB(pool)
		dueRepo := NewDueRepo(scopedDB)

		scopedCtxA := requestscope.WithPropertyID(ctx, propA)
		d, err := dueRepo.GetByID(scopedCtxA, dueA)
		if err != nil {
			t.Fatalf("failed fetching due A under scoped context: %v", err)
		}
		if d == nil || d.ID != dueA {
			t.Fatalf("expected due A %s, got %+v", dueA, d)
		}

		// Try fetching due B under scope A
		dB, err := dueRepo.GetByID(scopedCtxA, dueB)
		if err == nil && dB != nil {
			t.Fatalf("ScopedDB leaked Due B under Scope A: %+v", dB)
		}

		// Verify un-scoped read fails closed (zero rows/error)
		dUnscoped, err := dueRepo.GetByID(ctx, dueA)
		if err == nil && dUnscoped != nil {
			t.Fatalf("Un-scoped DueRepo query leaked Due A: %+v", dUnscoped)
		}
	})
}
