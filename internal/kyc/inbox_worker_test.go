package kyc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockKYCInboxStore struct {
	pendingIDs           []int64
	items                map[int64]*KYCInboxItem
	verifications        map[string]*domain.KYCVerification
	appliedWebhooks      []string
	processedIDs         []int64
	failedIDs            map[int64]string
	deadLetterIDs        map[int64]string
	applyError           error
	getVerificationError error
}

func newMockKYCInboxStore() *mockKYCInboxStore {
	return &mockKYCInboxStore{
		items:         make(map[int64]*KYCInboxItem),
		verifications: make(map[string]*domain.KYCVerification),
		failedIDs:     make(map[int64]string),
		deadLetterIDs: make(map[int64]string),
	}
}

func (m *mockKYCInboxStore) ListPendingInboxIDs(ctx context.Context, limit int) ([]int64, error) {
	return m.pendingIDs, nil
}

func (m *mockKYCInboxStore) LockAndClaimInboxTx(ctx context.Context, tx pgx.Tx, id int64, lease time.Duration) (*KYCInboxItem, error) {
	item, ok := m.items[id]
	if !ok {
		return nil, nil
	}
	if item.ProcessingStatus == "processing" && item.LeaseExpiresAt != nil && item.LeaseExpiresAt.After(time.Now()) {
		return nil, nil // busy lease
	}
	item.Attempts++
	item.ProcessingStatus = "processing"
	leaseExp := time.Now().Add(lease)
	item.LeaseExpiresAt = &leaseExp
	return item, nil
}

func (m *mockKYCInboxStore) MarkInboxProcessedTx(ctx context.Context, tx pgx.Tx, id int64) error {
	item, ok := m.items[id]
	if ok {
		item.ProcessingStatus = "processed"
		now := time.Now().UTC()
		item.ProcessedAt = &now
	}
	m.processedIDs = append(m.processedIDs, id)
	return nil
}

func (m *mockKYCInboxStore) MarkInboxFailedTx(ctx context.Context, tx pgx.Tx, id int64, errStr string, nextRetry time.Time) error {
	item, ok := m.items[id]
	if ok {
		item.ProcessingStatus = "failed"
		item.ErrorMessage = &errStr
		item.NextRetryAt = &nextRetry
	}
	m.failedIDs[id] = errStr
	return nil
}

func (m *mockKYCInboxStore) MarkInboxDeadLetterTx(ctx context.Context, tx pgx.Tx, id int64, errStr string) error {
	item, ok := m.items[id]
	if ok {
		item.ProcessingStatus = "dead_letter"
		item.ErrorMessage = &errStr
		now := time.Now().UTC()
		item.ProcessedAt = &now
	}
	m.deadLetterIDs[id] = errStr
	return nil
}

func (m *mockKYCInboxStore) GetVerificationForUpdate(ctx context.Context, tx pgx.Tx, verificationID, refID string) (*domain.KYCVerification, error) {
	if m.getVerificationError != nil {
		return nil, m.getVerificationError
	}
	if v, ok := m.verifications[verificationID]; ok {
		return v, nil
	}
	if v, ok := m.verifications[refID]; ok {
		return v, nil
	}
	return nil, nil
}

func (m *mockKYCInboxStore) ApplyVerificationWebhookTx(ctx context.Context, tx pgx.Tx, v *domain.KYCVerification, webhookStatus string) error {
	if m.applyError != nil {
		return m.applyError
	}
	m.appliedWebhooks = append(m.appliedWebhooks, webhookStatus)
	if webhookStatus == "SUCCESS" || webhookStatus == "VERIFIED" {
		v.Status = domain.KYCStatusVerified
	} else if webhookStatus == "FAILED" {
		v.Status = domain.KYCStatusFailed
	}
	return nil
}

type mockKYCAlerter struct {
	deadLetters []*KYCInboxItem
}

func (a *mockKYCAlerter) NotifyKYCDeadLetter(ctx context.Context, item *KYCInboxItem, failureErr string) error {
	a.deadLetters = append(a.deadLetters, item)
	return nil
}

type mockKYCCallbackProcessor struct {
	calledWithVID       string
	calledWithRefID     string
	calledWithEventType string
	calledWithStatus    string
	calledWithActor     string
	returnErr           error
}

