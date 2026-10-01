package postgres

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestPropertySettings_GetAndUpsert(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live property settings test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping test", err)
	}
	defer pool.Close()

	repo := NewPropertyRepo(pool)

	// 1. Seed a test property
	propID := uuid.New()
	ownerPhone := "9998887766"
	inviteCode := "SETT" + propID.String()[:4]
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Settings Test PG', $2, 'settings@upi', 'Settings Owner', 'settings@example.com', $3)
		ON CONFLICT (id) DO NOTHING`, propID, ownerPhone, inviteCode)
	if err != nil {
		t.Fatalf("seed property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id=$1`, propID)
	}()

	// 2. Fetch settings when no row exists yet (or backfilled default)
	settings, err := repo.GetSettings(ctx, propID)
	if err != nil {
		t.Fatalf("GetSettings failed: %v", err)
	}
	if settings.PropertyID != propID {
		t.Errorf("expected property ID %v, got %v", propID, settings.PropertyID)
	}
	if settings.PayoutAutoDispatch != false {
		t.Errorf("expected default PayoutAutoDispatch=false, got %v", settings.PayoutAutoDispatch)
	}
	if settings.ReminderCatchUpDays != 2 {
		t.Errorf("expected default ReminderCatchUpDays=2, got %v", settings.ReminderCatchUpDays)
	}
	expectedOffsets := []int{-3, 0, 1, 7}
	if !reflect.DeepEqual(settings.ReminderOffsets, expectedOffsets) {
		t.Errorf("expected default offsets %v, got %v", expectedOffsets, settings.ReminderOffsets)
	}

	// 3. Upsert custom settings
	custom := &domain.PropertySettings{
		PropertyID:          propID,
		PayoutAutoDispatch:  true,
		ReminderOffsets:     []int{-2, 0, 1, 5},
		ReminderCatchUpDays: 3,
		ActiveModules: map[string]bool{
			"gamification": true,
			"kyc":          false,
			"payouts":      true,
			"accounting":   true,
			"calendar":     false,
		},
		AutoApplyCredit: false,
	}

	err = repo.UpsertSettings(ctx, custom)
	if err != nil {
		t.Fatalf("UpsertSettings failed: %v", err)
	}

	// 4. Retrieve and verify custom settings persisted
	updated, err := repo.GetSettings(ctx, propID)
	if err != nil {
		t.Fatalf("GetSettings after upsert failed: %v", err)
	}
	if !updated.PayoutAutoDispatch {
		t.Errorf("expected PayoutAutoDispatch=true, got %v", updated.PayoutAutoDispatch)
	}
	if updated.ReminderCatchUpDays != 3 {
		t.Errorf("expected ReminderCatchUpDays=3, got %v", updated.ReminderCatchUpDays)
	}
	if !reflect.DeepEqual(updated.ReminderOffsets, []int{-2, 0, 1, 5}) {
		t.Errorf("expected updated offsets [-2 0 1 5], got %v", updated.ReminderOffsets)
	}
	if updated.ActiveModules["kyc"] != false {
		t.Errorf("expected kyc module to be false, got %v", updated.ActiveModules["kyc"])
	}
	if updated.AutoApplyCredit != false {
		t.Errorf("expected AutoApplyCredit=false, got %v", updated.AutoApplyCredit)
	}
}
