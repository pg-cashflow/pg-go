package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestLiveFinancialCategories_RLSSplitProtection(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	propA := uuid.New()
	propB := uuid.New()

	// Seed properties
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Category Prop A', '1 A St', '+919888800001', 'a@upi', 'Owner A', 'a@cat.com', $3),
		       ($2, 'Category Prop B', '2 B St', '+919888800002', 'b@upi', 'Owner B', 'b@cat.com', $4)
	`, propA, propB, "CA"+uuid.New().String()[:5], "CB"+uuid.New().String()[:5])
	if err != nil {
		t.Fatalf("failed inserting properties: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id IN ($1, $2)`, propA, propB)
	}()

	globalCode := fmt.Sprintf("GLOB_%s", uuid.New().String()[:8])
	propACode := fmt.Sprintf("PROPA_%s", uuid.New().String()[:8])
	propBCode := fmt.Sprintf("PROPB_%s", uuid.New().String()[:8])

	// Seed categories: 1 global (property_id NULL), 1 propA, 1 propB
	_, err = pool.Exec(ctx, `
		INSERT INTO financial_categories (property_id, code, name)
		VALUES (NULL, $1, 'Global Cat Test'),
		       ($2,   $3, 'Prop A Cat Test'),
		       ($4,   $5, 'Prop B Cat Test')
	`, globalCode, propA, propACode, propB, propBCode)
	if err != nil {
		t.Fatalf("failed inserting financial categories: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM financial_categories WHERE code IN ($1, $2, $3)`, globalCode, propACode, propBCode)
	}()

	// Connect as pgapp_app scoped to Property A
	err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
			return fmt.Errorf("set local role pgapp_app: %w", err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propA.String()); err != nil {
			return fmt.Errorf("set local app.current_property_id: %w", err)
		}

		// 1. SELECT test: pgapp_app should see global category + Prop A category, but NOT Prop B category
		rows, err := tx.Query(ctx, `SELECT code FROM financial_categories WHERE code IN ($1, $2, $3)`, globalCode, propACode, propBCode)
		if err != nil {
			return fmt.Errorf("select categories: %w", err)
		}
		defer rows.Close()

		visible := make(map[string]bool)
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return err
			}
			visible[c] = true
		}
		if !visible[globalCode] {
			t.Errorf("expected global category %s to be visible to pgapp_app", globalCode)
		}
		if !visible[propACode] {
			t.Errorf("expected prop A category %s to be visible to pgapp_app", propACode)
		}
		if visible[propBCode] {
			t.Errorf("prop B category %s should NOT be visible to pgapp_app (isolation leak)", propBCode)
		}

		// 2. UPDATE test on global category: pgapp_app must NOT be able to modify global categories
		tagUpdate, err := tx.Exec(ctx, `UPDATE financial_categories SET name = 'Hacked Global' WHERE code = $1`, globalCode)
		if err != nil {
			// Some PostgreSQL RLS configurations return an error on WITH CHECK violation
			// which is also acceptable protection.
		} else if tagUpdate.RowsAffected() != 0 {
			t.Errorf("CRITICAL: pgapp_app was able to update global category! rows affected: %d", tagUpdate.RowsAffected())
		}

		// 3. DELETE test on global category: pgapp_app must NOT be able to delete global categories
		tagDelete, err := tx.Exec(ctx, `DELETE FROM financial_categories WHERE code = $1`, globalCode)
		if err != nil {
			// Error on delete is acceptable
		} else if tagDelete.RowsAffected() != 0 {
			t.Errorf("CRITICAL: pgapp_app was able to delete global category! rows affected: %d", tagDelete.RowsAffected())
		}

		// 4. UPDATE test on Prop A category: pgapp_app SHOULD be able to modify its own property category
		tagPropUpdate, err := tx.Exec(ctx, `UPDATE financial_categories SET name = 'Updated Prop A Name' WHERE code = $1`, propACode)
		if err != nil {
			return fmt.Errorf("update prop A category: %w", err)
		}
		if tagPropUpdate.RowsAffected() != 1 {
			t.Errorf("expected 1 row updated for Prop A category, got %d", tagPropUpdate.RowsAffected())
		}

		return nil
	})
	if err != nil {
		t.Fatalf("RLS test transaction failed: %v", err)
	}

	// Verify outside pgapp_app that global category was never modified or deleted
	var globalName string
	err = pool.QueryRow(ctx, `SELECT name FROM financial_categories WHERE code = $1`, globalCode).Scan(&globalName)
	if err != nil {
		t.Fatalf("failed querying global category after test: %v", err)
	}
	if globalName != "Global Cat Test" {
		t.Errorf("global category was modified! got name %q", globalName)
	}
}
