package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

func TestLiveKYC_ScopedDBLeaseAcquireRelease(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	propID := uuid.New()
	tenantID := uuid.New()
	inviteCode := fmt.Sprintf("K%s", uuid.New().String()[:7])
	phone := fmt.Sprintf("+9199%08d", time.Now().UnixNano()%100000000)

	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_collection_mode)
		VALUES ($1, 'ScopedDB KYC Prop', 'Test Addr', '+919999900001', 'test@upi', 'Owner', 'owner@test.com', $2, 'manual_proof')
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("failed inserting property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
		VALUES ($1, $2, 'Scoped KYC Tenant', $3, 1000000, 5, 'active')
	`, tenantID, propID, phone)
	if err != nil {
		t.Fatalf("failed inserting tenant: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_in_flight_lock WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	}()

	scopedDB := NewScopedDB(pool)
	appKYCRepo := NewKYCRepo(pool, scopedDB)

	ctxScoped := requestscope.WithPropertyID(ctx, propID)
	token := uuid.New()

	// 1. Acquire lease via ScopedDB
	acquired, err := appKYCRepo.TryAcquireInFlightLockWithToken(ctxScoped, tenantID, token, 30*time.Second)
	if err != nil {
		t.Fatalf("TryAcquireInFlightLockWithToken failed: %v", err)
	}
	if !acquired {
		t.Fatalf("expected lease acquisition to succeed")
	}

	// 2. Verify lock row exists and has token + expires_at
	var dbToken uuid.UUID
	var expiresAt time.Time
	err = pool.QueryRow(ctx, `SELECT lease_token, expires_at FROM kyc_in_flight_lock WHERE tenant_id = $1`, tenantID).Scan(&dbToken, &expiresAt)
	if err != nil {
		t.Fatalf("failed querying kyc_in_flight_lock: %v", err)
	}
	if dbToken != token {
		t.Fatalf("expected token %s, got %s", token, dbToken)
	}
	if expiresAt.Before(time.Now().UTC()) {
		t.Fatalf("expected expires_at to be in future, got %s", expiresAt)
	}

	// 3. Release lease via ScopedDB with token
	err = appKYCRepo.ReleaseInFlightLockWithToken(ctxScoped, tenantID, token)
	if err != nil {
		t.Fatalf("ReleaseInFlightLockWithToken failed: %v", err)
	}

	// 4. Verify lock row is deleted
	var remaining int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM kyc_in_flight_lock WHERE tenant_id = $1`, tenantID).Scan(&remaining)
	if err != nil {
		t.Fatalf("failed querying remaining count: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected 0 remaining locks, got %d", remaining)
	}
}

