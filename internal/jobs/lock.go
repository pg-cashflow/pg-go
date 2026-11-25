package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

const (
	defaultJobLeaseTTL      = 10 * time.Minute
	defaultJobRenewInterval = 3 * time.Minute
)

// AcquireJobLock acquires a distributed resource lease for a named background job
// with automatic background lease renewal while the job is running.
// Returns a release function, a boolean indicating whether the lock was acquired, and an error.
// If another instance currently holds the lease, acquired is false and the caller should exit cleanly.
func AcquireJobLock(ctx context.Context, pool *pgxpool.Pool, jobName string) (func(context.Context) error, bool, error) {
	_, release, acquired, err := AcquireJobLockFenced(ctx, pool, jobName)
	return release, acquired, err
}

// AcquireJobLockFenced acquires a distributed resource lease for a named background job,
// launches a heartbeat renewal goroutine, and returns a derived context that is canceled
// immediately if the lease is lost or cannot be renewed.
func AcquireJobLockFenced(ctx context.Context, pool *pgxpool.Pool, jobName string) (context.Context, func(context.Context) error, bool, error) {
	if pool == nil {
		return nil, nil, false, errors.New("job lock: database pool is not configured")
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "worker"
	}
	owner := fmt.Sprintf("%s:%d", hostname, os.Getpid())

	manager := postgres.NewResourceLeaseManager(pool)
	lease, acquired, err := manager.TryAcquireLease(ctx, "job", jobName, owner, defaultJobLeaseTTL)
	if err != nil {
		return nil, nil, false, fmt.Errorf("acquire job lease %s: %w", jobName, err)
	}
	if !acquired {
		return nil, nil, false, nil
	}

	jobCtx, cancelJob := context.WithCancel(ctx)
	hbCtx, stopHB := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(defaultJobRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, cancelRenew := context.WithTimeout(hbCtx, 10*time.Second)
				renewed, renewErr := manager.RenewLease(renewCtx, lease, defaultJobLeaseTTL)
				cancelRenew()
				if renewErr != nil || !renewed {
					slog.Error("job lock: lost or failed to renew distributed lease; canceling job context",
						"job", jobName, "generation", lease.Generation, "renewed", renewed, "error", renewErr)
					cancelJob()
					return
				}
			}
		}
	}()

	var releaseOnce sync.Once
	var releaseErr error
	release := func(relCtx context.Context) error {
		releaseOnce.Do(func() {
			stopHB()
			wg.Wait()
			cancelJob()
			releaseErr = manager.ReleaseLease(relCtx, lease)
		})
		return releaseErr
	}
	return jobCtx, release, true, nil
}
