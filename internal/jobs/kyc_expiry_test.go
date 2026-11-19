package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockKYCExpiryRepo struct {
	lapsedCount  int64
	pendingCount int64
	manualCount  int64
	lapsedErr    error
	pendingErr   error
	manualErr    error
	cutoffPassed time.Time
	manualCutoff time.Time
	nowPassed    time.Time

	consentExpiredCount int64
	consentExpiredErr   error
	consentCutoffPassed time.Time

	pendingDGLList []domain.KYCVerification
	pendingDGLErr  error
	minAgePassed   time.Time
	maxAgePassed   time.Time
}

func (m *mockKYCExpiryRepo) ExpireLapsedVerifications(ctx context.Context, now time.Time) (int64, error) {
	m.nowPassed = now
	return m.lapsedCount, m.lapsedErr
}

func (m *mockKYCExpiryRepo) ExpireStalePendingVerifications(ctx context.Context, cutoff time.Time) (int64, error) {
	m.cutoffPassed = cutoff
	return m.pendingCount, m.pendingErr
}

func (m *mockKYCExpiryRepo) ExpireStaleManualReviews(ctx context.Context, cutoff time.Time) (int64, error) {
	m.manualCutoff = cutoff
	return m.manualCount, m.manualErr
}

func (m *mockKYCExpiryRepo) ExpireConsentWindowPendingDigiLocker(ctx context.Context, cutoff time.Time) (int64, error) {
	m.consentCutoffPassed = cutoff
	return m.consentExpiredCount, m.consentExpiredErr
}

func (m *mockKYCExpiryRepo) ListPendingDigiLockerForReconciliation(ctx context.Context, minAgeCutoff, maxAgeCutoff time.Time) ([]domain.KYCVerification, error) {
	m.minAgePassed = minAgeCutoff
	m.maxAgePassed = maxAgeCutoff
	return m.pendingDGLList, m.pendingDGLErr
}

type mockDigiLockerPoller struct {
	pollCount int
	polledIDs []uuid.UUID
}

func (p *mockDigiLockerPoller) PollDigiLockerStatus(ctx context.Context, tenantID uuid.UUID, actor string) (*domain.KYCVerification, error) {
	p.pollCount++
	p.polledIDs = append(p.polledIDs, tenantID)
	return &domain.KYCVerification{TenantID: tenantID, Status: domain.KYCStatusVerified}, nil
}

func TestKYCExpiryJob_Run(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	ttl := 1 * time.Hour

	mockRepo := &mockKYCExpiryRepo{
		lapsedCount:  3,
		pendingCount: 5,
		manualCount:  2,
	}

	job := NewKYCExpiryJob(mockRepo)
	err := job.Run(context.Background(), now, ttl)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	expectedCutoff := now.Add(-ttl)
	if !mockRepo.cutoffPassed.Equal(expectedCutoff) {
		t.Errorf("cutoff = %v, want %v", mockRepo.cutoffPassed, expectedCutoff)
	}
	expectedManualCutoff := now.Add(-48 * time.Hour)
	if !mockRepo.manualCutoff.Equal(expectedManualCutoff) {
		t.Errorf("manualCutoff = %v, want %v", mockRepo.manualCutoff, expectedManualCutoff)
	}
	if !mockRepo.nowPassed.Equal(now) {
		t.Errorf("now = %v, want %v", mockRepo.nowPassed, now)
	}
}

func TestKYCExpiryJob_Errors(t *testing.T) {
	now := time.Now().UTC()
	mockErr := errors.New("db error")

	// 1. Pending error
	job1 := NewKYCExpiryJob(&mockKYCExpiryRepo{pendingErr: mockErr})
	if err := job1.Run(context.Background(), now, time.Hour); err == nil {
		t.Fatal("expected error from pending sweep")
	}

	// 2. Lapsed error
	job2 := NewKYCExpiryJob(&mockKYCExpiryRepo{lapsedErr: mockErr})
	if err := job2.Run(context.Background(), now, time.Hour); err == nil {
		t.Fatal("expected error from lapsed sweep")
	}
}

func TestKYCExpiryJob_DigiLockerReconciliation(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	ttl := 24 * time.Hour

	tenant1 := uuid.New()
	tenant2 := uuid.New()

	mockRepo := &mockKYCExpiryRepo{
		consentExpiredCount: 2,
		pendingDGLList: []domain.KYCVerification{
			{ID: uuid.New(), TenantID: tenant1, Status: domain.KYCStatusPending, Method: domain.KYCMethodDigiLocker},
			{ID: uuid.New(), TenantID: tenant2, Status: domain.KYCStatusPending, Method: domain.KYCMethodDigiLocker},
		},
	}
	poller := &mockDigiLockerPoller{}

	job := NewKYCExpiryJob(mockRepo).WithPoller(poller)
	err := job.Run(context.Background(), now, ttl)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. Consent window expired cutoff should be now - 15m
	expectedConsentCutoff := now.Add(-15 * time.Minute)
	if !mockRepo.consentCutoffPassed.Equal(expectedConsentCutoff) {
		t.Errorf("consentCutoff = %v, want %v", mockRepo.consentCutoffPassed, expectedConsentCutoff)
	}

	// 2. Reconciliation window should be now - 5m to now - 15m
	expectedMinAge := now.Add(-5 * time.Minute)
	expectedMaxAge := now.Add(-15 * time.Minute)
	if !mockRepo.minAgePassed.Equal(expectedMinAge) {
		t.Errorf("minAge = %v, want %v", mockRepo.minAgePassed, expectedMinAge)
	}
	if !mockRepo.maxAgePassed.Equal(expectedMaxAge) {
		t.Errorf("maxAge = %v, want %v", mockRepo.maxAgePassed, expectedMaxAge)
	}

	// 3. Poller should have polled both tenant sessions
	if poller.pollCount != 2 {
		t.Errorf("expected 2 polls, got %d", poller.pollCount)
	}
	if len(poller.polledIDs) != 2 || poller.polledIDs[0] != tenant1 || poller.polledIDs[1] != tenant2 {
		t.Errorf("polled IDs = %v, want [%v, %v]", poller.polledIDs, tenant1, tenant2)
	}
}