func (m *mockKYCCallbackProcessor) ProcessDigiLockerCallback(ctx context.Context, vID, refID, eventType, status, actor string) (*domain.KYCVerification, error) {
	m.calledWithVID = vID
	m.calledWithRefID = refID
	m.calledWithEventType = eventType
	m.calledWithStatus = status
	m.calledWithActor = actor
	return nil, m.returnErr
}

func TestKYCInboxWorker_NormalProcessing(t *testing.T) {
	store := newMockKYCInboxStore()
	vID := "cf_verif_1001"
	statusSuccess := "SUCCESS"

	store.items[1] = &KYCInboxItem{
		ID:               1,
		VerificationID:   &vID,
		Status:           &statusSuccess,
		ProcessingStatus: "received",
		Attempts:         0,
		MaxAttempts:      5,
	}
	store.verifications[vID] = &domain.KYCVerification{
		ID:     uuid.New(),
		Status: domain.KYCStatusPending,
	}

	worker := NewKYCInboxWorker(nil, store, nil)
	err := worker.ProcessSingleItem(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.items[1].ProcessingStatus != "processed" {
		t.Fatalf("expected status processed, got %s", store.items[1].ProcessingStatus)
	}
	if store.verifications[vID].Status != domain.KYCStatusVerified {
		t.Fatalf("expected verification status verified, got %s", store.verifications[vID].Status)
	}
}

func TestKYCInboxWorker_TerminalStateProtection(t *testing.T) {
	store := newMockKYCInboxStore()
	vID := "cf_verif_1002"
	statusSuccess := "SUCCESS"

	store.items[2] = &KYCInboxItem{
		ID:               2,
		VerificationID:   &vID,
		Status:           &statusSuccess,
		ProcessingStatus: "received",
		Attempts:         0,
		MaxAttempts:      5,
	}
	// Already verified
	store.verifications[vID] = &domain.KYCVerification{
		ID:     uuid.New(),
		Status: domain.KYCStatusVerified,
	}

	worker := NewKYCInboxWorker(nil, store, nil)
	err := worker.ProcessSingleItem(context.Background(), 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should mark inbox processed, but skip mutation
	if len(store.appliedWebhooks) != 0 {
		t.Fatalf("expected zero webhook mutations on terminal record, got %d", len(store.appliedWebhooks))
	}
	if store.items[2].ProcessingStatus != "processed" {
		t.Fatalf("expected inbox status processed, got %s", store.items[2].ProcessingStatus)
	}
}

func TestKYCInboxWorker_RetryWithBackoff(t *testing.T) {
	store := newMockKYCInboxStore()
	vID := "cf_verif_1003"
	statusSuccess := "SUCCESS"

	store.items[3] = &KYCInboxItem{
		ID:               3,
		VerificationID:   &vID,
		Status:           &statusSuccess,
		ProcessingStatus: "received",
		Attempts:         0,
		MaxAttempts:      5,
	}
	store.verifications[vID] = &domain.KYCVerification{
		ID:     uuid.New(),
		Status: domain.KYCStatusPending,
	}
	store.applyError = errors.New("transient database connectivity failure")

	worker := NewKYCInboxWorker(nil, store, nil)
	err := worker.ProcessSingleItem(context.Background(), 3)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	item := store.items[3]
	if item.ProcessingStatus != "failed" {
		t.Fatalf("expected status failed, got %s", item.ProcessingStatus)
	}
	if item.Attempts != 1 {
		t.Fatalf("expected attempts 1, got %d", item.Attempts)
	}
	if item.NextRetryAt == nil || item.NextRetryAt.Before(time.Now()) {
		t.Fatalf("expected future next_retry_at, got %v", item.NextRetryAt)
	}
}

func TestKYCInboxWorker_DeadLetterAfterExhaustion(t *testing.T) {
	store := newMockKYCInboxStore()
	vID := "cf_verif_1004"
	statusSuccess := "SUCCESS"

	store.items[4] = &KYCInboxItem{
		ID:               4,
		VerificationID:   &vID,
		Status:           &statusSuccess,
		ProcessingStatus: "failed",
		Attempts:         4, // next attempt reaches max_attempts 5
		MaxAttempts:      5,
	}
	store.verifications[vID] = &domain.KYCVerification{
		ID:     uuid.New(),
		Status: domain.KYCStatusPending,
	}
	store.applyError = errors.New("permanent schema constraint violation")

	alerter := &mockKYCAlerter{}
	worker := NewKYCInboxWorker(nil, store, nil, alerter)

	err := worker.ProcessSingleItem(context.Background(), 4)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	item := store.items[4]
	if item.ProcessingStatus != "dead_letter" {
		t.Fatalf("expected status dead_letter, got %s", item.ProcessingStatus)
	}
	if len(alerter.deadLetters) != 1 {
		t.Fatalf("expected 1 dead-letter alert, got %d", len(alerter.deadLetters))
	}
}

func TestKYCInboxWorker_DigiLockerDelegation(t *testing.T) {
	store := newMockKYCInboxStore()
	vID := "cf_dgl_1005"
	refID := "ref_1005"
	eventType := "DIGILOCKER_VERIFICATION"
	statusSuccess := "SUCCESS"

	store.items[5] = &KYCInboxItem{
		ID:               5,
		VerificationID:   &vID,
		ReferenceID:      &refID,
		EventType:        &eventType,
		Status:           &statusSuccess,
		ProcessingStatus: "received",
		Attempts:         0,
		MaxAttempts:      5,
	}
	store.verifications[vID] = &domain.KYCVerification{
		ID:     uuid.New(),
		Method: domain.KYCMethodDigiLocker,
		Status: domain.KYCStatusPending,
	}

	processor := &mockKYCCallbackProcessor{}
	worker := NewKYCInboxWorker(nil, store, processor)

	err := worker.ProcessSingleItem(context.Background(), 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if processor.calledWithVID != vID {
		t.Errorf("expected vID %s, got %s", vID, processor.calledWithVID)
	}
	if processor.calledWithRefID != refID {
		t.Errorf("expected refID %s, got %s", refID, processor.calledWithRefID)
	}
	if processor.calledWithEventType != eventType {
		t.Errorf("expected eventType %s, got %s", eventType, processor.calledWithEventType)
	}
	if processor.calledWithStatus != statusSuccess {
		t.Errorf("expected status %s, got %s", statusSuccess, processor.calledWithStatus)
	}
	if processor.calledWithActor != "cashfree:inbox_worker" {
		t.Errorf("expected actor cashfree:inbox_worker, got %s", processor.calledWithActor)
	}

	// Raw ApplyVerificationWebhookTx should NOT be invoked
	if len(store.appliedWebhooks) != 0 {
		t.Fatalf("expected 0 direct store apply mutations, got %d", len(store.appliedWebhooks))
	}
	if store.items[5].ProcessingStatus != "processed" {
		t.Fatalf("expected inbox status processed, got %s", store.items[5].ProcessingStatus)
	}
}

func TestKYCInboxWorker_OCRNeverAutoVerifies(t *testing.T) {
	store := newMockKYCInboxStore()
	vID := "cf_ocr_1006"
	refID := "cf_ocr_ref_1006"
	eventType := "OCR_VERIFICATION"
	statusSuccess := "SUCCESS"

	store.items[6] = &KYCInboxItem{
		ID:               6,
		VerificationID:   &vID,
		ReferenceID:      &refID,
		EventType:        &eventType,
		Status:           &statusSuccess,
		Payload:          []byte(`{"fraud_checks":{"is_forged":false,"is_screenshot":false,"is_photo_of_screen":false}}`),
		ProcessingStatus: "received",
		Attempts:         0,
		MaxAttempts:      5,
	}
	store.verifications[vID] = &domain.KYCVerification{
		ID:     uuid.New(),
		Method: domain.KYCMethodOCR,
		Status: domain.KYCStatusPendingManualReview,
	}

	processor := &mockKYCCallbackProcessor{}
	worker := NewKYCInboxWorker(nil, store, processor)

	err := worker.ProcessSingleItem(context.Background(), 6)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// DigiLocker processor should NOT be called
	if processor.calledWithVID != "" {
		t.Errorf("expected processor not called for OCR, got called with vID %s", processor.calledWithVID)
	}

	// Store apply should NOT be called - OCR must never auto-verify
	if len(store.appliedWebhooks) != 0 {
		t.Fatalf("expected 0 direct store apply mutations for OCR, got %d", len(store.appliedWebhooks))
	}

	// Verification remains in pending_manual_review
	if store.verifications[vID].Status != domain.KYCStatusPendingManualReview {
		t.Fatalf("expected status %s, got %s", domain.KYCStatusPendingManualReview, store.verifications[vID].Status)
	}

	// Inbox item should be marked processed
	if store.items[6].ProcessingStatus != "processed" {
		t.Fatalf("expected inbox status processed, got %s", store.items[6].ProcessingStatus)
	}
}
