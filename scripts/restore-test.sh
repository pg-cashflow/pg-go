#!/usr/bin/env bash
# Automated Backup Restoration & Data Integrity Verification Test
# Spec: Master Engineering Prompt §31 ("A backup is not valid until restore is tested")
# Usage:
#   ./scripts/restore-test.sh [path/to/backup.sql.gz]
# Environment:
#   TEST_DATABASE_URL: Target database URL to restore into (default: postgres://postgres:postgres@localhost:5432/pg_restore_test?sslmode=disable)
#   KEEP_RESTORE_DB: If "1", preserves the test database upon completion.
set -euo pipefail

BACKUP_FILE="${1:-}"
TEST_DB_URL="${TEST_DATABASE_URL:-postgres://postgres:postgres@localhost:5432/pg_restore_test?sslmode=disable}"
KEEP_RESTORE_DB="${KEEP_RESTORE_DB:-0}"

if [[ -z "$BACKUP_FILE" ]]; then
    # Look for most recent local dump in /tmp
    LATEST_LOCAL=$(ls -t /tmp/pg-go-backup-*.sql.gz 2>/dev/null | head -n 1 || true)
    if [[ -n "$LATEST_LOCAL" && -f "$LATEST_LOCAL" ]]; then
        BACKUP_FILE="$LATEST_LOCAL"
        echo "[INFO] No backup file provided. Found latest local backup: $BACKUP_FILE"
    else
        echo "[ERROR] No backup file specified and no recent backup found in /tmp."
        echo "Usage: $0 <path-to-backup.sql.gz>"
        exit 1
    fi
fi

if [[ ! -f "$BACKUP_FILE" ]]; then
    echo "[ERROR] Backup file not found: $BACKUP_FILE"
    exit 1
fi

echo "================================================================"
echo "Starting Backup Restoration & Financial Verification Test"
echo "Backup File: $BACKUP_FILE"
echo "Target DB:   $TEST_DB_URL"
echo "================================================================"

# Extract base connection without database name for CREATE/DROP operations
BASE_URL="${TEST_DB_URL%/*}"
RESTORE_DB_NAME="${TEST_DB_URL##*/}"
RESTORE_DB_NAME="${RESTORE_DB_NAME%%\?*}"

echo "[1/4] Recreating target database '$RESTORE_DB_NAME'..."
psql "$BASE_URL/postgres" -v ON_ERROR_STOP=1 <<EOF >/dev/null 2>&1 || true
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$RESTORE_DB_NAME';
DROP DATABASE IF EXISTS "$RESTORE_DB_NAME";
CREATE DATABASE "$RESTORE_DB_NAME";
EOF

echo "[2/4] Restoring schema and data..."
if [[ "$BACKUP_FILE" == *.gz ]]; then
    gunzip -c "$BACKUP_FILE" | psql "$TEST_DB_URL" -v ON_ERROR_STOP=1 >/dev/null
else
    psql "$TEST_DB_URL" -v ON_ERROR_STOP=1 < "$BACKUP_FILE" >/dev/null
fi

echo "[3/4] Verifying database integrity & financial invariants..."

# Query 1: Verify core tables exist
TABLES_COUNT=$(psql "$TEST_DB_URL" -t -A -c "
    SELECT count(*) FROM information_schema.tables 
    WHERE table_name IN ('properties', 'tenants', 'dues', 'payments', 'financial_journal_entries', 'ledger_outbox_events');
")
if [[ "$TABLES_COUNT" -lt 6 ]]; then
    echo "[FAIL] Missing core tables! Found only $TABLES_COUNT of 6 required tables."
    exit 1
fi
echo "  ✓ Core tables present: $TABLES_COUNT tables verified."

# Query 2: Invariant Check — Double-entry balance
UNBALANCED=$(psql "$TEST_DB_URL" -t -A -c "
    SELECT coalesce(sum(debit_paise) - sum(credit_paise), 0) FROM financial_journal_entries;
")
if [[ "$UNBALANCED" -ne 0 ]]; then
    echo "[FAIL] Double-entry ledger imbalance detected! Net difference: ${UNBALANCED} paise"
    exit 1
fi
echo "  ✓ Double-entry balance holds: Net difference is 0 paise."

# Query 3: Invariant Check — Per-property & source journal balance
UNBALANCED_SOURCES=$(psql "$TEST_DB_URL" -t -A -c "
    SELECT count(*) FROM (
        SELECT property_id, source_type, source_id
        FROM financial_journal_entries
        GROUP BY property_id, source_type, source_id
        HAVING sum(debit_paise) <> sum(credit_paise)
    ) imbalanced;
")
if [[ "$UNBALANCED_SOURCES" -ne 0 ]]; then
    echo "[FAIL] Found $UNBALANCED_SOURCES imbalanced journal entries!"
    exit 1
fi
echo "  ✓ All journal source entries are strictly balanced."

# Query 4: Invariant Check — Integer paise validation (no negative or decimal values)
INVALID_PAISE=$(psql "$TEST_DB_URL" -t -A -c "
    SELECT count(*) FROM financial_journal_entries 
    WHERE debit_paise < 0 OR credit_paise < 0;
")
if [[ "$INVALID_PAISE" -ne 0 ]]; then
    echo "[FAIL] Found $INVALID_PAISE negative journal line values!"
    exit 1
fi
echo "  ✓ Zero negative amounts in financial journal."

echo "[4/4] Cleanup..."
if [[ "$KEEP_RESTORE_DB" == "1" ]]; then
    echo "  [INFO] Preserving restore database '$RESTORE_DB_NAME' (KEEP_RESTORE_DB=1)."
else
    psql "$BASE_URL/postgres" -v ON_ERROR_STOP=1 <<EOF >/dev/null 2>&1 || true
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$RESTORE_DB_NAME';
DROP DATABASE IF EXISTS "$RESTORE_DB_NAME";
EOF
    echo "  ✓ Ephemeral test database cleaned up."
fi

echo "================================================================"
echo "RESTORE VERIFICATION TEST: PASSED"
echo "All schema, data, and double-entry invariants verified."
echo "================================================================"
exit 0
