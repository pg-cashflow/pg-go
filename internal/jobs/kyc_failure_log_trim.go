package jobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TrimKYCFailureLog deletes kyc_failure_log entries older than 24 hours.
func TrimKYCFailureLog(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM kyc_failure_log WHERE created_at < now() - interval '24 hours'
	`)
	if err != nil {
		return 0, fmt.Errorf("trim kyc_failure_log: %w", err)
	}
	return tag.RowsAffected(), nil
}

// TrimRateLimitBuckets deletes rate_limit_bucket entries older than 1 hour.
func TrimRateLimitBuckets(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM rate_limit_bucket WHERE updated_at < now() - interval '1 hour'
	`)
	if err != nil {
		return 0, fmt.Errorf("trim rate_limit_bucket: %w", err)
	}
	return tag.RowsAffected(), nil
}
