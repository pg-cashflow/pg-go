package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

type mockKYCExpiryRepo struct {
	lapsedCount  int64
	pendingCount int64
	lapsedErr    error
	pendingErr   error
	cutoffPassed time.Time
	nowPassed    time.Time
}

func (m *mockKYCExpiryRepo) ExpireLapsedVerifications(ctx context.Context, now time.Time) (int64, error) {
	m.nowPassed = now
	return m.lapsedCount, m.lapsedErr
}

func (m *mockKYCExpiryRepo) ExpireStalePendingVerifications(ctx context.Context, cutoff time.Time) (int64, error) {
	m.cutoffPassed = cutoff
	return m.pendingCount, m.pendingErr
}

func TestKYCExpiryJob_Run(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	ttl := 1 * time.Hour

	mockRepo := &mockKYCExpiryRepo{
		lapsedCount:  3,
		pendingCount: 5,
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
