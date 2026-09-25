package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/jobs"
)

type fakeReconcileRepo struct {
	pending   []domain.KYCVerification
	failedIDs map[uuid.UUID]string
}

func (f *fakeReconcileRepo) ListPendingDigiLockerVerifications(_ context.Context) ([]domain.KYCVerification, error) {
	return f.pending, nil
}

func (f *fakeReconcileRepo) FailVerificationTx(_ context.Context, id uuid.UUID, reason, _ string) error {
	if f.failedIDs == nil {
		f.failedIDs = make(map[uuid.UUID]string)
	}
	f.failedIDs[id] = reason
	return nil
}

type fakeStatusChecker struct {
	statuses map[string]*cashfree.DigiLockerStatus
}

func (f *fakeStatusChecker) GetDigiLockerStatus(_ context.Context, id string) (*cashfree.DigiLockerStatus, error) {
	if f.statuses == nil {
		return nil, nil
	}
	return f.statuses[id], nil
}

type fakeCompleter struct {
	completed map[string]string
}

func (f *fakeCompleter) ProcessDigiLockerCompletion(_ context.Context, vendorRefID, failedReason, _ string) error {
	if f.completed == nil {
		f.completed = make(map[string]string)
	}
	f.completed[vendorRefID] = failedReason
	return nil
}

func TestDigiLockerReconcileJob_Sweep(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	// Session 1: 2 minutes old -> skipped (< 5 min)
	vYoung := domain.KYCVerification{
		ID:                uuid.New(),
		VendorReferenceID: "ref-young",
		Method:            domain.KYCMethodDigiLocker,
		Status:            domain.KYCStatusPending,
		CreatedAt:         now.Add(-2 * time.Minute),
	}

	// Session 2: 15 minutes old, Completed -> active checked and completed
	vCompleted := domain.KYCVerification{
		ID:                uuid.New(),
		VendorReferenceID: "ref-completed",
		Method:            domain.KYCMethodDigiLocker,
		Status:            domain.KYCStatusPending,
		CreatedAt:         now.Add(-15 * time.Minute),
	}

	// Session 3: 20 minutes old, Failed -> active checked and failed
	vFailed := domain.KYCVerification{
		ID:                uuid.New(),
		VendorReferenceID: "ref-failed",
		Method:            domain.KYCMethodDigiLocker,
		Status:            domain.KYCStatusPending,
		CreatedAt:         now.Add(-20 * time.Minute),
	}

	// Session 4: 75 minutes old -> force failed (> 60 min)
	vExpired := domain.KYCVerification{
		ID:                uuid.New(),
		VendorReferenceID: "ref-expired",
		Method:            domain.KYCMethodDigiLocker,
		Status:            domain.KYCStatusPending,
		CreatedAt:         now.Add(-75 * time.Minute),
	}

	repo := &fakeReconcileRepo{
		pending: []domain.KYCVerification{vYoung, vCompleted, vFailed, vExpired},
	}
	checker := &fakeStatusChecker{
		statuses: map[string]*cashfree.DigiLockerStatus{
			"ref-completed": {Status: "COMPLETED"},
			"ref-failed":    {Status: "FAILED", Message: "user rejected"},
		},
	}
	completer := &fakeCompleter{}

	job := &jobs.DigiLockerReconcileJob{
		Repo:      repo,
		Cashfree:  checker,
		Completer: completer,
		Now:       func() time.Time { return now },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("job.Run failed: %v", err)
	}

	// Verify vExpired force-failed with consent_window_expired
	if reason, ok := repo.failedIDs[vExpired.ID]; !ok || reason != "consent_window_expired" {
		t.Errorf("expected vExpired to be force-failed with consent_window_expired, got %v", reason)
	}

	// Verify vYoung was neither failed nor completed
	if _, ok := repo.failedIDs[vYoung.ID]; ok {
		t.Errorf("vYoung should not be failed")
	}
	if _, ok := completer.completed[vYoung.VendorReferenceID]; ok {
		t.Errorf("vYoung should not be completed")
	}

	// Verify vCompleted was processed as completed
	if reason, ok := completer.completed[vCompleted.VendorReferenceID]; !ok || reason != "" {
		t.Errorf("expected vCompleted to be completed with empty reason, got %v", reason)
	}

	// Verify vFailed was processed as failed with message
	if reason, ok := completer.completed[vFailed.VendorReferenceID]; !ok || reason != "user rejected" {
		t.Errorf("expected vFailed to be completed with 'user rejected', got %v", reason)
	}
}
