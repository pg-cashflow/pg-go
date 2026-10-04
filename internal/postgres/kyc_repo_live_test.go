package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLiveKYCRepo_LifecycleAndAuditChain(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres KYC test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres KYC test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	// Apply all project migrations using the canonical migration runner (schema_migrations)
	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := Migrate(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("failed to apply migrations: %v", err)
	}

	// Create test property and tenant
	propID := uuid.New()
	tenantID := uuid.New()

	inviteCode := fmt.Sprintf("K%s", uuid.New().String()[:7])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_collection_mode)
		VALUES ($1, 'KYC Test Property', 'Test Address', '+919999900000', 'test@upi', 'Test Owner', 'owner@test.com', $2, 'manual_proof')
		ON CONFLICT (id) DO NOTHING
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("failed to insert test property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	testPhone := fmt.Sprintf("+9199%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
		VALUES ($1, $2, 'KYC Test Tenant', $3, 1000000, 5, 'active')
	`, tenantID, propID, testPhone)
	if err != nil {
		t.Fatalf("failed to insert test tenant: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_audit_log WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_verification WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_consent WHERE tenant_id = $1`, tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	}()

	repo := NewKYCRepo(pool)

	// 1. Record Consent
	consent := &domain.KYCConsent{
		TenantID:        tenantID,
		Purpose:         "tenant_identity_verification",
		ConsentVersion:  "v1",
		ConsentText:     "I hereby consent to Aadhaar verification via DigiLocker.",
		ConsentTextHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
	if err := repo.RecordConsent(ctx, consent, "tenant"); err != nil {
		t.Fatalf("RecordConsent failed: %v", err)
	}
	if consent.ID == uuid.Nil {
		t.Fatal("expected consent ID to be populated")
	}

	// 2. First Initiate Verification
	vendorRef1 := "cf_ref_test_001"
	v1, err := repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", vendorRef1, "tenant")
	if err != nil {
		t.Fatalf("InitiateVerificationTx 1 failed: %v", err)
	}
	if v1.Status != domain.KYCStatusPending {
		t.Fatalf("expected status pending, got %s", v1.Status)
	}

	// 3. Second Initiate Verification (supersede pending)
	vendorRef2 := "cf_ref_test_002"
	v2, err := repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", vendorRef2, "tenant")
	if err != nil {
		t.Fatalf("InitiateVerificationTx 2 (supersede) failed: %v", err)
	}
	if v2.Status != domain.KYCStatusPending {
		t.Fatalf("expected status pending, got %s", v2.Status)
	}

	// Check that v1 is now expired/superseded
	v1Reloaded, err := repo.GetVerificationByVendorRefID(ctx, vendorRef1)
	if err != nil {
		t.Fatalf("failed to fetch v1: %v", err)
	}
	if v1Reloaded.Status != domain.KYCStatusExpired {
		t.Fatalf("expected v1 status to be expired, got %s", v1Reloaded.Status)
	}

	// 4. Complete Verification on v2
	verifiedAt := time.Now().UTC()
	expiresAt := verifiedAt.Add(365 * 24 * time.Hour)
	identityHash := domain.ComputeIdentityHash("test-secret", "KYC Test Tenant", "1995-01-01", "M", "9999")
	v2Completed, err := repo.CompleteVerificationTx(ctx, v2.ID, "9999", identityHash, true, 1, nil, false, nil, verifiedAt, expiresAt, "system")
	if err != nil {
		t.Fatalf("CompleteVerificationTx failed: %v", err)
	}
	if v2Completed.Status != domain.KYCStatusVerified || *v2Completed.MaskedUID != "9999" {
		t.Fatalf("unexpected completed verification: %+v", v2Completed)
	}

	// Tenant profile should have aadhaar_last4 updated
	var aadhaarLast4 *string
	err = pool.QueryRow(ctx, `SELECT aadhaar_last4 FROM tenants WHERE id = $1`, tenantID).Scan(&aadhaarLast4)
	if err != nil || aadhaarLast4 == nil || *aadhaarLast4 != "9999" {
		t.Fatalf("expected tenant aadhaar_last4 to be 9999, got %v", aadhaarLast4)
	}

	// 5. Initiating again while active and verified should fail with ErrAlreadyVerified
	_, err = repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", "cf_ref_test_003", "tenant")
	if err != domain.ErrAlreadyVerified {
		t.Fatalf("expected ErrAlreadyVerified, got %v", err)
	}

	// 6. Test Audit Chain Monotonicity & prev_hash continuity
	logs, err := repo.ListAuditLogsByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListAuditLogsByTenant failed: %v", err)
	}

	// Expected audit sequence:
	// 1. consent_given
	// 2. verification_initiated (v1)
	// 3. verification_superseded (v1)
	// 4. verification_initiated (v2)
	// 5. verification_completed (v2)
	if len(logs) < 5 {
		t.Fatalf("expected at least 5 audit log entries, got %d", len(logs))
	}

	var previousHash *string
	for i, log := range logs {
		// Verify monotonic IDs
		if i > 0 && log.ID <= logs[i-1].ID {
			t.Fatalf("audit log IDs not strictly increasing: log[%d]=%d <= log[%d]=%d", i, log.ID, i-1, logs[i-1].ID)
		}

		// Verify cryptographic hash chain
		if i == 0 {
			if log.PrevHash != nil {
				t.Fatalf("first audit entry should have nil prev_hash, got %v", log.PrevHash)
			}
		} else {
			if log.PrevHash == nil || previousHash == nil || *log.PrevHash != *previousHash {
				t.Fatalf("audit chain broken at entry %d (action=%s): got prev_hash %v, want %v", i, log.Action, log.PrevHash, previousHash)
			}
		}
		previousHash = log.DetailHash
	}

	// 7. Test Duplicate Collision Handling (Flag-Don't-Block)
	tenant2ID := uuid.New()
	testPhone2 := fmt.Sprintf("+9199%08d", (time.Now().UnixNano()+123)%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
		VALUES ($1, $2, 'KYC Tenant Two', $3, 1200000, 5, 'active')
	`, tenant2ID, propID, testPhone2)
	if err != nil {
		t.Fatalf("failed to insert test tenant 2: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_audit_log WHERE tenant_id = $1`, tenant2ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_verification WHERE tenant_id = $1`, tenant2ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_consent WHERE tenant_id = $1`, tenant2ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenant2ID)
	}()

	consent2 := &domain.KYCConsent{
		TenantID:        tenant2ID,
		Purpose:         "tenant_identity_verification",
		ConsentVersion:  "v1",
		ConsentText:     "Consent for tenant 2",
		ConsentTextHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
	if err := repo.RecordConsent(ctx, consent2, "tenant"); err != nil {
		t.Fatalf("RecordConsent for tenant 2 failed: %v", err)
	}

	vTenant2, err := repo.InitiateVerificationTx(ctx, tenant2ID, consent2.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", "cf_ref_test_t2", "tenant")
	if err != nil {
		t.Fatalf("InitiateVerificationTx for tenant 2 failed: %v", err)
	}

	// Complete verification for tenant 2 with the EXACT SAME identityHash as tenant 1
	vT2Completed, err := repo.CompleteVerificationTx(ctx, vTenant2.ID, "9999", identityHash, true, 1, nil, false, nil, verifiedAt, expiresAt, "system")
	if err != nil {
		t.Fatalf("CompleteVerificationTx for tenant 2 failed (should flag, not block): %v", err)
	}
	if vT2Completed.Status != domain.KYCStatusVerified {
		t.Fatalf("expected tenant 2 verification to succeed with status verified, got %s", vT2Completed.Status)
	}
	if !vT2Completed.DuplicateDetected {
		t.Fatal("expected duplicate_detected=true on collided identity verification")
	}

	// Check that tenant 2's audit log contains the duplicate_detected event
	t2Logs, err := repo.ListAuditLogsByTenant(ctx, tenant2ID)
	if err != nil {
		t.Fatalf("ListAuditLogsByTenant for tenant 2 failed: %v", err)
	}
	var foundDuplicateAudit bool
	for _, l := range t2Logs {
		if l.Action == "duplicate_detected" {
			foundDuplicateAudit = true
			break
		}
	}
	if !foundDuplicateAudit {
		t.Fatal("expected 'duplicate_detected' action in tenant 2 audit trail")
	}

	// Verify SYMMETRIC collision flagging: tenant 1's record must now ALSO be flagged duplicate_detected=true!
	v1AfterCollision, err := repo.GetVerificationByVendorRefID(ctx, vendorRef2)
	if err != nil {
		t.Fatalf("failed to fetch v2: %v", err)
	}
	if !v1AfterCollision.DuplicateDetected {
		t.Fatal("expected symmetric duplicate_detected=true on original tenant verification")
	}

	t1Logs, err := repo.ListAuditLogsByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("ListAuditLogsByTenant for tenant 1 failed: %v", err)
	}
	var foundT1DuplicateAudit bool
	for _, l := range t1Logs {
		if l.Action == "duplicate_detected" {
			foundT1DuplicateAudit = true
			break
		}
	}
	if !foundT1DuplicateAudit {
		t.Fatal("expected symmetric 'duplicate_detected' action in tenant 1 audit trail")
	}

	// 8. Test Lapsed Verification Clears Profile Aadhaar (Zero Stale PII Policy)
	// Simulate tenant 2's verification lapsing by setting expires_at in the past
	pastTime := time.Now().UTC().Add(-2 * time.Hour)
	_, err = pool.Exec(ctx, `UPDATE kyc_verification SET expires_at = $2 WHERE id = $1`, vT2Completed.ID, pastTime)
	if err != nil {
		t.Fatalf("failed to simulate lapsed verification: %v", err)
	}

	// Re-initiating for tenant 2 must notice expiry, mark old row as expired, and clear tenant 2's aadhaar_last4
	_, err = repo.InitiateVerificationTx(ctx, tenant2ID, consent2.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", "cf_ref_test_t2_rekyc", "tenant")
	if err != nil {
		t.Fatalf("re-initiating after lapse failed: %v", err)
	}

	// Tenant 2 profile aadhaar_last4 must be cleared to nil immediately
	var t2AadhaarLast4 *string
	err = pool.QueryRow(ctx, `SELECT aadhaar_last4 FROM tenants WHERE id = $1`, tenant2ID).Scan(&t2AadhaarLast4)
	if err != nil || t2AadhaarLast4 != nil {
		t.Fatalf("expected tenant 2 aadhaar_last4 to be nil after lapse re-initiation, got %v", t2AadhaarLast4)
	}

	// 9. Revoke Consent (DPDP Rule 8 compliance)
	if err := repo.RevokeConsent(ctx, tenantID, "tenant"); err != nil {
		t.Fatalf("RevokeConsent failed: %v", err)
	}

	// Verify verification record PII is scrubbed
	v2Revoked, err := repo.GetVerificationByVendorRefID(ctx, vendorRef2)
	if err != nil {
		t.Fatalf("failed to fetch revoked v2: %v", err)
	}
	if v2Revoked.Status != domain.KYCStatusRevoked {
		t.Fatalf("expected v2 status revoked, got %s", v2Revoked.Status)
	}
	if v2Revoked.MaskedUID != nil || v2Revoked.IdentityHash != nil {
		t.Fatalf("expected PII fields to be nil on revoked verification: masked=%v hash=%v", v2Revoked.MaskedUID, v2Revoked.IdentityHash)
	}

	// Verify tenant profile aadhaar_last4 is cleared
	err = pool.QueryRow(ctx, `SELECT aadhaar_last4 FROM tenants WHERE id = $1`, tenantID).Scan(&aadhaarLast4)
	if err != nil || aadhaarLast4 != nil {
		t.Fatalf("expected tenant aadhaar_last4 to be nil after revocation, got %v", aadhaarLast4)
	}
}

func TestLiveKYCRepo_ConcurrentCompleteAndSupersede(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres KYC test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres KYC test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	repo := NewKYCRepo(pool)

	// Run multiple race iterations to exercise different timing interleavings
	for i := 0; i < 5; i++ {
		func(i int) {
			propID := uuid.New()
			tenantID := uuid.New()
			inviteCode := fmt.Sprintf("C%s", uuid.New().String()[:7])

			_, err = pool.Exec(ctx, `
				INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_collection_mode)
				VALUES ($1, 'Concurrent Property', 'Test Address', '+919999900000', 'test@upi', 'Test Owner', 'owner@test.com', $2, 'manual_proof')
				ON CONFLICT (id) DO NOTHING
			`, propID, inviteCode)
			if err != nil {
				t.Fatalf("iteration %d: failed to insert property: %v", i, err)
			}

			testPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+int64(i*1000000007))%10000000000)
			_, err = pool.Exec(ctx, `
				INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
				VALUES ($1, $2, 'Concurrent Tenant', $3, 1000000, 5, 'active')
			`, tenantID, propID, testPhone)
			if err != nil {
				t.Fatalf("iteration %d: failed to insert tenant: %v", i, err)
			}

			defer func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_audit_log WHERE tenant_id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_verification WHERE tenant_id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_consent WHERE tenant_id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
			}()

			consent := &domain.KYCConsent{
				TenantID:        tenantID,
				Purpose:         "tenant_identity_verification",
				ConsentVersion:  "v1",
				ConsentText:     "Consent text",
				ConsentTextHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			}
			if err := repo.RecordConsent(ctx, consent, "tenant"); err != nil {
				t.Fatalf("iteration %d: RecordConsent failed: %v", i, err)
			}

			raceID := uuid.New().String()[:8]
			vendorRefA := fmt.Sprintf("cf_race_%s_%d_A", raceID, i)
			vA, err := repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", vendorRefA, "tenant")
			if err != nil {
				t.Fatalf("iteration %d: initial InitiateVerificationTx failed: %v", i, err)
			}

			// Barrier to synchronize start of two racing transactions
			var startBarrier sync.WaitGroup
			startBarrier.Add(1)

			var completeErr error
			var completeResult *domain.KYCVerification
			var supersedeErr error
			var supersedeResult *domain.KYCVerification

			var done sync.WaitGroup
			done.Add(2)

			// Goroutine 1: Complete verification vA
			go func() {
				defer done.Done()
				startBarrier.Wait()

				verifiedAt := time.Now().UTC()
				expiresAt := verifiedAt.Add(365 * 24 * time.Hour)
				identityHash := domain.ComputeIdentityHash("test-secret", "Concurrent Tenant", "1995-01-01", "M", "1234")
				completeResult, completeErr = repo.CompleteVerificationTx(ctx, vA.ID, "1234", identityHash, true, 1, nil, false, nil, verifiedAt, expiresAt, "webhook")
			}()

			// Goroutine 2: Supersede pending verification by initiating session B
			vendorRefB := fmt.Sprintf("cf_race_%s_%d_B", raceID, i)
			go func() {
				defer done.Done()
				startBarrier.Wait()

				supersedeResult, supersedeErr = repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", vendorRefB, "tenant")
			}()

			// Release both goroutines simultaneously
			startBarrier.Done()
			done.Wait()

			// Verify outcome:
			// Either:
			// Outcome 1: Complete won.
			//   vA was completed (completeErr == nil).
			//   supersede saw verified vA and returned ErrAlreadyVerified.
			// Outcome 2: Supersede won.
			//   supersede succeeded (supersedeErr == nil, supersedeResult.Status == pending).
			//   vA was superseded/expired, so Complete failed with ErrVerificationNotPending.
			// In NEITHER outcome should:
			//   - A deadlock error occur (pq/pgx deadlock 40P01)
			//   - Both claim to have completed/superseded vA into an inconsistent state
			t.Logf("iteration %d: completeErr=%v, supersedeErr=%v", i, completeErr, supersedeErr)

			if completeErr != nil && !errors.Is(completeErr, domain.ErrVerificationNotPending) {
				t.Fatalf("iteration %d: unexpected completeErr: %v", i, completeErr)
			}
			if supersedeErr != nil && !errors.Is(supersedeErr, domain.ErrAlreadyVerified) {
				t.Fatalf("iteration %d: unexpected supersedeErr: %v", i, supersedeErr)
			}

			if completeErr == nil {
				if completeResult == nil || completeResult.Status != domain.KYCStatusVerified {
					t.Fatalf("iteration %d: expected completeResult verified, got %+v", i, completeResult)
				}
				// Complete succeeded: vA must be verified
				reloadedVA, err := repo.GetVerificationByVendorRefID(ctx, vendorRefA)
				if err != nil {
					t.Fatalf("iteration %d: fetch vA: %v", i, err)
				}
				if reloadedVA.Status != domain.KYCStatusVerified {
					t.Fatalf("iteration %d: expected vA status verified, got %s", i, reloadedVA.Status)
				}
			} else {
				// Complete failed with ErrVerificationNotPending: supersede must have succeeded
				if supersedeErr != nil {
					t.Fatalf("iteration %d: both operations failed! completeErr=%v, supersedeErr=%v", i, completeErr, supersedeErr)
				}
				reloadedVA, err := repo.GetVerificationByVendorRefID(ctx, vendorRefA)
				if err != nil {
					t.Fatalf("iteration %d: fetch vA: %v", i, err)
				}
				if reloadedVA.Status != domain.KYCStatusExpired {
					t.Fatalf("iteration %d: expected vA status expired, got %s", i, reloadedVA.Status)
				}
				if supersedeResult == nil || supersedeResult.Status != domain.KYCStatusPending {
					t.Fatalf("iteration %d: expected session B pending, got %+v", i, supersedeResult)
				}
			}
		}(i)
	}
}

