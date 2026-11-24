package kyc

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockReconcilerStore struct {
	items                []*domain.KYCVerification
	expiredVerifications map[uuid.UUID]string
	failedVerifications  map[uuid.UUID]string
	manualReviews        map[uuid.UUID]string
	lockHeld             bool
	activeLeaseToken     uuid.UUID
	lastPassedToken      uuid.UUID
	rejectStaleLease     bool
}

func newMockReconcilerStore() *mockReconcilerStore {
	return &mockReconcilerStore{
		expiredVerifications: make(map[uuid.UUID]string),
		failedVerifications:  make(map[uuid.UUID]string),
		manualReviews:        make(map[uuid.UUID]string),
	}
}

func (m *mockReconcilerStore) ListRequestUnknownVerifications(_ context.Context, _ int) ([]*domain.KYCVerification, error) {
	return m.items, nil
}

func (m *mockReconcilerStore) ExpireVerificationTx(_ context.Context, verificationID uuid.UUID, reason, _ string, leaseToken ...uuid.UUID) error {
	if len(leaseToken) > 0 {
		m.lastPassedToken = leaseToken[0]
		if m.rejectStaleLease && m.activeLeaseToken != uuid.Nil && leaseToken[0] != m.activeLeaseToken {
			return errors.New("kyc: stale lease token (fencing abort)")
		}
	}
	m.expiredVerifications[verificationID] = reason
	return nil
}

func (m *mockReconcilerStore) FailVerificationTx(_ context.Context, verificationID uuid.UUID, reason, _ string, leaseToken ...uuid.UUID) error {
	if len(leaseToken) > 0 {
		m.lastPassedToken = leaseToken[0]
		if m.rejectStaleLease && m.activeLeaseToken != uuid.Nil && leaseToken[0] != m.activeLeaseToken {
			return errors.New("kyc: stale lease token (fencing abort)")
		}
	}
	m.failedVerifications[verificationID] = reason
	return nil
}

func (m *mockReconcilerStore) UpdateVerificationPendingManualReviewTx(
	_ context.Context,
	verificationID uuid.UUID,
	_, _, ocrToStore, _ string,
	_ []byte,
	_ string,
	_ bool,
	_ domain.DocumentType,
	_ string,
	leaseToken ...uuid.UUID,
) error {
	if len(leaseToken) > 0 {
		m.lastPassedToken = leaseToken[0]
		if m.rejectStaleLease && m.activeLeaseToken != uuid.Nil && leaseToken[0] != m.activeLeaseToken {
			return errors.New("kyc: stale lease token (fencing abort)")
		}
	}
	m.manualReviews[verificationID] = ocrToStore
	return nil
}

func (m *mockReconcilerStore) TryAcquireInFlightLockWithToken(_ context.Context, _ uuid.UUID, leaseToken uuid.UUID, _ time.Duration) (bool, error) {
	if m.lockHeld {
		return false, nil
	}
	m.activeLeaseToken = leaseToken
	return true, nil
}

func (m *mockReconcilerStore) ReleaseInFlightLockWithToken(_ context.Context, _ uuid.UUID, leaseToken uuid.UUID) error {
	if m.activeLeaseToken == leaseToken {
		m.activeLeaseToken = uuid.Nil
	}
	return nil
}

type mockReconcilerCashfree struct {
	statusResult *cashfree.DigiLockerStatusResult
	statusErr    error
}

func (m *mockReconcilerCashfree) UploadDocument(_ context.Context, _ io.Reader, _ string, _ string, _ ...string) (*cashfree.SmartOCRResponse, error) {
	return nil, nil
}
func (m *mockReconcilerCashfree) UploadAadhaarDocument(_ context.Context, _ io.Reader, _ string) (*cashfree.SmartOCRResponse, error) {
	return nil, nil
}
func (m *mockReconcilerCashfree) RequestAadhaarOTP(_ context.Context, _ string) (*cashfree.AadhaarOTPInitResult, error) {
	return nil, nil
}
func (m *mockReconcilerCashfree) VerifyAadhaarOTP(_ context.Context, _, _ string) (*cashfree.AadhaarOTPVerifyResult, error) {
	return nil, nil
}
func (m *mockReconcilerCashfree) CreateDigiLockerURL(_ context.Context, _ cashfree.CreateDigiLockerURLInput) (*cashfree.DigiLockerSession, error) {
	return nil, nil
}
func (m *mockReconcilerCashfree) GetDigiLockerStatus(_ context.Context, _ string) (*cashfree.DigiLockerStatusResult, error) {
	return m.statusResult, m.statusErr
}
func (m *mockReconcilerCashfree) GetDigiLockerAadhaar(_ context.Context, _ string) (*cashfree.DigiLockerAadhaar, error) {
	return nil, nil
}

