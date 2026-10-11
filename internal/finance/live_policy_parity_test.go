package finance_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// TestLivePostgres_PolicyExceededErrorIdentity verifies that when a manager exceeds
// spend limits against the real PostgreSQL repository, the error returned matches
// finance.ErrPolicyExceeded via errors.Is, ensuring HTTP handlers correctly return
// 400 Bad Request instead of falling through to 500 Internal Server Error.
func TestLivePostgres_PolicyExceededErrorIdentity(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := testutil.RequireDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	pool, err := postgres.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer pool.Close()

	// Apply migrations
	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := postgres.Migrate(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("apply migrations failed: %v", err)
	}

	propID := uuid.New()
	managerID := uuid.New()
	inviteCode := fmt.Sprintf("M%s", uuid.New().String()[:7])

	// 1. Seed Property
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_collection_mode)
		VALUES ($1, 'Policy Parity Property', 'Test Address', '+919999900022', 'parity@upi', 'Parity Owner', 'owner_parity@test.com', $2, 'manual_proof')
		ON CONFLICT (id) DO NOTHING
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("failed to insert test property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	// 2. Seed Manager User
	managerPhone := fmt.Sprintf("+9198%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'manager', $3)
		ON CONFLICT (id) DO NOTHING
	`, managerID, managerPhone, propID)
	if err != nil {
		t.Fatalf("failed to insert manager user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, managerID)
	}()

	// 3. Initialize Finance Service with Postgres repository
	repo := postgres.NewFinanceRepo(pool)
	svc := finance.NewService(repo, nil)

	// Ensure default policy is loaded
	if err := repo.EnsureDefaults(ctx, propID); err != nil {
		t.Fatalf("EnsureDefaults failed: %v", err)
	}

	// 4. Submit an expense that exceeds manager daily limit (default daily limit is 10,000 INR = 1,000,000 paise)
	// Submitting 20,000 INR (2,000,000 paise) with non-emergency
	in := finance.CreateExpenseInput{
		PropertyID:     propID,
		CategoryCode:   "maintenance",
		VendorName:     "Test Plumbing",
		Description:    "Exceeding spend cap",
		AmountPaise:    2_000_000,
		Emergency:      false,
		ActorID:        managerID,
		ActorRole:      string(domain.RoleManager),
		IdempotencyKey: uuid.New().String(),
		OccurredAt:     time.Now().UTC(),
	}

	_, _, err = svc.CreateExpense(ctx, in)
	if err == nil {
		t.Fatal("expected CreateExpense to fail with spend policy exceeded, got nil error")
	}

	// The crucial assertion: HTTP layer checks errors.Is(err, finance.ErrPolicyExceeded)
	if !errors.Is(err, finance.ErrPolicyExceeded) {
		t.Fatalf("expected error to match finance.ErrPolicyExceeded via errors.Is, got: %v", err)
	}
}
