#!/usr/bin/env bash
# Database Backup Script
# Creates a compressed PostgreSQL dump and uploads to Cloudflare R2 if configured,
# or saves to a local backup directory.
set -euo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"

BACKUP_DIR="${BACKUP_DIR:-/tmp/pg-backups}"
mkdir -p "$BACKUP_DIR"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="${BACKUP_DIR}/pg-go-backup-${STAMP}.sql.gz"

echo "Creating compressed backup at ${OUT}..."
pg_dump "$DATABASE_URL" | gzip -c > "$OUT"
echo "Backup created successfully ($(du -h "$OUT" | cut -f1))."

# If R2 environment variables are present, upload via rclone
if [[ -n "${R2_BUCKET:-}" && -n "${R2_ENDPOINT:-}" && -n "${R2_ACCESS_KEY_ID:-}" && -n "${R2_SECRET_ACCESS_KEY:-}" ]]; then
    export RCLONE_CONFIG_R2_TYPE="${RCLONE_CONFIG_R2_TYPE:-s3}"
    export RCLONE_CONFIG_R2_PROVIDER="${RCLONE_CONFIG_R2_PROVIDER:-Cloudflare}"
    export RCLONE_CONFIG_R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
    export RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
    export RCLONE_CONFIG_R2_ENDPOINT="$R2_ENDPOINT"
    export RCLONE_CONFIG_R2_ACL="${RCLONE_CONFIG_R2_ACL:-private}"
    export RCLONE_CONFIG_R2_NO_CHECK_BUCKET="${RCLONE_CONFIG_R2_NO_CHECK_BUCKET:-true}"

    DEST="r2:${R2_BUCKET}/pg-go/${STAMP}.sql.gz"
    echo "Uploading to R2: ${DEST}..."
    rclone copyto "$OUT" "$DEST" --s3-upload-concurrency 4
    echo "R2 upload complete."
fi

echo "Backup process finished: ${OUT}"