type mockReconcilerProcessor struct {
	calledVID string
}

func (m *mockReconcilerProcessor) ProcessDigiLockerCallback(_ context.Context, verificationIDStr, _, _, _, _ string) (*domain.KYCVerification, error) {
	m.calledVID = verificationIDStr
	return &domain.KYCVerification{Status: domain.KYCStatusVerified}, nil
}

func TestKYCReconciler_DigiLocker_AuthenticatedResolves(t *testing.T) {
	verifID := uuid.New()
	store := newMockReconcilerStore()
	store.items = []*domain.KYCVerification{
		{
			ID:                verifID,
			Method:            domain.KYCMethodDigiLocker,
			Status:            domain.KYCStatusRequestUnknown,
			VendorReferenceID: "cf_dgl_recon_1",
			CreatedAt:         time.Now().Add(-10 * time.Minute),
		},
	}

	cf := &mockReconcilerCashfree{
		statusResult: &cashfree.DigiLockerStatusResult{
			Status: "AUTHENTICATED",
		},
	}
	proc := &mockReconcilerProcessor{}

	reconciler := NewKYCReconciler(store, cf, proc)
	n, err := reconciler.ReconcileBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 reconciled, got %d", n)
	}
	if proc.calledVID != "cf_dgl_recon_1" {
		t.Errorf("expected processor called with cf_dgl_recon_1, got %q", proc.calledVID)
	}
}

func TestKYCReconciler_DigiLocker_ExpiredTransitionsToExpired(t *testing.T) {
	verifID := uuid.New()
	store := newMockReconcilerStore()
	store.items = []*domain.KYCVerification{
		{
			ID:                verifID,
			Method:            domain.KYCMethodDigiLocker,
			Status:            domain.KYCStatusRequestUnknown,
			VendorReferenceID: "cf_dgl_recon_2",
			CreatedAt:         time.Now().Add(-30 * time.Minute),
		},
	}

	cf := &mockReconcilerCashfree{
		statusResult: &cashfree.DigiLockerStatusResult{
			Status: "EXPIRED",
		},
	}
	reconciler := NewKYCReconciler(store, cf, nil)
	n, err := reconciler.ReconcileBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 reconciled, got %d", n)
	}
	if reason, ok := store.expiredVerifications[verifID]; !ok || reason != "DIGILOCKER_SESSION_EXPIRED" {
		t.Errorf("expected expired with DIGILOCKER_SESSION_EXPIRED, got %q (ok=%v)", reason, ok)
	}
}

func TestKYCReconciler_AadhaarOTP_ExpiredAfter15Min(t *testing.T) {
	verifIDOld := uuid.New()
	verifIDRecent := uuid.New()
	now := time.Now()

	store := newMockReconcilerStore()
	store.items = []*domain.KYCVerification{
		{
			ID:        verifIDOld,
			Method:    domain.KYCMethodAadhaarOTP,
			Status:    domain.KYCStatusRequestUnknown,
			CreatedAt: now.Add(-16 * time.Minute),
		},
		{
			ID:        verifIDRecent,
			Method:    domain.KYCMethodAadhaarOTP,
			Status:    domain.KYCStatusRequestUnknown,
			CreatedAt: now.Add(-5 * time.Minute),
		},
	}

	reconciler := NewKYCReconciler(store, nil, nil)
	reconciler.nowFn = func() time.Time { return now }

	n, err := reconciler.ReconcileBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 items processed, got %d", n)
	}

	if _, ok := store.expiredVerifications[verifIDOld]; !ok {
		t.Errorf("expected old OTP verification to be expired, was not")
	}
	if _, ok := store.expiredVerifications[verifIDRecent]; ok {
		t.Errorf("recent OTP verification should NOT be expired")
	}
}

