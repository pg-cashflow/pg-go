package finance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockMailer struct {
	to      string
	subject string
	body    string
	err     error
}

func (m *mockMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	m.to = to
	m.subject = subject
	m.body = htmlBody
	return m.err
}

type mockSMSGateway struct {
	phone   string
	message string
	err     error
}

func (s *mockSMSGateway) Send(ctx context.Context, phone, message string) error {
	s.phone = phone
	s.message = message
	return s.err
}

func TestEmailDeadLetterNotifier_Send(t *testing.T) {
	ctx := context.Background()
	mm := &mockMailer{}
	notifier := NewEmailDeadLetterNotifier(mm, "ops-admin@pgcashflow.com")

	propID := uuid.New()
	sourceID := uuid.New()
	evt := &domain.LedgerOutboxEvent{
		ID:             101,
		EventType:      "departure_settlement_mirror",
		PropertyID:     propID,
		SourceID:       sourceID,
		IdempotencyKey: "departure_settlement:" + sourceID.String(),
		MaxAttempts:    5,
		CreatedAt:      time.Now().UTC(),
	}

	err := notifier.NotifyDeadLetter(ctx, evt, "unbalanced journal entry: debits 500 != credits 600")
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}

	if mm.to != "ops-admin@pgcashflow.com" {
		t.Errorf("expected to 'ops-admin@pgcashflow.com', got '%s'", mm.to)
	}

	if !strings.Contains(mm.subject, "101") || !strings.Contains(mm.subject, "Dead-Lettered") {
		t.Errorf("expected subject to contain event id and 'Dead-Lettered', got '%s'", mm.subject)
	}

	if !strings.Contains(mm.body, "unbalanced journal entry") {
		t.Errorf("expected body to contain error message, got '%s'", mm.body)
	}

	if !strings.Contains(mm.body, propID.String()) {
		t.Errorf("expected body to contain property ID, got '%s'", mm.body)
	}
}

func TestEmailDeadLetterNotifier_ErrorsAndNil(t *testing.T) {
	ctx := context.Background()

	// Nil receiver
	var nilNotifier *EmailDeadLetterNotifier
	if err := nilNotifier.NotifyDeadLetter(ctx, &domain.LedgerOutboxEvent{}, "err"); err == nil {
		t.Errorf("expected error on nil receiver, got nil")
	}

	// Nil mailer
	n1 := NewEmailDeadLetterNotifier(nil, "admin@example.com")
	if err := n1.NotifyDeadLetter(ctx, &domain.LedgerOutboxEvent{}, "err"); err == nil {
		t.Errorf("expected error on nil mailer, got nil")
	}

	// Empty admin email
	mm := &mockMailer{}
	n2 := NewEmailDeadLetterNotifier(mm, "")
	if err := n2.NotifyDeadLetter(ctx, &domain.LedgerOutboxEvent{}, "err"); err == nil {
		t.Errorf("expected error on empty admin email, got nil")
	}

	// Mailer send failure bubbles up
	sendErr := errors.New("smtp timeout")
	mm.err = sendErr
	n3 := NewEmailDeadLetterNotifier(mm, "admin@example.com")
	if err := n3.NotifyDeadLetter(ctx, &domain.LedgerOutboxEvent{ID: 1}, "err"); !errors.Is(err, sendErr) {
		t.Errorf("expected sendErr to bubble up, got %v", err)
	}
}

func TestEmailDeadLetterNotifier_WithSMSBackstop(t *testing.T) {
	ctx := context.Background()
	mm := &mockMailer{}
	ms := &mockSMSGateway{}
	notifier := NewEmailDeadLetterNotifier(mm, "ops-admin@pgcashflow.com").
		WithSMSBackstop(ms, "+919876543210")

	evt := &domain.LedgerOutboxEvent{
		ID:          202,
		EventType:   "departure_settlement_mirror",
		MaxAttempts: 5,
	}

	err := notifier.NotifyDeadLetter(ctx, evt, "test error")
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}

	if mm.to != "ops-admin@pgcashflow.com" {
		t.Errorf("expected email to 'ops-admin@pgcashflow.com', got '%s'", mm.to)
	}
	if ms.phone != "+919876543210" {
		t.Errorf("expected sms to '+919876543210', got '%s'", ms.phone)
	}
	if !strings.Contains(ms.message, "202") || !strings.Contains(ms.message, "departure_settlement_mirror") {
		t.Errorf("expected sms message to contain event info, got '%s'", ms.message)
	}

	// SMS failure bubbles up via errors.Join
	smsErr := errors.New("sms gateway connection timeout")
	ms.err = smsErr
	err = notifier.NotifyDeadLetter(ctx, evt, "test error")
	if err == nil || !strings.Contains(err.Error(), "sms backstop send") {
		t.Errorf("expected sms error to bubble up, got: %v", err)
	}
}
