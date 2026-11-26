package postgres

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestPaymentIntentParentPropertyRLS verifies that a multi-due parent is visible
// only to its owning property and that a child due from another property cannot
// be attached to it while using the real pgapp_app role.
func TestPaymentIntentParentPropertyRLS(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("apply migrations before payment-intent RLS regression: %v", err)
	}

	var roleExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_app')`).Scan(&roleExists); err != nil {
		t.Fatalf("check pgapp_app role: %v", err)
	}
	if !roleExists {
		t.Skip("pgapp_app role does not exist; migrations must be applied before the RLS regression test")
	}

	propertyA, propertyB := uuid.New(), uuid.New()
	tenantA, tenantB := uuid.New(), uuid.New()
	dueA1, dueA2, dueB := uuid.New(), uuid.New(), uuid.New()
	intentID := uuid.New()
	fixtureSuffix := uuid.NewString()[:8]
	inviteA := "PA" + fixtureSuffix
	inviteB := "PB" + fixtureSuffix
	phoneA := fmt.Sprintf("+919%09d", time.Now().UnixNano()%1_000_000_000)
	phoneB := fmt.Sprintf("+918%09d", (time.Now().UnixNano()+1)%1_000_000_000)
	ownerPhoneA := fmt.Sprintf("+917%09d", (time.Now().UnixNano()+2)%1_000_000_000)
	ownerPhoneB := fmt.Sprintf("+916%09d", (time.Now().UnixNano()+3)%1_000_000_000)
	vpaA := "a-" + fixtureSuffix + "@upi"
	vpaB := "b-" + fixtureSuffix + "@upi"
	emailA := "owner-a-" + fixtureSuffix + "@example.test"
	emailB := "owner-b-" + fixtureSuffix + "@example.test"

	// Seed tenant and due fixtures using the migration owner, outside app role.
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Payment Intent RLS A', 'Address A', $9, $5, 'Owner A', $7, $3),
		       ($2, 'Payment Intent RLS B', 'Address B', $10, $6, 'Owner B', $8, $4)
	`, propertyA, propertyB, inviteA, inviteB, vpaA, vpaB, emailA, emailB, ownerPhoneA, ownerPhoneB)
	if err != nil {
		t.Fatalf("seed properties: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM payment_intents WHERE id = $1`, intentID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM dues WHERE id IN ($1, $2, $3)`, dueA1, dueA2, dueB)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM tenants WHERE id IN ($1, $2)`, tenantA, tenantB)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM properties WHERE id IN ($1, $2)`, propertyA, propertyB)
	})

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $3, 'Tenant A', $5, 'A-1', 10000, 5, 'active'),
		       ($2, $4, 'Tenant B', $6, 'B-1', 10000, 5, 'active')
	`, tenantA, tenantB, propertyA, propertyB, phoneA, phoneB)
	if err != nil {
		t.Fatalf("seed tenants: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $4, $6, $8, 10000, 10000, 'pending', CURRENT_DATE, 'rent', CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month'),
		       ($2, $5, $6, $8, 10000, 10000, 'pending', CURRENT_DATE + INTERVAL '1 month', 'rent', CURRENT_DATE + INTERVAL '1 month', CURRENT_DATE + INTERVAL '2 months'),
		       ($3, $7, $9, $10, 10000, 10000, 'pending', CURRENT_DATE, 'rent', CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month')
	`, dueA1, dueA2, dueB, "A"+uuid.NewString()[:7], "A"+uuid.NewString()[:7], propertyA, "B"+uuid.NewString()[:7], tenantA, propertyB, tenantB)
	if err != nil {
		t.Fatalf("seed dues: %v", err)
	}

	// A multi-due parent has no due_id. The property_id field is its RLS anchor.
	err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE pgapp_app`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.current_property_id', $1, true)`, propertyA.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_intents (id, property_id, due_id, provider, provider_order_id, amount_paise, status)
			VALUES ($1, $2, NULL, 'cashfree', $3, 20000, 'initiating')
		`, intentID, propertyA, "rls-"+uuid.NewString()); err != nil {
			return fmt.Errorf("insert parent intent for property A: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_intent_dues (payment_intent_id, due_id, amount_paise, status)
			VALUES ($1, $2, 10000, 'initiating'), ($1, $3, 10000, 'initiating')
		`, intentID, dueA1, dueA2); err != nil {
			return fmt.Errorf("insert same-property intent dues: %w", err)
		}
		var visible int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM payment_intents WHERE id = $1`, intentID).Scan(&visible); err != nil {
			return err
		}
		if visible != 1 {
			return fmt.Errorf("property A should see its multi-due parent, got %d rows", visible)
		}
		// Cross-property child link must be rejected by RLS and/or the consistency trigger.
		childTx, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		_, crossPropertyErr := childTx.Exec(ctx, `
			INSERT INTO payment_intent_dues (payment_intent_id, due_id, amount_paise, status)
			VALUES ($1, $2, 10000, 'initiating')
		`, intentID, dueB)
		_ = childTx.Rollback(ctx)
		if crossPropertyErr == nil {
			return fmt.Errorf("cross-property due was attached to the property A intent")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("property A app-role transaction failed: %v", err)
	}

	// Even a maintenance connection must not move the parent away from the
	// property that owns its existing child dues.
	if _, err := pool.Exec(ctx, `UPDATE payment_intents SET property_id = $2 WHERE id = $1`, intentID, propertyB); err == nil {
		t.Fatal("payment intent property changed while linked dues still belong to the original property")
	}

	// Property B and an unscoped app transaction must not see the parent.
	for _, scope := range []struct {
		name       string
		propertyID *uuid.UUID
	}{
		{name: "property B", propertyID: &propertyB},
		{name: "unscoped"},
	} {
		t.Run(scope.name, func(t *testing.T) {
			err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SET LOCAL ROLE pgapp_app`); err != nil {
					return err
				}
				setting := ""
				if scope.propertyID != nil {
					setting = scope.propertyID.String()
				}
				if _, err := tx.Exec(ctx, `SELECT set_config('app.current_property_id', $1, true)`, setting); err != nil {
					return err
				}
				var visible int
				if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM payment_intents WHERE id = $1`, intentID).Scan(&visible); err != nil {
					return err
				}
				if visible != 0 {
					return fmt.Errorf("expected no parent rows under %s scope, got %d", scope.name, visible)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