func TestLiveKYCRepo_ConcurrentDoubleComplete(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres KYC test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres KYC test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	repo := NewKYCRepo(pool)

	for i := 0; i < 3; i++ {
		func(i int) {
			propID := uuid.New()
			tenantID := uuid.New()
			inviteCode := fmt.Sprintf("D%s", uuid.New().String()[:7])

			_, err = pool.Exec(ctx, `
				INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_collection_mode)
				VALUES ($1, 'Double Complete Property', 'Test Address', '+919999900000', 'test@upi', 'Test Owner', 'owner@test.com', $2, 'manual_proof')
				ON CONFLICT (id) DO NOTHING
			`, propID, inviteCode)
			if err != nil {
				t.Fatalf("iteration %d: failed to insert property: %v", i, err)
			}

			testPhone := fmt.Sprintf("+9197%08d", (time.Now().UnixNano()+int64(i*1000))%100000000)
			_, err = pool.Exec(ctx, `
				INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
				VALUES ($1, $2, 'Double Tenant', $3, 1000000, 5, 'active')
			`, tenantID, propID, testPhone)
			if err != nil {
				t.Fatalf("iteration %d: failed to insert tenant: %v", i, err)
			}

			defer func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_audit_log WHERE tenant_id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_verification WHERE tenant_id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_consent WHERE tenant_id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
			}()

			consent := &domain.KYCConsent{
				TenantID:        tenantID,
				Purpose:         "tenant_identity_verification",
				ConsentVersion:  "v1",
				ConsentText:     "Consent text",
				ConsentTextHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			}
			if err := repo.RecordConsent(ctx, consent, "tenant"); err != nil {
				t.Fatalf("iteration %d: RecordConsent failed: %v", i, err)
			}

			raceID := uuid.New().String()[:8]
			vendorRef := fmt.Sprintf("cf_dbl_%s_%d", raceID, i)
			v, err := repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", vendorRef, "tenant")
			if err != nil {
				t.Fatalf("iteration %d: initial InitiateVerificationTx failed: %v", i, err)
			}

			var startBarrier sync.WaitGroup
			startBarrier.Add(1)

			var err1, err2 error
			var done sync.WaitGroup
			done.Add(2)

			verifiedAt := time.Now().UTC()
			expiresAt := verifiedAt.Add(365 * 24 * time.Hour)
			identityHash := domain.ComputeIdentityHash("test-secret", "Double Tenant", "1995-01-01", "M", "5678")

			// Goroutine 1: complete v
			go func() {
				defer done.Done()
				startBarrier.Wait()
				_, err1 = repo.CompleteVerificationTx(ctx, v.ID, "5678", identityHash, true, 1, nil, false, nil, verifiedAt, expiresAt, "webhook-worker-1")
			}()

			// Goroutine 2: duplicate complete v
			go func() {
				defer done.Done()
				startBarrier.Wait()
				_, err2 = repo.CompleteVerificationTx(ctx, v.ID, "5678", identityHash, true, 1, nil, false, nil, verifiedAt, expiresAt, "webhook-worker-2")
			}()

			startBarrier.Done()
			done.Wait()

			t.Logf("iteration %d: err1=%v, err2=%v", i, err1, err2)

			// Exactly one must succeed and exactly one must fail with ErrVerificationNotPending
			successCount := 0
			if err1 == nil {
				successCount++
			} else if !errors.Is(err1, domain.ErrVerificationNotPending) {
				t.Fatalf("iteration %d: unexpected err1: %v", i, err1)
			}

			if err2 == nil {
				successCount++
			} else if !errors.Is(err2, domain.ErrVerificationNotPending) {
				t.Fatalf("iteration %d: unexpected err2: %v", i, err2)
			}

			if successCount != 1 {
				t.Fatalf("iteration %d: expected exactly 1 successful completion, got %d (err1=%v, err2=%v)", i, successCount, err1, err2)
			}

			// Verify row is verified
			reloaded, err := repo.GetVerificationByVendorRefID(ctx, vendorRef)
			if err != nil || reloaded.Status != domain.KYCStatusVerified {
				t.Fatalf("iteration %d: expected verification verified, got %+v (err=%v)", i, reloaded, err)
			}

			// Verify tenant profile aadhaar_last4 was set
			var aadhaarLast4 *string
			err = pool.QueryRow(ctx, `SELECT aadhaar_last4 FROM tenants WHERE id = $1`, tenantID).Scan(&aadhaarLast4)
			if err != nil || aadhaarLast4 == nil || *aadhaarLast4 != "5678" {
				t.Fatalf("iteration %d: expected aadhaar_last4 '5678', got %v", i, aadhaarLast4)
			}
		}(i)
	}
}

