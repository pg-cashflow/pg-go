package postgres

import (
	"context"
	"errors"
	"fmt"
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

	propID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()
	inviteCode := fmt.Sprintf("P%s", uuid.New().String()[:7])

	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_mode)
		VALUES ($1, 'Live Token Prop', 'Address', '+919999988888', 'prop@upi', 'Owner', 'o@test.com', $2, 'manual')
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("insert test property: %v", err)
	}
	defer func() {
		ctxClean := context.Background()
		_, _ = pool.Exec(ctxClean, `DELETE FROM payment_tokens WHERE due_id = $1`, dueID)
		_, _ = pool.Exec(ctxClean, `DELETE FROM dues WHERE id = $1`, dueID)
		_, _ = pool.Exec(ctxClean, `DELETE FROM tenants WHERE id = $1`, tenantID)
		_, _ = pool.Exec(ctxClean, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	tenantPhone := fmt.Sprintf("+9199%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Test Tenant', $3, '101', 550000, 5, 'active')
	`, tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("insert test tenant: %v", err)
	}

	dueCode := uuid.New().String()[:8]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $2, $3, $4, 550000, 550000, 'pending', CURRENT_DATE, 'rent', CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month')
	`, dueID, dueCode, propID, tenantID)
	if err != nil {
		t.Fatalf("insert test due: %v", err)
	}

	tok := &domain.PaymentToken{
		DueID:     dueID,
		TokenHash: "test_token_hash_concurrency_" + uuid.NewString(),
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
		Used:      false,
	}

	if err := repo.Create(ctx, tok); err != nil {
		t.Fatalf("failed to create test token: %v", err)
	}

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
	err = repo.MarkUsed(ctx, tok.ID)
	if !errors.Is(err, ErrTokenAlreadyUsed) && !errors.Is(err, domain.ErrTokenAlreadyUsed) {
		t.Errorf("expected ErrTokenAlreadyUsed on replay, got: %v", err)
	}
}
