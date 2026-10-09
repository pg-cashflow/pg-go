package postgres

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLivePostgresBalanceRunsImmutabilityAndCascadeBlock(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Ensure migrations 031, 032, and 035 are applied
	for _, m := range []string{
		"031_daily_settlement_balances.sql",
		"032_settlement_balance_runs.sql",
		"035_balance_runs_immutability.sql",
	} {
		mPath := filepath.Join("..", "..", "migrations", m)
		mSQL, err := os.ReadFile(mPath)
		if err != nil {
			t.Fatalf("failed reading migration %s: %v", m, err)
		}
		if _, err := pool.Exec(ctx, string(mSQL)); err != nil {
			t.Fatalf("failed applying migration %s: %v", m, err)
		}
	}

	propID := uuid.New()
	inviteCode := "AUD" + propID.String()[:6]
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, $2, '123 Test St', '+919876543210', 'audit@upi', 'Audit Owner', 'audit@test.com', $3)
		ON CONFLICT (id) DO NOTHING`,
		propID, "Audit Immutability Property "+propID.String()[:8], inviteCode,
	)
	if err != nil {
		t.Fatalf("failed inserting test property: %v", err)
	}

	balancer := NewSettlementBalancerRepo(pool)
	reconDate := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	bal := &domain.DailySettlementBalance{
		PropertyID:            propID,
		ReconDate:             reconDate,
		GatewayGrossPaise:     100000,
		GatewayNetSettledPaise: 98000,
		GatewayFeesPaise:      2000,
		BankCreditsPaise:      98000,
		IsBalanced:            true,
	}

	if err := balancer.UpsertDailyBalance(ctx, bal); err != nil {
		t.Fatalf("failed inserting daily balance: %v", err)
	}

	// Fetch run ID
	var runID uuid.UUID
	err = pool.QueryRow(ctx, `SELECT id FROM daily_settlement_balance_runs WHERE property_id = $1 LIMIT 1`, propID).Scan(&runID)
	if err != nil {
		t.Fatalf("failed querying balance run: %v", err)
	}

	t.Run("direct UPDATE on balance runs is blocked", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE daily_settlement_balance_runs SET gateway_gross_paise = 99999 WHERE id = $1`, runID)
		if err == nil {
			t.Fatalf("expected error updating immutable balance run, got nil")
		}
		if !strings.Contains(err.Error(), "immutable append-only audit table") {
			t.Fatalf("expected immutability error message, got: %v", err)
		}
	})

	t.Run("direct DELETE on balance runs is blocked", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM daily_settlement_balance_runs WHERE id = $1`, runID)
		if err == nil {
			t.Fatalf("expected error deleting immutable balance run, got nil")
		}
		if !strings.Contains(err.Error(), "immutable append-only audit table") {
			t.Fatalf("expected immutability error message, got: %v", err)
		}
	})

	t.Run("cascaded DELETE from properties is blocked by audit immutability", func(t *testing.T) {
		// Attempting to hard-delete a property that has recorded balance runs
		// must fail because the trigger fires on the cascaded delete to daily_settlement_balance_runs.
		_, err := pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
		if err == nil {
			t.Fatalf("expected error when cascade-deleting property with immutable balance runs, got nil")
		}
		if !strings.Contains(err.Error(), "immutable append-only audit table") {
			t.Fatalf("expected immutability error message on cascaded delete, got: %v", err)
		}
	})
}