func TestLiveKYCRepo_ConcurrentRevokeAndComplete(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres KYC test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres KYC test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	repo := NewKYCRepo(pool)

	for i := 0; i < 5; i++ {
		func(i int) {
			propID := uuid.New()
			tenantID := uuid.New()
			inviteCode := fmt.Sprintf("R%s", uuid.New().String()[:7])

			_, err = pool.Exec(ctx, `
				INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_collection_mode)
				VALUES ($1, 'Revoke Complete Property', 'Test Address', '+919999900000', 'test@upi', 'Test Owner', 'owner@test.com', $2, 'manual_proof')
				ON CONFLICT (id) DO NOTHING
			`, propID, inviteCode)
			if err != nil {
				t.Fatalf("iteration %d: failed to insert property: %v", i, err)
			}

			testPhone := fmt.Sprintf("+9196%08d", (time.Now().UnixNano()+int64(i*1000))%100000000)
			_, err = pool.Exec(ctx, `
				INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
				VALUES ($1, $2, 'Revoke Tenant', $3, 1000000, 5, 'active')
			`, tenantID, propID, testPhone)
			if err != nil {
				t.Fatalf("iteration %d: failed to insert tenant: %v", i, err)
			}

			defer func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_audit_log WHERE tenant_id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_verification WHERE tenant_id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM kyc_consent WHERE tenant_id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
			}()

			consent := &domain.KYCConsent{
				TenantID:        tenantID,
				Purpose:         "tenant_identity_verification",
				ConsentVersion:  "v1",
				ConsentText:     "Consent text",
				ConsentTextHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			}
			if err := repo.RecordConsent(ctx, consent, "tenant"); err != nil {
				t.Fatalf("iteration %d: RecordConsent failed: %v", i, err)
			}

			raceID := uuid.New().String()[:8]
			vendorRef := fmt.Sprintf("cf_rev_%s_%d", raceID, i)
			v, err := repo.InitiateVerificationTx(ctx, tenantID, consent.ID, domain.KYCMethodDigiLocker, "cashfree_secure_id", vendorRef, "tenant")
			if err != nil {
				t.Fatalf("iteration %d: initial InitiateVerificationTx failed: %v", i, err)
			}

			var startBarrier sync.WaitGroup
			startBarrier.Add(1)

			var completeErr error
			var revokeErr error
			var done sync.WaitGroup
			done.Add(2)

			verifiedAt := time.Now().UTC()
			expiresAt := verifiedAt.Add(365 * 24 * time.Hour)
			identityHash := domain.ComputeIdentityHash("test-secret", "Revoke Tenant", "1995-01-01", "M", "7777")

			// Goroutine 1: CompleteVerificationTx
			go func() {
				defer done.Done()
				startBarrier.Wait()
				_, completeErr = repo.CompleteVerificationTx(ctx, v.ID, "7777", identityHash, true, 1, nil, false, nil, verifiedAt, expiresAt, "webhook")
			}()

			// Goroutine 2: RevokeConsent
			go func() {
				defer done.Done()
				startBarrier.Wait()
				revokeErr = repo.RevokeConsent(ctx, tenantID, "tenant")
			}()

			startBarrier.Done()
			done.Wait()

			t.Logf("iteration %d: completeErr=%v, revokeErr=%v", i, completeErr, revokeErr)

			// RevokeConsent must ALWAYS succeed (DPDP absolute right)
			if revokeErr != nil {
				t.Fatalf("iteration %d: RevokeConsent failed: %v", i, revokeErr)
			}

			// CompleteVerificationTx either:
			// - succeeded first (nil), then RevokeConsent scrubbed it, OR
			// - failed second with ErrVerificationNotPending (because RevokeConsent ran first)
			if completeErr != nil && !errors.Is(completeErr, domain.ErrVerificationNotPending) {
				t.Fatalf("iteration %d: unexpected completeErr: %v", i, completeErr)
			}

			// In ALL cases: Final state MUST have all PII completely scrubbed
			reloaded, err := repo.GetVerificationByVendorRefID(ctx, vendorRef)
			if err != nil {
				t.Fatalf("iteration %d: fetch verification: %v", i, err)
			}
			if reloaded.Status != domain.KYCStatusRevoked {
				t.Fatalf("iteration %d: expected status 'revoked', got %s", i, reloaded.Status)
			}
			if reloaded.MaskedUID != nil || reloaded.IdentityHash != nil {
				t.Fatalf("iteration %d: PII not scrubbed on revoked record: masked=%v hash=%v", i, reloaded.MaskedUID, reloaded.IdentityHash)
			}

			var aadhaarLast4 *string
			err = pool.QueryRow(ctx, `SELECT aadhaar_last4 FROM tenants WHERE id = $1`, tenantID).Scan(&aadhaarLast4)
			if err != nil || aadhaarLast4 != nil {
				t.Fatalf("iteration %d: expected tenant aadhaar_last4 to be nil after revocation, got %v", i, aadhaarLast4)
			}
		}(i)
	}
}

