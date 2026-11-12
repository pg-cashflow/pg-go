package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// AcquireJobLock acquires a distributed resource lease for a named background job.
// Returns a release function, a boolean indicating whether the lock was acquired, and an error.
// If another instance currently holds the lease, acquired is false and the caller should exit cleanly.
func AcquireJobLock(ctx context.Context, pool *pgxpool.Pool, jobName string) (func(context.Context) error, bool, error) {
	if pool == nil {
		return nil, false, errors.New("job lock: database pool is not configured")
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "worker"
	}
	owner := fmt.Sprintf("%s:%d", hostname, os.Getpid())

	manager := postgres.NewResourceLeaseManager(pool)
	lease, acquired, err := manager.TryAcquireLease(ctx, "job", jobName, owner, 10*time.Minute)
	if err != nil {
		return nil, false, fmt.Errorf("acquire job lease %s: %w", jobName, err)
	}
	if !acquired {
		return nil, false, nil
	}

	release := func(relCtx context.Context) error {
		return manager.ReleaseLease(relCtx, lease)
	}
	return release, true, nil
}
