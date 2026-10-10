package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLivePostgres_OTPRepo_MarkUsed_AtomicRace(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 15*time.Second)
	if pool == nil {
		return
	}

	repo := NewOTPRepo(pool)
	ctx := context.Background()

	testPhone := "+919999900001"
	req := &OTPRequest{
		Phone:     testPhone,
		OTPHash:   "test_hash_concurrency_guard",
		Attempts:  0,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
		Used:      false,
		Purpose:   "login",
	}

	if err := repo.Create(ctx, req); err != nil {
		t.Fatalf("failed to create test OTP: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM otp_requests WHERE id=$1`, req.ID)
	})

	const numWorkers = 10
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	var successCount int64
	var alreadyUsedCount int64
	var unexpectedErrCount int64

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startBarrier

			err := repo.MarkUsed(ctx, req.ID)
			if err == nil {
				atomic.AddInt64(&successCount, 1)
			} else if errors.Is(err, ErrOTPAlreadyUsed) || errors.Is(err, domain.ErrOTPAlreadyUsed) {
				atomic.AddInt64(&alreadyUsedCount, 1)
			} else {
				atomic.AddInt64(&unexpectedErrCount, 1)
			}
		}()
	}

	close(startBarrier)
	wg.Wait()

	if successCount != 1 {
		t.Errorf("expected exactly 1 winner, got %d successes", successCount)
	}
	if alreadyUsedCount != numWorkers-1 {
		t.Errorf("expected %d ErrOTPAlreadyUsed, got %d", numWorkers-1, alreadyUsedCount)
	}
	if unexpectedErrCount != 0 {
		t.Errorf("unexpected errors encountered: %d", unexpectedErrCount)
	}

	// Sequential replay check: subsequent call must return ErrOTPAlreadyUsed
	err := repo.MarkUsed(ctx, req.ID)
	if !errors.Is(err, ErrOTPAlreadyUsed) && !errors.Is(err, domain.ErrOTPAlreadyUsed) {
		t.Errorf("expected ErrOTPAlreadyUsed on replay, got: %v", err)
	}
}

func TestLivePostgres_TokenRepo_MarkUsed_AtomicRace(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 15*time.Second)
	if pool == nil {
		return
	}

	repo := NewTokenRepo(pool)
	ctx := context.Background()

	// Insert test due for foreign key or insert token directly
	dueID := uuid.New()
	tok := &domain.PaymentToken{
		DueID:     dueID,
		TokenHash: "test_token_hash_concurrency_" + uuid.NewString(),
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
		Used:      false,
	}

	// payment_tokens has a foreign key to dues(id)
	// Let's create a minimal property and due or test via insert if foreign key exists
	// Check if due_id references dues:
	var dueExists bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dues WHERE id=$1)`, dueID).Scan(&dueExists)
	if !dueExists {
		// Use a transaction or create dummy property & due for test token
		var dummyPropID uuid.UUID
		err := pool.QueryRow(ctx, `
			INSERT INTO properties (name, address, upi_vpa, owner_name, payment_mode)
			VALUES ('Token Race Prop', 'Address', 'prop@upi', 'Owner', 'qr_manual')
			RETURNING id`).Scan(&dummyPropID)
		if err != nil {
			t.Logf("skipping token live test if dummy property cannot be created: %v", err)
			return
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id=$1`, dummyPropID)
		})

		err = pool.QueryRow(ctx, `
			INSERT INTO dues (property_id, tenant_id, amount, month, status)
			VALUES ($1, gen_random_uuid(), 50000, '2026-03', 'open')
			RETURNING id`, dummyPropID).Scan(&dueID)
		if err != nil {
			t.Logf("skipping token live test if dummy due cannot be created: %v", err)
			return
		}
		tok.DueID = dueID
	}

	if err := repo.Create(ctx, tok); err != nil {
		t.Fatalf("failed to create test token: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM payment_tokens WHERE id=$1`, tok.ID)
	})

	const numWorkers = 10
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	var successCount int64
	var alreadyUsedCount int64
	var unexpectedErrCount int64

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startBarrier

			err := repo.MarkUsed(ctx, tok.ID)
			if err == nil {
				atomic.AddInt64(&successCount, 1)
			} else if errors.Is(err, ErrTokenAlreadyUsed) || errors.Is(err, domain.ErrTokenAlreadyUsed) {
				atomic.AddInt64(&alreadyUsedCount, 1)
			} else {
				atomic.AddInt64(&unexpectedErrCount, 1)
			}
		}()
	}

	close(startBarrier)
	wg.Wait()

	if successCount != 1 {
		t.Errorf("expected exactly 1 winner, got %d successes", successCount)
	}
	if alreadyUsedCount != numWorkers-1 {
		t.Errorf("expected %d ErrTokenAlreadyUsed, got %d", numWorkers-1, alreadyUsedCount)
	}
	if unexpectedErrCount != 0 {
		t.Errorf("unexpected errors encountered: %d", unexpectedErrCount)
	}

	// Sequential replay check
	err := repo.MarkUsed(ctx, tok.ID)
	if !errors.Is(err, ErrTokenAlreadyUsed) && !errors.Is(err, domain.ErrTokenAlreadyUsed) {
		t.Errorf("expected ErrTokenAlreadyUsed on replay, got: %v", err)
	}
}
