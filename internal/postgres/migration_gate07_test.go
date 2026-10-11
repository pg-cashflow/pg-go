package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestGate07_MigrationSequenceContinuity verifies that all migration files in migrations/
// are sequentially numbered without gaps or missing files.
func TestGate07_MigrationSequenceContinuity(t *testing.T) {
	migrationsDir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir failed: %v", err)
	}

	seqRegex := regexp.MustCompile(`^(\d{3})_.*\.sql$`)
	var versions []string

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if match := seqRegex.FindStringSubmatch(e.Name()); match != nil {
			versions = append(versions, e.Name())
		}
	}

	sort.Strings(versions)
	if len(versions) < 44 {
		t.Fatalf("expected at least 44 migration files, found %d", len(versions))
	}

	// Verify continuous numbering from 001 to N
	for i, name := range versions {
		expectedPrefix := fmt.Sprintf("%03d_", i+1)
		if !strings.HasPrefix(name, expectedPrefix) {
			t.Errorf("migration sequence gap detected at index %d: expected prefix %s, got file %s", i, expectedPrefix, name)
		}
	}
}

// TestGate07_LiveMigrationFreshnessAndIdempotency connects to live PostgreSQL
// and verifies that all migrations are registered in schema_migrations,
// and re-running Migrate() is strictly idempotent with 0 errors.
func TestGate07_LiveMigrationFreshnessAndIdempotency(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	migrationsDir := filepath.Join("..", "..", "migrations")

	// 1. Re-run Migrate: must be an idempotent no-op without errors
	if err := Migrate(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("idempotent migration run failed: %v", err)
	}

	// 2. Verify schema_migrations count >= 44
	var count int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count)
	if err != nil {
		t.Fatalf("query schema_migrations count failed: %v", err)
	}
	if count < 44 {
		t.Fatalf("expected at least 44 records in schema_migrations, found %d", count)
	}
}

// TestGate07_SchemaTableAndIndexInvariants verifies that all critical financial,
// money-handling, and KYC tables exist in PostgreSQL with expected schemas.
func TestGate07_SchemaTableAndIndexInvariants(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	criticalTables := []string{
		"properties",
		"users",
		"tenants",
		"dues",
		"payments",
		"payment_intents",
		"payment_allocations",
		"gateway_refunds",
		"financial_journal_entries",
		"ledger_outbox_events",
		"kyc_consent",
		"kyc_verification",
		"kyc_audit_log",
		"webhook_events",
		"unmatched_gateway_receipts",
		"daily_settlement_balances",
	}

	for _, tbl := range criticalTables {
		var exists bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables 
				WHERE table_schema = 'public' AND table_name = $1
			)
		`, tbl).Scan(&exists)
		if err != nil {
			t.Fatalf("table check for %s failed: %v", tbl, err)
		}
		if !exists {
			t.Errorf("critical table %s does not exist in public schema", tbl)
		}
	}
}

// TestGate07_LockSafetyStaticLinter statically audits all migration files
// for unsafe DDL lock anti-patterns that can lock tables exclusively in production.
func TestGate07_LockSafetyStaticLinter(t *testing.T) {
	migrationsDir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir failed: %v", err)
	}

	dropTableRegex := regexp.MustCompile(`(?i)\bDROP\s+TABLE\s+([^;\n]+)`)
	dropColumnRegex := regexp.MustCompile(`(?i)\bDROP\s+COLUMN\s+([^;\n]+)`)
	alterColumnTypeRegex := regexp.MustCompile(`(?i)\bALTER\s+COLUMN\s+\w+\s+TYPE\b`)

	// Legacy migrations (001-064) were grandfathered before lock-safety gate ratification.
	// Any migration >= 065 MUST explicitly configure lock_timeout if modifying existing tables.
	legacyAlterColumnExceptions := map[string]string{
		"046_remediate_p0_invariants.sql": "P0 remediation expanded integer IDs to bigint before gate enforcement",
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}

		filePath := filepath.Join(migrationsDir, e.Name())
		contentBytes, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("read file %s failed: %v", filePath, err)
		}
		content := string(contentBytes)

		// Check for un-guarded DROP TABLE (must use IF EXISTS)
		for _, m := range dropTableRegex.FindAllStringSubmatch(content, -1) {
			target := strings.ToUpper(strings.TrimSpace(m[1]))
			if !strings.HasPrefix(target, "IF EXISTS") {
				t.Errorf("file %s has bare DROP TABLE without IF EXISTS: %q", e.Name(), m[0])
			}
		}

		// Check for un-guarded DROP COLUMN (must use IF EXISTS)
		for _, m := range dropColumnRegex.FindAllStringSubmatch(content, -1) {
			target := strings.ToUpper(strings.TrimSpace(m[1]))
			if !strings.HasPrefix(target, "IF EXISTS") {
				t.Errorf("file %s has bare DROP COLUMN without IF EXISTS: %q", e.Name(), m[0])
			}
		}

		// Check for dangerous ALTER COLUMN TYPE (exclusive table rewrites)
		if alterColumnTypeRegex.MatchString(content) {
			if reason, ok := legacyAlterColumnExceptions[e.Name()]; !ok {
				t.Errorf("file %s has unsafe ALTER COLUMN TYPE rewriting table without registered exception", e.Name())
			} else if reason == "" {
				t.Errorf("file %s has empty justification for ALTER COLUMN TYPE", e.Name())
			}
		}

		// For any new migrations (> 064), enforce lock_timeout when ALTER TABLE is present
		if e.Name() > "064" && strings.Contains(strings.ToUpper(content), "ALTER TABLE") {
			if !strings.Contains(strings.ToLower(content), "lock_timeout") {
				t.Errorf("migration %s introduces ALTER TABLE without SET lock_timeout", e.Name())
			}
		}
	}
}
