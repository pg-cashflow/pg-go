package postgres

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLivePostgres_FinanceInvariantsDatabaseConstraints(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := Migrate(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("failed to apply migrations: %v", err)
	}

	propID := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode)
		VALUES ($1, 'Constraint Test Property', 'Test Address', '+919999900011', 'test@upi', 'Test Owner', 'owner_test@example.com', $2, 'manual')
		ON CONFLICT (id) DO NOTHING
	`, propID, "INV"+propID.String()[:5])
	if err != nil {
		t.Fatalf("failed to insert test property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	// 1. Test fiscal_month_start_day > 28 is refused by DB check constraint
	_, err = pool.Exec(ctx, `
		INSERT INTO property_finance_settings (property_id, fiscal_month_start_day, tdr_effective_bps)
		VALUES ($1, 29, 100)
	`, propID)
	if err == nil || !strings.Contains(err.Error(), "chk_property_finance_settings_fiscal_day") {
		t.Fatalf("expected chk_property_finance_settings_fiscal_day violation for day 29, got err: %v", err)
	}

	// 2. Test fiscal_month_start_day = 0 is refused
	_, err = pool.Exec(ctx, `
		INSERT INTO property_finance_settings (property_id, fiscal_month_start_day, tdr_effective_bps)
		VALUES ($1, 0, 100)
	`, propID)
	if err == nil || !strings.Contains(err.Error(), "chk_property_finance_settings_fiscal_day") {
		t.Fatalf("expected chk_property_finance_settings_fiscal_day violation for day 0, got err: %v", err)
	}

	// 3. Test tdr_effective_bps > 10000 is refused
	_, err = pool.Exec(ctx, `
		INSERT INTO property_finance_settings (property_id, fiscal_month_start_day, tdr_effective_bps)
		VALUES ($1, 15, 10001)
	`, propID)
	if err == nil || !strings.Contains(err.Error(), "chk_property_finance_settings_tdr_bps") {
		t.Fatalf("expected chk_property_finance_settings_tdr_bps violation, got err: %v", err)
	}

	// 4. Valid finance settings insertion succeeds
	_, err = pool.Exec(ctx, `
		INSERT INTO property_finance_settings (property_id, fiscal_month_start_day, tdr_effective_bps)
		VALUES ($1, 28, 100)
	`, propID)
	if err != nil {
		t.Fatalf("expected valid settings to succeed, got: %v", err)
	}

	// 5. Test ordering violation: single > daily is refused by DB check constraint
	_, err = pool.Exec(ctx, `
		INSERT INTO approval_policies (property_id, single_expense_limit_paise, manager_daily_limit_paise, manager_monthly_limit_paise)
		VALUES ($1, 2000000, 1000000, 5000000)
	`, propID)
	if err == nil || !strings.Contains(err.Error(), "chk_approval_policies_ordering") {
		t.Fatalf("expected chk_approval_policies_ordering violation for single > daily, got err: %v", err)
	}

	// 6. Test ordering violation: daily > monthly is refused
	_, err = pool.Exec(ctx, `
		INSERT INTO approval_policies (property_id, single_expense_limit_paise, manager_daily_limit_paise, manager_monthly_limit_paise)
		VALUES ($1, 500000, 6000000, 5000000)
	`, propID)
	if err == nil || !strings.Contains(err.Error(), "chk_approval_policies_ordering") {
		t.Fatalf("expected chk_approval_policies_ordering violation for daily > monthly, got err: %v", err)
	}

	// 7. Test negative value is refused
	_, err = pool.Exec(ctx, `
		INSERT INTO approval_policies (property_id, single_expense_limit_paise, manager_daily_limit_paise, manager_monthly_limit_paise)
		VALUES ($1, -500, 1000000, 5000000)
	`, propID)
	if err == nil || !strings.Contains(err.Error(), "chk_approval_policies_non_negative") {
		t.Fatalf("expected chk_approval_policies_non_negative violation, got err: %v", err)
	}

	// 8. Test value exceeding ceiling (10^11 paise) is refused
	_, err = pool.Exec(ctx, `
		INSERT INTO approval_policies (property_id, single_expense_limit_paise, manager_daily_limit_paise, manager_monthly_limit_paise)
		VALUES ($1, 500000, 100000000001, 100000000001)
	`, propID)
	if err == nil || !strings.Contains(err.Error(), "chk_approval_policies_max_ceiling") {
		t.Fatalf("expected chk_approval_policies_max_ceiling violation, got err: %v", err)
	}

	// 9. Valid approval policy insertion succeeds
	_, err = pool.Exec(ctx, `
		INSERT INTO approval_policies (property_id, single_expense_limit_paise, manager_daily_limit_paise, manager_monthly_limit_paise)
		VALUES ($1, 500000, 1000000, 5000000)
	`, propID)
	if err != nil {
		t.Fatalf("expected valid policy to succeed, got: %v", err)
	}
}