func TestLivePostgres_ConcurrentMigrate(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	migrationsDir := filepath.Join("..", "..", "migrations")

	const concurrency = 5
	var startBarrier sync.WaitGroup
	startBarrier.Add(1)

	var done sync.WaitGroup
	done.Add(concurrency)

	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer done.Done()
			startBarrier.Wait()
			errs[idx] = Migrate(ctx, pool, migrationsDir)
		}(i)
	}

	startBarrier.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed Migrate: %v", i, err)
		}
	}
}

func TestLiveKYCRepo_InFlightLock_Concurrency(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres KYC in-flight lock test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres KYC in-flight lock test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	migrationsDir := filepath.Join("..", "..", "migrations")
	if err := Migrate(ctx, pool, migrationsDir); err != nil {
		t.Fatalf("failed to apply migrations: %v", err)
	}

	repo := NewKYCRepo(pool)

	propID := uuid.New()
	tenantID := uuid.New()
	inviteCode := fmt.Sprintf("L%s", uuid.New().String()[:7])

	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, payment_collection_mode)
		VALUES ($1, 'Lock Test Property', 'Test Address', '+919999900001', 'locktest@upi', 'Lock Owner', 'lockowner@test.com', $2, 'manual_proof')
		ON CONFLICT (id) DO NOTHING
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("failed to insert test property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
		VALUES ($1, $2, 'Lock Tenant', '+919999900002', 1000000, 5, 'active')
	`, tenantID, propID)
	if err != nil {
		t.Fatalf("failed to insert test tenant: %v", err)
	}
	defer func() {
		_ = repo.ReleaseInFlightLock(context.Background(), tenantID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID)
	}()

	// 1. Initial acquire succeeds
	acquired, err := repo.TryAcquireInFlightLock(ctx, tenantID, 30*time.Second)
	if err != nil {
		t.Fatalf("initial TryAcquireInFlightLock failed: %v", err)
	}
	if !acquired {
		t.Fatal("expected first acquire to succeed")
	}

	// 2. Immediate second acquire fails (lock held)
	acquired, err = repo.TryAcquireInFlightLock(ctx, tenantID, 30*time.Second)
	if err != nil {
		t.Fatalf("second TryAcquireInFlightLock failed: %v", err)
	}
	if acquired {
		t.Fatal("expected second acquire to fail while lease is active")
	}

	// 3. Concurrent simultaneous race: 10 goroutines racing to acquire lock
	_ = repo.ReleaseInFlightLock(ctx, tenantID)

	const concurrency = 10
	var startBarrier sync.WaitGroup
	startBarrier.Add(1)
	var done sync.WaitGroup
	done.Add(concurrency)

	results := make([]bool, concurrency)
	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer done.Done()
			startBarrier.Wait()
			ok, _ := repo.TryAcquireInFlightLock(ctx, tenantID, 30*time.Second)
			results[idx] = ok
		}(i)
	}
	startBarrier.Done()
	done.Wait()

	winCount := 0
	for _, ok := range results {
		if ok {
			winCount++
		}
	}
	if winCount != 1 {
		t.Fatalf("expected exactly 1 winner out of %d concurrent racers, got %d", concurrency, winCount)
	}

	// 4. Release lock allows re-acquisition
	if err := repo.ReleaseInFlightLock(ctx, tenantID); err != nil {
		t.Fatalf("ReleaseInFlightLock failed: %v", err)
	}
	acquired, err = repo.TryAcquireInFlightLock(ctx, tenantID, 30*time.Second)
	if err != nil || !acquired {
		t.Fatalf("expected acquire to succeed after release, got acquired=%v, err=%v", acquired, err)
	}

	// 5. TTL expiration: lease with 100ms TTL expires
	time.Sleep(150 * time.Millisecond)
	acquired, err = repo.TryAcquireInFlightLock(ctx, tenantID, 100*time.Millisecond)
	if err != nil || !acquired {
		t.Fatalf("expected expired lease to be overwritten and acquired, got acquired=%v, err=%v", acquired, err)
	}
}



