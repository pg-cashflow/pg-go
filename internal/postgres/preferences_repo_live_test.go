package postgres

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLivePostgresPreferencesAndCheckConstraint(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Read and apply migration 014 directly from migrations/014_user_preferences.sql
	migrationPath := filepath.Join("..", "..", "migrations", "014_user_preferences.sql")
	migrationBytes, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("failed to read migration file %s: %v", migrationPath, err)
	}
	if _, err := pool.Exec(ctx, string(migrationBytes)); err != nil {
		t.Fatalf("apply migration 014 SQL: %v", err)
	}

	// Read and apply migration 082 directly from migrations/082_user_preferences_theme.sql
	themeMigrationPath := filepath.Join("..", "..", "migrations", "082_user_preferences_theme.sql")
	themeMigrationBytes, err := os.ReadFile(themeMigrationPath)
	if err != nil {
		t.Fatalf("failed to read migration file %s: %v", themeMigrationPath, err)
	}
	if _, err := pool.Exec(ctx, string(themeMigrationBytes)); err != nil {
		t.Fatalf("apply migration 082 SQL: %v", err)
	}

	// Verify table and constraint exist in PostgreSQL system catalogs
	var tableExists bool
	err = pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables 
			WHERE table_name = 'user_preferences'
		)
	`).Scan(&tableExists)
	if err != nil || !tableExists {
		t.Fatalf("user_preferences table does not exist in Postgres: %v", err)
	}

	var constraintExists bool
	err = pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint 
			WHERE conname = 'chk_user_preferences_locale'
		)
	`).Scan(&constraintExists)
	if err != nil || !constraintExists {
		t.Fatalf("chk_user_preferences_locale constraint does not exist in Postgres: %v", err)
	}

	// Create a test user to satisfy foreign key
	userRepo := NewUserRepo(pool)
	testUser := &domain.User{
		Role:  domain.RoleOwner,
		Phone: "+9199" + fmtRandomPhone(),
		Email: "test-" + uuid.NewString()[:8] + "@example.com",
	}
	if err := userRepo.Create(ctx, testUser); err != nil {
		t.Fatalf("create test user: %v", err)
	}

	repo := NewPreferencesRepo(pool)

	// 1a. Directly verify PostgreSQL check constraint violation returns SQLSTATE 23514
	_, rawErr := pool.Exec(ctx, `INSERT INTO user_preferences (user_id, locale) VALUES ($1, $2)`, testUser.ID, "fr-FR")
	if rawErr == nil {
		t.Fatalf("expected live Postgres error on inserting 'fr-FR', got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(rawErr, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("expected SQLSTATE 23514 (check_violation), got: %v", rawErr)
	}
	if pgErr.ConstraintName != "chk_user_preferences_locale" {
		t.Fatalf("expected constraint name 'chk_user_preferences_locale', got: %s", pgErr.ConstraintName)
	}

	// 1b. Verify preferences repo Upsert cleanly maps 23514 to ErrInvalidLocale
	_, err = repo.Upsert(ctx, testUser.ID, strPtr("fr-FR"), nil)
	if err == nil {
		t.Fatalf("expected error for invalid locale 'fr-FR', got nil")
	}
	if !errors.Is(err, ErrInvalidLocale) {
		t.Fatalf("expected ErrInvalidLocale from live check constraint, got: %v", err)
	}

	// 2. Verify valid locale insert; theme stays unset (nil) because it was never sent
	pref, err := repo.Upsert(ctx, testUser.ID, strPtr("te-IN"), nil)
	if err != nil {
		t.Fatalf("upsert valid locale 'te-IN': %v", err)
	}
	if pref.Locale != "te-IN" {
		t.Errorf("expected 'te-IN', got %s", pref.Locale)
	}
	if pref.Theme != nil {
		t.Errorf("expected theme to remain nil when not sent, got %v", *pref.Theme)
	}

	// 3. Verify get by user ID
	fetched, err := repo.GetByUserID(ctx, testUser.ID)
	if err != nil {
		t.Fatalf("GetByUserID: %v", err)
	}
	if fetched.Locale != "te-IN" {
		t.Errorf("expected 'te-IN', got %s", fetched.Locale)
	}

	// 4. Verify theme-only update does NOT clobber the previously saved locale
	themed, err := repo.Upsert(ctx, testUser.ID, nil, strPtr("dark"))
	if err != nil {
		t.Fatalf("upsert theme 'dark': %v", err)
	}
	if themed.Locale != "te-IN" {
		t.Errorf("theme-only update must not change locale; expected 'te-IN', got %s", themed.Locale)
	}
	if themed.Theme == nil || *themed.Theme != "dark" {
		t.Errorf("expected theme 'dark', got %v", themed.Theme)
	}

	// 5. Verify locale-only update does NOT clobber the previously saved theme
	localeOnly, err := repo.Upsert(ctx, testUser.ID, strPtr("kn-IN"), nil)
	if err != nil {
		t.Fatalf("upsert update to 'kn-IN': %v", err)
	}
	if localeOnly.Locale != "kn-IN" {
		t.Errorf("expected 'kn-IN', got %s", localeOnly.Locale)
	}
	if localeOnly.Theme == nil || *localeOnly.Theme != "dark" {
		t.Errorf("locale-only update must not change theme; expected 'dark', got %v", localeOnly.Theme)
	}

	// 6. Verify an invalid theme is rejected and distinguished from ErrInvalidLocale
	_, err = repo.Upsert(ctx, testUser.ID, nil, strPtr("neon"))
	if err == nil {
		t.Fatalf("expected error for invalid theme 'neon', got nil")
	}
	if !errors.Is(err, ErrInvalidTheme) {
		t.Fatalf("expected ErrInvalidTheme from live check constraint, got: %v", err)
	}

	// Clean up test user
	_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, testUser.ID)
}

func strPtr(s string) *string { return &s }

func fmtRandomPhone() string {
	u := uuid.New()
	digits := ""
	for _, b := range u[:] {
		digits += string('0' + (b % 10))
		if len(digits) >= 8 {
			break
		}
	}
	return digits
}
