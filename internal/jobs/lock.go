package jobs

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AcquireJobLock acquires a session-level PostgreSQL advisory lock for a named background job.
// Returns a release function, a boolean indicating whether the lock was acquired, and an error.
// If another instance currently holds the lock, acquired is false and the caller should exit cleanly.
func AcquireJobLock(ctx context.Context, pool *pgxpool.Pool, jobName string) (func(context.Context) error, bool, error) {
	if pool == nil {
		return func(context.Context) error { return nil }, true, nil
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("job_lock:" + jobName))
	lockID := int64(h.Sum64())

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire conn for job lock %s: %w", jobName, err)
	}

	var acquired bool
	err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockID).Scan(&acquired)
	if err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("query advisory lock %s: %w", jobName, err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	release := func(relCtx context.Context) error {
		defer conn.Release()
		var unlocked bool
		return conn.QueryRow(relCtx, "SELECT pg_advisory_unlock($1)", lockID).Scan(&unlocked)
	}
	return release, true, nil
}