func TestLiveKYC_LeaseFencingTwoWorkers(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	propID := uuid.New()
	tenantID := uuid.New()
	inviteCode := fmt.Sprintf("K%s", uuid.New().String()[:7])
	phone := fmt.Sprintf("+9199%08d", time.Now().UnixNano()%100000000)

	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_collection_mode)
		VALUES ($1, 'Fencing Prop', 'Addr', '+919999900002', 'test@upi', 'Owner', 'owner@test.com', $2, 'manual_proof')
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("failed inserting property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
		VALUES ($1, $2, 'Fencing Tenant', $3, 1000000, 5, 'active')
	`, tenantID, propID, phone)
	if err != nil {
		t.Fatalf("failed inserting tenant: %v", err)
	}

	consentID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO kyc_consent (id, tenant_id, consent_version, purpose, consent_text, consent_text_hash, consent_given_at, ip_address, user_agent)
		VALUES ($1, $2, 'v1.0', 'residency_verification_and_kyc', 'I agree', 'mockhash64mockhash64mockhash64mockhash64mockhash64mockhash64mock', NOW(), '127.0.0.1', 'test-agent')
	`, consentID, tenantID)
	if err != nil {
		t.Fatalf("failed inserting consent: %v", err)
	}

	repo := NewKYCRepo(pool)

	verif, err := repo.InitiateVerificationTx(ctx, tenantID, consentID, domain.KYCMethodAadhaarOTP, "cashfree_otp", "cf_ref_fencing_1", "tester")
	if err != nil {
		t.Fatalf("failed initiating verification: %v", err)
	}

	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_audit_log WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_verification WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_in_flight_lock WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_consent WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	}()

	tokenA := uuid.New()
	tokenB := uuid.New()

	// 1. Worker A acquires lease with short TTL (400ms)
	acquiredA, err := repo.TryAcquireInFlightLockWithToken(ctx, tenantID, tokenA, 400*time.Millisecond)
	if err != nil || !acquiredA {
		t.Fatalf("worker A failed to acquire lease: acquired=%v, err=%v", acquiredA, err)
	}

	// 2. Worker B attempts acquisition immediately -> must return false
	acquiredB, err := repo.TryAcquireInFlightLockWithToken(ctx, tenantID, tokenB, 10*time.Second)
	if err != nil {
		t.Fatalf("worker B acquire error: %v", err)
	}
	if acquiredB {
		t.Fatalf("worker B should NOT have acquired lock while worker A's lease is active")
	}

	// 3. Wait for Worker A's lease to expire
	time.Sleep(500 * time.Millisecond)

	// 4. Worker B acquires expired lease
	acquiredB2, err := repo.TryAcquireInFlightLockWithToken(ctx, tenantID, tokenB, 10*time.Second)
	if err != nil || !acquiredB2 {
		t.Fatalf("worker B failed to acquire expired lease: acquired=%v, err=%v", acquiredB2, err)
	}

	// 5. Worker A attempts state mutation with stale tokenA -> must fail with fencing abort
	now := time.Now().UTC()
	_, err = repo.CompleteVerificationTx(ctx, verif.ID, "1234", "hash123", true, 1, nil, false, nil, now, now.AddDate(1, 0, 0), nil, "workerA", tokenA)
	if err == nil {
		t.Fatalf("expected Worker A stale mutation to FAIL, but succeeded")
	}
	if !strings.Contains(err.Error(), "stale lease token") && !strings.Contains(err.Error(), "fencing abort") {
		t.Fatalf("expected fencing abort error, got: %v", err)
	}

	// 6. Worker A attempts release with stale tokenA -> must NOT delete Worker B's lease
	err = repo.ReleaseInFlightLockWithToken(ctx, tenantID, tokenA)
	if err != nil {
		t.Fatalf("ReleaseInFlightLockWithToken error: %v", err)
	}

	var activeToken uuid.UUID
	err = pool.QueryRow(ctx, `SELECT lease_token FROM kyc_in_flight_lock WHERE tenant_id = $1`, tenantID).Scan(&activeToken)
	if err != nil {
		t.Fatalf("failed reading active lock: %v", err)
	}
	if activeToken != tokenB {
		t.Fatalf("expected Worker B token %s to remain in place, got %s", tokenB, activeToken)
	}

	// 7. Worker B completes verification with valid tokenB -> must succeed
	completed, err := repo.CompleteVerificationTx(ctx, verif.ID, "1234", "hash123", true, 1, nil, false, nil, now, now.AddDate(1, 0, 0), nil, "workerB", tokenB)
	if err != nil {
		t.Fatalf("Worker B valid mutation failed: %v", err)
	}
	if completed.Status != domain.KYCStatusVerified {
		t.Fatalf("expected status verified, got %s", completed.Status)
	}

	// 8. Worker B releases lease with tokenB -> must succeed
	err = repo.ReleaseInFlightLockWithToken(ctx, tenantID, tokenB)
	if err != nil {
		t.Fatalf("Worker B release failed: %v", err)
	}

	var remainingCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM kyc_in_flight_lock WHERE tenant_id = $1`, tenantID).Scan(&remainingCount)
	if remainingCount != 0 {
		t.Fatalf("expected 0 locks remaining, found %d", remainingCount)
	}
}
