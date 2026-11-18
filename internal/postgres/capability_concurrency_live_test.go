package postgres

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestLivePostgres_Capability_2PL_MutationPrecedesRevocation verifies that when a manager mutation
// acquires Level 1 (users FOR SHARE) and Level 2 (manager_capabilities FOR SHARE) first,
// a concurrent revocation transaction waits, allowing the mutation to commit cleanly before revocation.
func TestLivePostgres_Capability_2PL_MutationPrecedesRevocation(t *testing.T) {
	pool, cfg := setupLiveTestPool(t, 60*time.Second)
	if pool == nil || cfg == nil {
		return
	}
	ctx := context.Background()

	if err := Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	propID := uuid.New()
	managerID := uuid.New()
	ownerID := uuid.New()
	inviteCode := fmt.Sprintf("C2PL%s", uuid.New().String()[:7])

	// 1. Seed Property
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode)
		VALUES ($1, '2PL Test PG', '123 Lock St', '+919999988888', 'owner@upi', 'Owner', 'c2pl@test.com', $2, 'manual')
		ON CONFLICT (id) DO NOTHING`, propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	// 2. Seed Users
	mPhone := fmt.Sprintf("+9191%08d", time.Now().UnixNano()%100000000)
	oPhone := fmt.Sprintf("+9192%08d", (time.Now().UnixNano()+1)%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'manager', $3), ($4, $5, 'owner', $3)`,
		managerID, mPhone, propID, ownerID, oPhone)
	if err != nil {
		t.Fatalf("insert users: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE property_id = $1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM manager_capabilities WHERE property_id = $1`, propID)
	}()

	repo := NewCapabilityRepo(pool)

	// 3. Grant capability to manager
	if err := repo.Grant(ctx, propID, managerID, "expenses.post", ownerID); err != nil {
		t.Fatalf("grant capability: %v", err)
	}

	// Channel coordination for strict 2PL interleaving
	tx1Locked := make(chan struct{})
	tx2AttemptLock := make(chan struct{})
	tx1Committed := make(chan struct{})

	var tx1Err, tx2Err error
	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 1: Manager mutation transaction (Tx 1)
	go func() {
		defer wg.Done()
		tx1Err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
			// Step 1: Assert capability under Tx (Level 1 FOR SHARE, Level 2 FOR SHARE)
			if err := AssertCapabilityTx(ctx, tx, propID, managerID, "manager", "expenses.post"); err != nil {
				return fmt.Errorf("assert capability: %w", err)
			}
			// Signal that Tx 1 has acquired Level 1 & 2 locks
			close(tx1Locked)

			// Wait for Tx 2 to begin trying to acquire Level 1 FOR UPDATE
			<-tx2AttemptLock
			time.Sleep(150 * time.Millisecond)

			// Mutation succeeds and commits
			return nil
		})
		close(tx1Committed)
	}()

	// Goroutine 2: Owner revocation transaction (Tx 2)
	go func() {
		defer wg.Done()
		<-tx1Locked

		// Inform Tx 1 that Tx 2 is now attempting to acquire Level 1 FOR UPDATE
		close(tx2AttemptLock)

		// RevokeTx acquires Level 1 users FOR UPDATE, which must block until Tx 1 commits
		tx2Err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
			return repo.RevokeTx(ctx, tx, propID, managerID, "expenses.post")
		})
	}()

	wg.Wait()

	if tx1Err != nil {
		t.Fatalf("Tx 1 (mutation) failed unexpectedly: %v", tx1Err)
	}
	if tx2Err != nil {
		t.Fatalf("Tx 2 (revocation) failed unexpectedly: %v", tx2Err)
	}

	// Verify capability is now revoked in durable state
	active, err := repo.HasActive(ctx, propID, managerID, "expenses.post")
	if err != nil {
		t.Fatalf("check active capability: %v", err)
	}
	if active {
		t.Fatalf("expected capability to be revoked after Tx 2 commit")
	}
}

// TestLivePostgres_Capability_2PL_ConcurrentRevocation_BlocksAndDenies verifies that when an owner
// revocation acquires Level 1 (users FOR UPDATE) first, a concurrent manager mutation
// blocks on Level 1 (users FOR SHARE), unblocks after revocation commit, sees revoked status,
// and fails closed with ErrCapabilityDenied.
func TestLivePostgres_Capability_2PL_ConcurrentRevocation_BlocksAndDenies(t *testing.T) {
	pool, cfg := setupLiveTestPool(t, 60*time.Second)
	if pool == nil || cfg == nil {
		return
	}
	ctx := context.Background()

	if err := Migrate(ctx, pool, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}

	propID := uuid.New()
	managerID := uuid.New()
	ownerID := uuid.New()
	inviteCode := fmt.Sprintf("CBLK%s", uuid.New().String()[:7])

	// 1. Seed Property
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode)
		VALUES ($1, '2PL Block Test PG', '456 Block Ave', '+919999988889', 'owner@upi', 'Owner', 'cblk@test.com', $2, 'manual')
		ON CONFLICT (id) DO NOTHING`, propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	// 2. Seed Users
	mPhone := fmt.Sprintf("+9193%08d", time.Now().UnixNano()%100000000)
	oPhone := fmt.Sprintf("+9194%08d", (time.Now().UnixNano()+2)%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'manager', $3), ($4, $5, 'owner', $3)`,
		managerID, mPhone, propID, ownerID, oPhone)
	if err != nil {
		t.Fatalf("insert users: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE property_id = $1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM manager_capabilities WHERE property_id = $1`, propID)
	}()

	repo := NewCapabilityRepo(pool)

	// 3. Grant capability to manager
	if err := repo.Grant(ctx, propID, managerID, "expenses.post", ownerID); err != nil {
		t.Fatalf("grant capability: %v", err)
	}

	tx2Locked := make(chan struct{})
	tx1AttemptLock := make(chan struct{})

	var tx1Err, tx2Err error
	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine 2: Owner revocation transaction (Tx 2) acquiring Level 1 FOR UPDATE first
	go func() {
		defer wg.Done()
		tx2Err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
			// Acquire Level 1 lock FOR UPDATE
			if err := repo.RevokeTx(ctx, tx, propID, managerID, "expenses.post"); err != nil {
				return err
			}
			// Signal that Level 1 lock is held
			close(tx2Locked)

			// Wait for Tx 1 to attempt lock and block
			<-tx1AttemptLock
			time.Sleep(150 * time.Millisecond)

			// Commit revocation
			return nil
		})
	}()

	// Goroutine 1: Manager mutation transaction (Tx 1) attempting Level 1 FOR SHARE
	go func() {
		defer wg.Done()
		<-tx2Locked

		close(tx1AttemptLock)

		// AssertCapabilityTx attempts users FOR SHARE -> blocks on Tx 2
		tx1Err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
			return AssertCapabilityTx(ctx, tx, propID, managerID, "manager", "expenses.post")
		})
	}()

	wg.Wait()

	if tx2Err != nil {
		t.Fatalf("Tx 2 (revocation) failed: %v", tx2Err)
	}

	// Invariant: Tx 1 MUST fail with ErrCapabilityDenied after unblocking
	if !errors.Is(tx1Err, ErrCapabilityDenied) {
		t.Fatalf("expected Tx 1 to fail with ErrCapabilityDenied, got: %v", tx1Err)
	}
}
