#!/usr/bin/env bash
# Daily Postgres dump → gzip → rclone upload to Cloudflare R2.
# Expected secrets/env: DATABASE_URL, R2_BUCKET, R2_ENDPOINT, R2_ACCESS_KEY_ID,
# R2_SECRET_ACCESS_KEY, and optionally RCLONE_CONFIG_R2_TYPE=s3.
set -euo pipefail

: "${DATABASE_URL:?DATABASE_URL is required}"
: "${R2_BUCKET:?R2_BUCKET is required}"
: "${R2_ENDPOINT:?R2_ENDPOINT is required}"
: "${R2_ACCESS_KEY_ID:?R2_ACCESS_KEY_ID is required}"
: "${R2_SECRET_ACCESS_KEY:?R2_SECRET_ACCESS_KEY is required}"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="/tmp/pg-go-backup-${STAMP}.sql.gz"

echo "Dumping database to ${OUT}"
pg_dump "$DATABASE_URL" | gzip -c > "$OUT"

# Configure an ephemeral rclone remote named "r2" (S3-compatible).
export RCLONE_CONFIG_R2_TYPE="${RCLONE_CONFIG_R2_TYPE:-s3}"
export RCLONE_CONFIG_R2_PROVIDER="${RCLONE_CONFIG_R2_PROVIDER:-Cloudflare}"
export RCLONE_CONFIG_R2_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
export RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY"
export RCLONE_CONFIG_R2_ENDPOINT="$R2_ENDPOINT"
export RCLONE_CONFIG_R2_ACL="${RCLONE_CONFIG_R2_ACL:-private}"
export RCLONE_CONFIG_R2_NO_CHECK_BUCKET="${RCLONE_CONFIG_R2_NO_CHECK_BUCKET:-true}"

DEST="r2:${R2_BUCKET}/pg-go/${STAMP}.sql.gz"
echo "Uploading to ${DEST}"
rclone copyto "$OUT" "$DEST" --s3-upload-concurrency 4

echo "Backup complete: ${DEST}"
rm -f "$OUT"
