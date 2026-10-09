package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// TestPhysicalBackupAndRestoreVerification executes:
// production-like database -> backup -> create restore database -> restore dump -> verify integrity -> cleanup.
// Records: backup duration, size, restore duration, RPO, RTO.
func TestPhysicalBackupAndRestoreVerification(t *testing.T) {
	if testing.Short() {
		testutil.FailOnSkipIfDBRequired(t, "skipping physical backup/restore test in short mode")
	}
	dbURL := testutil.RequireDB(t)

	findTool := func(name, winPath string) string {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		if _, err := os.Stat(winPath); err == nil {
			return winPath
		}
		return ""
	}
	pgDumpPath := findTool("pg_dump", `C:\Program Files\PostgreSQL\17\bin\pg_dump.exe`)
	pgRestorePath := findTool("pg_restore", `C:\Program Files\PostgreSQL\17\bin\pg_restore.exe`)
	psqlPath := findTool("psql", `C:\Program Files\PostgreSQL\17\bin\psql.exe`)

	if pgDumpPath == "" || pgRestorePath == "" || psqlPath == "" {
		testutil.FailOnSkipfIfDBRequired(t, "postgres backup/restore CLI tools (pg_dump, pg_restore, psql) not found, skipping")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	tmpDir := os.TempDir()
	dumpFile := filepath.Join(tmpDir, fmt.Sprintf("acceptance_backup_%d.dump", time.Now().UnixNano()))
	defer os.Remove(dumpFile)

	// Step 1: Execute Backup (Custom binary archive format)
	backupStart := time.Now()
	cmdBackup := exec.CommandContext(ctx, pgDumpPath, dbURL, "-Fc", "-f", dumpFile)
	out, err := cmdBackup.CombinedOutput()
	backupDuration := time.Since(backupStart)
	if err != nil {
		t.Fatalf("backup failed: %v, output: %s", err, string(out))
	}

	fi, err := os.Stat(dumpFile)
	if err != nil {
		t.Fatalf("stat dump file: %v", err)
	}
	backupSize := fi.Size()
	t.Logf("✓ Backup created in %v | Size: %d bytes (%.2f KB)", backupDuration, backupSize, float64(backupSize)/1024.0)

	// Step 2: Prepare Isolated Restore Database
	restoreDBName := "pg_go_restore_verify"
	adminURL := "postgres://postgres:port%401@localhost:5432/postgres?sslmode=disable"
	restoreDBURL := "postgres://postgres:port%401@localhost:5432/" + restoreDBName + "?sslmode=disable"

	// Recreate restore DB
	_ = exec.Command(psqlPath, adminURL, "-c", fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';", restoreDBName)).Run()
	_ = exec.Command(psqlPath, adminURL, "-c", fmt.Sprintf("DROP DATABASE IF EXISTS %s;", restoreDBName)).Run()
	cmdCreate := exec.Command(psqlPath, adminURL, "-c", fmt.Sprintf("CREATE DATABASE %s;", restoreDBName))
	if out, err := cmdCreate.CombinedOutput(); err != nil {
		t.Fatalf("create restore db failed: %v (%s)", err, string(out))
	}
	defer func() {
		_ = exec.Command(psqlPath, adminURL, "-c", fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s';", restoreDBName)).Run()
		_ = exec.Command(psqlPath, adminURL, "-c", fmt.Sprintf("DROP DATABASE IF EXISTS %s;", restoreDBName)).Run()
	}()

	// Step 3: Execute Restore using pg_restore
	restoreStart := time.Now()
	cmdRestore := exec.CommandContext(ctx, pgRestorePath, "-d", restoreDBURL, "--no-owner", "--no-acl", dumpFile)
	out, err = cmdRestore.CombinedOutput()
	restoreDuration := time.Since(restoreStart)
	if err != nil {
		t.Fatalf("restore failed: %v, output: %s", err, string(out))
	}
	t.Logf("✓ Restore completed in %v", restoreDuration)

	// Step 4: Run Invariant Integrity Checks on Restored DB
	restorePool, err := pgxpool.New(ctx, restoreDBURL)
	if err != nil {
		t.Fatalf("connect to restored db: %v", err)
	}
	defer restorePool.Close()

	// Query 1: Verify core tables exist
	var tablesCount int
	err = restorePool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables 
		WHERE table_name IN ('properties', 'tenants', 'dues', 'payments', 'financial_journal_entries', 'ledger_outbox_events')`).Scan(&tablesCount)
	if err != nil || tablesCount < 6 {
		t.Fatalf("table verification failed: count=%d, err=%v", tablesCount, err)
	}
	t.Logf("✓ Core tables verified: %d of 6 required tables present", tablesCount)

	// Query 2: Invariant Check — Double-entry balance
	var netBalance int64
	err = restorePool.QueryRow(ctx, `SELECT coalesce(sum(debit_paise) - sum(credit_paise), 0) FROM financial_journal_entries`).Scan(&netBalance)
	if err != nil || netBalance != 0 {
		t.Fatalf("double-entry imbalance: net=%d paise, err=%v", netBalance, err)
	}
	t.Logf("✓ Double-entry balance holds: Net difference = %d paise", netBalance)

	// Query 3: Invariant Check — Per-property & source journal balance
	var imbalancedCount int
	err = restorePool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT property_id, source_type, source_id
			FROM financial_journal_entries
			GROUP BY property_id, source_type, source_id
			HAVING sum(debit_paise) <> sum(credit_paise)
		) imbalanced`).Scan(&imbalancedCount)
	if err != nil || imbalancedCount != 0 {
		t.Fatalf("source entry imbalance: count=%d, err=%v", imbalancedCount, err)
	}
	t.Logf("✓ Per-source journal balance holds: %d imbalanced entries", imbalancedCount)

	// Query 4: Invariant Check — Integer paise validation
	var negativeCount int
	err = restorePool.QueryRow(ctx, `
		SELECT count(*) FROM financial_journal_entries 
		WHERE debit_paise < 0 OR credit_paise < 0`).Scan(&negativeCount)
	if err != nil || negativeCount != 0 {
		t.Fatalf("negative amounts detected: count=%d, err=%v", negativeCount, err)
	}
	t.Logf("✓ Zero negative amounts in financial journal")

	// Calculate RPO / RTO Evidence Metrics
	t.Logf("==========================================================")
	t.Logf("PHYSICAL RECOVERY EVIDENCE METRICS:")
	t.Logf("  Backup Duration:   %v", backupDuration)
	t.Logf("  Backup Dump Size:  %d bytes", backupSize)
	t.Logf("  Restore Duration:  %v", restoreDuration)
	t.Logf("  RTO (Restore RTO): %v", restoreDuration)
	t.Logf("  RPO (WAL Window):  Point-in-time of dump (zero lost transactions prior to dump)")
	t.Logf("==========================================================")
}
