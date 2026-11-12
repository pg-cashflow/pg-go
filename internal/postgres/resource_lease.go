package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ResourceLease represents an acquired distributed lease with a fencing generation counter.
type ResourceLease struct {
	ResourceType string
	ResourceID   string
	LeaseOwner   string
	LeaseToken   uuid.UUID
	Generation   int64
	ExpiresAt    time.Time
}

// ResourceLeaseManager manages distributed leases stored in PostgreSQL.
// It uses monotonic fencing generation numbers to protect against stale workers.
type ResourceLeaseManager struct {
	pool *pgxpool.Pool
}

// NewResourceLeaseManager creates a new ResourceLeaseManager.
func NewResourceLeaseManager(pool *pgxpool.Pool) *ResourceLeaseManager {
	return &ResourceLeaseManager{pool: pool}
}

// TryAcquireLease attempts to acquire or claim an expired lease for a resource.
// Returns the acquired ResourceLease and true if successful. Returns false if currently held.
func (m *ResourceLeaseManager) TryAcquireLease(ctx context.Context, resourceType, resourceID, owner string, ttl time.Duration) (*ResourceLease, bool, error) {
	if m == nil || m.pool == nil {
		return nil, false, errors.New("resource lease manager: database pool is nil")
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	token := uuid.New()
	lease := &ResourceLease{
		ResourceType: resourceType,
		ResourceID:   resourceID,
		LeaseOwner:   owner,
		LeaseToken:   token,
	}

	query := `
		INSERT INTO resource_leases (resource_type, resource_id, lease_owner, lease_token, generation, expires_at)
		VALUES ($1, $2, $3, $4, 1, NOW() + $5::interval)
		ON CONFLICT (resource_type, resource_id)
		DO UPDATE SET
			lease_owner = EXCLUDED.lease_owner,
			lease_token = EXCLUDED.lease_token,
			generation  = resource_leases.generation + 1,
			acquired_at = NOW(),
			expires_at  = NOW() + $5::interval
		WHERE resource_leases.expires_at <= NOW()
		RETURNING generation, expires_at;
	`
	ttlStr := fmt.Sprintf("%d milliseconds", ttl.Milliseconds())
	err := m.pool.QueryRow(ctx, query, resourceType, resourceID, owner, token, ttlStr).Scan(&lease.Generation, &lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("acquire resource lease (%s, %s): %w", resourceType, resourceID, err)
	}

	return lease, true, nil
}

// RenewLease extends the validity of an existing lease if the token and generation match.
func (m *ResourceLeaseManager) RenewLease(ctx context.Context, lease *ResourceLease, ttl time.Duration) (bool, error) {
	if m == nil || m.pool == nil {
		return false, errors.New("resource lease manager: database pool is nil")
	}
	if lease == nil {
		return false, errors.New("resource lease is nil")
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	ttlStr := fmt.Sprintf("%d milliseconds", ttl.Milliseconds())
	query := `
		UPDATE resource_leases
		SET expires_at = NOW() + $5::interval
		WHERE resource_type = $1
		  AND resource_id = $2
		  AND lease_token = $3
		  AND generation = $4
		  AND expires_at > NOW()
		RETURNING expires_at;
	`
	var newExpiresAt time.Time
	err := m.pool.QueryRow(ctx, query, lease.ResourceType, lease.ResourceID, lease.LeaseToken, lease.Generation, ttlStr).Scan(&newExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("renew resource lease (%s, %s): %w", lease.ResourceType, lease.ResourceID, err)
	}

	lease.ExpiresAt = newExpiresAt
	return true, nil
}

// ReleaseLease deletes the lease row if the token and generation match.
func (m *ResourceLeaseManager) ReleaseLease(ctx context.Context, lease *ResourceLease) error {
	if m == nil || m.pool == nil {
		return errors.New("resource lease manager: database pool is nil")
	}
	if lease == nil {
		return nil
	}

	query := `
		DELETE FROM resource_leases
		WHERE resource_type = $1
		  AND resource_id = $2
		  AND lease_token = $3
		  AND generation = $4;
	`
	_, err := m.pool.Exec(ctx, query, lease.ResourceType, lease.ResourceID, lease.LeaseToken, lease.Generation)
	if err != nil {
		return fmt.Errorf("release resource lease (%s, %s): %w", lease.ResourceType, lease.ResourceID, err)
	}
	return nil
}

// ValidateLeaseTx verifies that the lease is still owned and unexpired inside a transaction.
func (m *ResourceLeaseManager) ValidateLeaseTx(ctx context.Context, tx pgx.Tx, lease *ResourceLease) (bool, error) {
	if tx == nil {
		return false, errors.New("transaction is nil")
	}
	if lease == nil {
		return false, errors.New("lease is nil")
	}

	query := `
		SELECT 1 FROM resource_leases
		WHERE resource_type = $1
		  AND resource_id = $2
		  AND lease_token = $3
		  AND generation = $4
		  AND expires_at > NOW()
		FOR UPDATE;
	`
	var valid int
	err := tx.QueryRow(ctx, query, lease.ResourceType, lease.ResourceID, lease.LeaseToken, lease.Generation).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate lease tx: %w", err)
	}
	return true, nil
}