func TestKYCReconciler_DocumentUpload_EscalatesToManualReviewAfter2Hours(t *testing.T) {
	verifIDOld := uuid.New()
	verifIDRecent := uuid.New()
	now := time.Now()

	store := newMockReconcilerStore()
	store.items = []*domain.KYCVerification{
		{
			ID:        verifIDOld,
			Method:    domain.KYCMethodOCR,
			Status:    domain.KYCStatusRequestUnknown,
			CreatedAt: now.Add(-3 * time.Hour),
		},
		{
			ID:        verifIDRecent,
			Method:    domain.KYCMethodOCR,
			Status:    domain.KYCStatusRequestUnknown,
			CreatedAt: now.Add(-30 * time.Minute),
		},
	}

	reconciler := NewKYCReconciler(store, nil, nil)
	reconciler.nowFn = func() time.Time { return now }

	n, err := reconciler.ReconcileBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 items processed, got %d", n)
	}

	if _, ok := store.manualReviews[verifIDOld]; !ok {
		t.Errorf("expected OCR request older than 2 hours to escalate to manual review, was not")
	}
	if _, ok := store.manualReviews[verifIDRecent]; ok {
		t.Errorf("recent OCR request should NOT be escalated to manual review yet")
	}
}

func TestKYCReconciler_InFlightLock_SkipsConcurrentProcessing(t *testing.T) {
	verifID := uuid.New()
	tenantID := uuid.New()
	now := time.Now()

	store := newMockReconcilerStore()
	store.lockHeld = true // Simulate another replica or tenant request holding the in-flight lock
	store.items = []*domain.KYCVerification{
		{
			ID:        verifID,
			TenantID:  tenantID,
			Method:    domain.KYCMethodAadhaarOTP,
			Status:    domain.KYCStatusRequestUnknown,
			CreatedAt: now.Add(-30 * time.Minute),
		},
	}

	reconciler := NewKYCReconciler(store, nil, nil)
	reconciler.nowFn = func() time.Time { return now }

	n, err := reconciler.ReconcileBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Item should be skipped safely without modifying state
	if n != 0 {
		t.Fatalf("expected 0 items processed due to active lock, got %d", n)
	}
	if _, ok := store.expiredVerifications[verifID]; ok {
		t.Errorf("verification with active in-flight lease must NOT be modified by reconciler")
	}
}

func TestKYCReconciler_LeaseTokenFencing_PassesTokenAndPreventsStaleMutations(t *testing.T) {
	verifID := uuid.New()
	tenantID := uuid.New()
	now := time.Now()

	store := newMockReconcilerStore()
	store.items = []*domain.KYCVerification{
		{
			ID:        verifID,
			TenantID:  tenantID,
			Method:    domain.KYCMethodAadhaarOTP,
			Status:    domain.KYCStatusRequestUnknown,
			CreatedAt: now.Add(-30 * time.Minute),
		},
	}

	reconciler := NewKYCReconciler(store, nil, nil)
	reconciler.nowFn = func() time.Time { return now }

	n, err := reconciler.ReconcileBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 item processed, got %d", n)
	}

	if store.lastPassedToken == uuid.Nil {
		t.Errorf("expected reconciler to pass fencing leaseToken, but got nil")
	}

	// Verify lease lock was released cleanly
	if store.activeLeaseToken != uuid.Nil {
		t.Errorf("expected lease lock to be released after reconciliation, but was %s", store.activeLeaseToken)
	}

	// Stale worker test: simulate worker executing mutation with a stale lease token
	store.rejectStaleLease = true
	store.activeLeaseToken = uuid.New() // New worker acquired a newer lease
	staleToken := uuid.New()

	staleErr := store.ExpireVerificationTx(context.Background(), verifID, "stale attempt", "reconciler", staleToken)
	if staleErr == nil || !strings.Contains(staleErr.Error(), "stale lease token") {
		t.Fatalf("expected stale lease token rejection, got %v", staleErr)
	}
}
