package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// TestLiveRoleGrantsAndImmutability (Ticket R6, Track R, ADR-030)
// Verifies live database security boundaries across pgapp_app, pgapp_maint, and pgapp_worker:
// 1. All three roles exist, have rolsuper=false, and rolbypassrls=false.
// 2. All three roles are denied access to schema_migrations.
// 3. All three roles are denied UPDATE and DELETE on immutable financial ledger tables:
//    - financial_journal_entries
//    - capital_transactions
//    - expense_payments
//    - payout_records
//    - payout_record_items
// 4. pgapp_worker can perform valid writes across all 9 job families.
func TestLiveRoleGrantsAndImmutability(t *testing.T) {
	testutil.RequireDB(t)
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Ensure roles exist in local test postgres if running in ephemeral test container
	_, err := pool.Exec(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_app') THEN
				CREATE ROLE pgapp_app WITH NOSUPERUSER NOBYPASSRLS;
			END IF;
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_maint') THEN
				CREATE ROLE pgapp_maint WITH NOSUPERUSER NOBYPASSRLS;
			END IF;
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_worker') THEN
				CREATE ROLE pgapp_worker WITH NOSUPERUSER NOBYPASSRLS;
			END IF;
			BEGIN
				EXECUTE format('GRANT pgapp_app, pgapp_maint, pgapp_worker TO %I', CURRENT_USER);
			EXCEPTION WHEN OTHERS THEN NULL;
			END;
		END $$;
	`)
	if err != nil {
		t.Fatalf("setup test roles failed: %v", err)
	}

	roles := []string{"pgapp_app", "pgapp_maint", "pgapp_worker"}

	t.Run("Subtest 1: All roles exist and enforce NOBYPASSRLS and NOSUPERUSER", func(t *testing.T) {
		for _, role := range roles {
			var rolsuper, rolbypassrls bool
			err := pool.QueryRow(ctx, `
				SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1
			`, role).Scan(&rolsuper, &rolbypassrls)
			if err != nil {
				t.Fatalf("role %s missing from pg_roles: %v", role, err)
			}
			if rolsuper {
				t.Errorf("SECURITY DEFECT: role %s has rolsuper=true", role)
			}
			if rolbypassrls {
				t.Errorf("SECURITY DEFECT: role %s has rolbypassrls=true", role)
			}
		}
	})

	t.Run("Subtest 2: All three roles denied access to schema_migrations", func(t *testing.T) {
		for _, role := range roles {
			t.Run(role, func(t *testing.T) {
				err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
						return err
					}
					var count int
					return tx.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count)
				})
				if err == nil {
					t.Fatalf("expected role %s to be denied SELECT on schema_migrations, but query succeeded", role)
				}
				if !strings.Contains(err.Error(), "permission denied") {
					t.Fatalf("expected permission denied error for %s on schema_migrations, got: %v", role, err)
				}
			})
		}
	})

	t.Run("Subtest 3: All three roles denied UPDATE and DELETE on immutable financial tables", func(t *testing.T) {
		immutableTables := []string{
			"financial_journal_entries",
			"capital_transactions",
			"expense_payments",
			"payout_records",
			"payout_record_items",
		}

		for _, role := range roles {
			for _, table := range immutableTables {
				t.Run(role+"_UPDATE_"+table, func(t *testing.T) {
					err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
						if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
							return err
						}
						_, err := tx.Exec(ctx, "UPDATE "+table+" SET id = id WHERE false")
						return err
					})
					if err == nil {
						t.Fatalf("expected role %s to be denied UPDATE on %s, but succeeded", role, table)
					}
					if !strings.Contains(err.Error(), "permission denied") {
						t.Fatalf("expected permission denied on UPDATE %s for %s, got: %v", table, role, err)
					}
				})

				t.Run(role+"_DELETE_"+table, func(t *testing.T) {
					err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
						if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
							return err
						}
						_, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE false")
						return err
					})
					if err == nil {
						t.Fatalf("expected role %s to be denied DELETE on %s, but succeeded", role, table)
					}
					if !strings.Contains(err.Error(), "permission denied") {
						t.Fatalf("expected permission denied on DELETE %s for %s, got: %v", table, role, err)
					}
				})
			}
		}
	})

	t.Run("Subtest 4: pgapp_worker write capability across all 9 job families", func(t *testing.T) {
		propID := uuid.New()
		tenantID := uuid.New()
		dueID := uuid.New()
		paymentID := uuid.New()

		// Seed base property and tenant under maintenance mode
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'")
			_, err := tx.Exec(ctx, `
				INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
				VALUES ($1, 'Worker Test Prop', '1 Worker Way', '+919888800001', 'worker@upi', 'Owner W', 'worker@test.com', $2)
				ON CONFLICT (id) DO NOTHING;
			`, propID, "WRK"+uuid.New().String()[:5])
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
				VALUES ($1, $2, 'Worker Tenant', $3, 1000000, 1, 'active');
			`, tenantID, propID, "+91"+uuid.New().String()[:10])
			return err
		})
		if err != nil {
			t.Fatalf("failed seeding property and tenant for worker test: %v", err)
		}

		t.Cleanup(func() {
			_ = WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
				_, _ = tx.Exec(context.Background(), "SET LOCAL app.ledger_maintenance = 'on'")
				_, _ = tx.Exec(context.Background(), "DELETE FROM payments WHERE property_id = $1", propID)
				_, _ = tx.Exec(context.Background(), "DELETE FROM dues WHERE property_id = $1", propID)
				_, _ = tx.Exec(context.Background(), "DELETE FROM points_ledger WHERE property_id = $1", propID)
				_, _ = tx.Exec(context.Background(), "DELETE FROM daily_financial_rollups WHERE property_id = $1", propID)
				_, _ = tx.Exec(context.Background(), "DELETE FROM kpi_snapshots WHERE property_id = $1", propID)
				_, _ = tx.Exec(context.Background(), "DELETE FROM notifications WHERE property_id = $1", propID)
				_, _ = tx.Exec(context.Background(), "DELETE FROM tenants WHERE property_id = $1", propID)
				_, _ = tx.Exec(context.Background(), "DELETE FROM properties WHERE id = $1", propID)
				return nil
			})
		})

		// Job 1: Billing cycle (due insert)
		t.Run("Job 1: billing-cycle due insert", func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
					VALUES ($1, $2, $3, $4, 'rent', 1000000, 1000000, CURRENT_DATE, CURRENT_DATE + 30, CURRENT_DATE + 5, 'pending')
				`, dueID, "DUE"+uuid.New().String()[:5], tenantID, propID)
				return err
			})
			if err != nil {
				t.Fatalf("worker billing-cycle write failed: %v", err)
			}
		})

		// Job 2: Cashfree poll (payment insert / status update)
		t.Run("Job 2: cashfree-poll payment insert", func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO payments (id, due_id, tenant_id, property_id, amount, payment_method, status)
					VALUES ($1, $2, $3, $4, 1000000, 'cashfree', 'pending')
				`, paymentID, dueID, tenantID, propID)
				return err
			})
			if err != nil {
				t.Fatalf("worker cashfree-poll write failed: %v", err)
			}
		})

		// Job 3: Daily rollup (daily_financial_rollups upsert)
		t.Run("Job 3: daily-rollup upsert", func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO daily_financial_rollups (id, property_id, rollup_date, total_collected_paise, total_due_paise, total_expenses_paise)
					VALUES ($1, $2, CURRENT_DATE, 1000000, 1000000, 0)
					ON CONFLICT (property_id, rollup_date) DO UPDATE SET total_collected_paise = EXCLUDED.total_collected_paise;
				`, uuid.New(), propID)
				return err
			})
			if err != nil {
				t.Fatalf("worker daily-rollup write failed: %v", err)
			}
		})

		// Job 4: DigiLocker reconcile (kyc_consent & verification insert)
		t.Run("Job 4: digilocker-reconcile kyc write", func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
					return err
				}
				consentID := uuid.New()
				_, err := tx.Exec(ctx, `
					INSERT INTO kyc_consent (id, tenant_id, purpose, consent_text, consent_text_hash)
					VALUES ($1, $2, 'tenant_verification', 'consent text', '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef')
				`, consentID, tenantID)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `
					INSERT INTO kyc_verification (id, tenant_id, consent_id, vendor_reference_id, method, status)
					VALUES ($1, $2, $3, $4, 'digilocker', 'pending')
				`, uuid.New(), tenantID, consentID, "VR"+uuid.New().String()[:8])
				return err
			})
			if err != nil {
				t.Fatalf("worker digilocker-reconcile write failed: %v", err)
			}
		})

		// Job 5: Financial summary (events write)
		t.Run("Job 5: financial-summary event insert", func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO events (id, property_id, event_type, payload)
					VALUES ($1, $2, 'financial.daily_summary.generated', '{"summary": "ok"}')
				`, uuid.New(), propID)
				return err
			})
			if err != nil {
				t.Fatalf("worker financial-summary event insert failed: %v", err)
			}
		})

		// Job 6: Gamification cycle (points_ledger insert)
		t.Run("Job 6: gamification-cycle points insert", func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO points_ledger (tenant_id, property_id, delta, reason, category)
					VALUES ($1, $2, 50, 'on_time_payment', 'payment')
				`, tenantID, propID)
				return err
			})
			if err != nil {
				t.Fatalf("worker gamification-cycle write failed: %v", err)
			}
		})

		// Job 7: KPI snapshot (kpi_snapshots insert)
		t.Run("Job 7: kpi-snapshot insert", func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO kpi_snapshots (id, property_id, snapshot_date, total_tenants, occupied_beds)
					VALUES ($1, $2, CURRENT_DATE, 1, 1)
				`, uuid.New(), propID)
				return err
			})
			if err != nil {
				t.Fatalf("worker kpi-snapshot write failed: %v", err)
			}
		})

		// Job 8: KYC expiry (tenants status update)
		t.Run("Job 8: kyc-expiry tenant status update", func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					UPDATE tenants SET updated_at = NOW() WHERE id = $1
				`, tenantID)
				return err
			})
			if err != nil {
				t.Fatalf("worker kyc-expiry tenant update failed: %v", err)
			}
		})

		// Job 9: Reminder (notifications insert)
		t.Run("Job 9: reminder notification insert", func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `
					INSERT INTO notifications (id, property_id, user_id, title, body, channel)
					VALUES ($1, $2, $3, 'Rent Due Reminder', 'Please pay your rent', 'push')
				`, uuid.New(), propID, tenantID)
				return err
			})
			if err != nil {
				t.Fatalf("worker reminder notification insert failed: %v", err)
			}
		})
	})
}
