package postgres

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLivePostgresMigrateDetectsEditedAppliedMigration proves that editing an already-applied
// migration file makes Migrate fail loudly instead of silently skipping it.
func TestLivePostgresMigrateDetectsEditedAppliedMigration(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use a unique version name and a throwaway table so the shared dev database is not polluted.
	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	table := "mig_checksum_probe_" + suffix
	version := "zz_" + suffix + "_probe.sql"
	dir := t.TempDir()
	file := filepath.Join(dir, version)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+table)
		_, _ = pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version = $1`, version)
	})

	if err := os.WriteFile(file, []byte("CREATE TABLE "+table+" (id INT);"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("first Migrate failed: %v", err)
	}
	// Re-running with an unchanged file is a no-op.
	if err := Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("second Migrate (unchanged) failed: %v", err)
	}

	// Edit the applied file: must now fail with a clear message.
	if err := os.WriteFile(file, []byte("CREATE TABLE "+table+" (id INT, extra INT);"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Migrate(ctx, pool, dir)
	if err == nil || !strings.Contains(err.Error(), "modified after it was applied") {
		t.Fatalf("expected checksum drift error, got %v", err)
	}
}

// TestLivePostgresRefreshTokenRebackfill036 checks that the 036 statement only moves
// family_started_at earlier, and is idempotent.
func TestLivePostgresRefreshTokenRebackfill036(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", "036_refresh_tokens_family_started_at_rebackfill.sql"))
	if err != nil {
		t.Fatal(err)
	}

	userID, familyOld, familyOK := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, phone, role) VALUES ($1, $2, 'tenant')`,
		userID, "+91"+strings.ReplaceAll(uuid.NewString(), "-", "")[:10]); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM refresh_tokens WHERE user_id = $1`, userID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	now := time.Now().UTC()
	day := 24 * time.Hour
	ins := func(fam uuid.UUID, hash string, created, started time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at, revoked, family_started_at, created_at)
			VALUES ($1,$2,$3,$4,$5,FALSE,$6,$7)`,
			uuid.New(), userID, fam, hash+uuid.NewString()[:6], now.Add(30*day), started, created); err != nil {
			t.Fatalf("insert token: %v", err)
		}
	}
	// familyOld looks like the output of the ORIGINAL 034: each row's started == its own created_at.
	ins(familyOld, "old_a_", now.Add(-80*day), now.Add(-80*day))
	ins(familyOld, "old_b_", now.Add(-1*day), now.Add(-1*day))
	// familyOK has a correctly inherited, earlier started_at than its only surviving row's created_at.
	ins(familyOK, "ok_a_", now.Add(-2*day), now.Add(-85*day))

	for i := 0; i < 2; i++ { // run twice: idempotent
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("run 036 (pass %d): %v", i+1, err)
		}
	}

	var minOld, maxOld, okStarted time.Time
	if err := pool.QueryRow(ctx, `SELECT MIN(family_started_at), MAX(family_started_at) FROM refresh_tokens WHERE family_id=$1`, familyOld).Scan(&minOld, &maxOld); err != nil {
		t.Fatal(err)
	}
	if !minOld.Equal(maxOld) || now.Sub(minOld) < 79*day {
		t.Fatalf("familyOld not restored to its earliest created_at: min=%v max=%v", minOld, maxOld)
	}
	if err := pool.QueryRow(ctx, `SELECT family_started_at FROM refresh_tokens WHERE family_id=$1`, familyOK).Scan(&okStarted); err != nil {
		t.Fatal(err)
	}
	if now.Sub(okStarted) < 84*day {
		t.Fatalf("036 must never move family_started_at later; got %v", okStarted)
	}
}

// TestLivePostgresPropertyArchive covers the supported alternative to hard-deleting a property.
func TestLivePostgresPropertyArchive(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	repo := NewPropertyRepo(pool)
	inviteCode := "ARCH" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
	var propID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO properties (name, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ('archive-test', '+919999999999', 'x@upi', 'Owner', 'o@example.com', $1) RETURNING id`, inviteCode).Scan(&propID); err != nil {
		t.Fatalf("insert property: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id=$1`, propID) })

	contains := func() bool {
		list, err := repo.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, p := range list {
			if p.ID == propID {
				return true
			}
		}
		return false
	}
	if !contains() {
		t.Fatal("new property should be listed")
	}
	if err := repo.Archive(ctx, propID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := repo.Archive(ctx, propID); err != nil { // idempotent
		t.Fatalf("second archive: %v", err)
	}
	if contains() {
		t.Fatal("archived property must not be listed")
	}
	if err := repo.Unarchive(ctx, propID); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if !contains() {
		t.Fatal("unarchived property should be listed again")
	}
	if err := repo.Archive(ctx, uuid.New()); err != ErrPropertyNotFound {
		t.Fatalf("expected ErrPropertyNotFound, got %v", err)
	}
}

// TestLivePostgresMigrate_LineEndingNormalizationAndSemanticTamperRejection explicitly proves:
// 1. CRLF version is accepted without modification error.
// 2. LF version is accepted without modification error.
// 3. Semantic content edits are strictly rejected with an invariant violation error.
func TestLivePostgresMigrate_LineEndingNormalizationAndSemanticTamperRejection(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	suffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	table := "mig_probe_norm_" + suffix
	version := "zz_" + suffix + "_norm.sql"
	dir := t.TempDir()
	file := filepath.Join(dir, version)

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+table)
		_, _ = pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version = $1`, version)
	})

	// Step 1: Initial apply with LF line endings
	lfContent := []byte("CREATE TABLE " + table + " (\n    id INT\n);\n")
	if err := os.WriteFile(file, lfContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("initial LF Migrate failed: %v", err)
	}

	// Step 2: Convert to CRLF line endings (e.g. Windows Git checkout) -> MUST BE ACCEPTED
	crlfContent := []byte("CREATE TABLE " + table + " (\r\n    id INT\r\n);\r\n")
	if err := os.WriteFile(file, crlfContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("CRLF version was rejected: %v (line ending normalization invariant broken)", err)
	}

	// Step 3: Revert to LF line endings (e.g. Linux Git checkout) -> MUST BE ACCEPTED
	if err := os.WriteFile(file, lfContent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool, dir); err != nil {
		t.Fatalf("LF version was rejected: %v", err)
	}

	// Step 4: Semantic content tampering -> MUST BE STRICTLY REJECTED
	tamperedContent := []byte("CREATE TABLE " + table + " (\n    id INT,\n    tampered INT\n);\n")
	if err := os.WriteFile(file, tamperedContent, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Migrate(ctx, pool, dir)
	if err == nil {
		t.Fatalf("expected semantic edit to be rejected, but Migrate succeeded")
	}
	if !strings.Contains(err.Error(), "never edit an applied migration, add a new one") {
		t.Fatalf("expected 'never edit an applied migration' error, got %v", err)
	}
}
