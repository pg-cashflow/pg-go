package finance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestPaymentMirror_DeterministicLineIDs(t *testing.T) {
	propID := uuid.New()
	sourceID := uuid.New()
	now := time.Now().UTC()

	specs := []LineSpec{
		{Account: domain.AcctGatewayClearing, Debit: 1500000, LineKind: "cash_in"},
		{Account: domain.AcctRentRevenue, Credit: 1000000, LineKind: "rent_collected"},
		{Account: domain.AcctDepositLiability, Credit: 500000, LineKind: "deposit_liability"},
	}

	lines1, err := MakeLines(propID, sourceID, "payment", now, specs)
	if err != nil {
		t.Fatalf("MakeLines 1 failed: %v", err)
	}

	// Re-run with identical parameters simulating worker retry
	lines2, err := MakeLines(propID, sourceID, "payment", now, specs)
	if err != nil {
		t.Fatalf("MakeLines 2 failed: %v", err)
	}

	if len(lines1) != len(lines2) {
		t.Fatalf("length mismatch: %d vs %d", len(lines1), len(lines2))
	}

	for i := range lines1 {
		if lines1[i].ID != lines2[i].ID {
			t.Errorf("line %d: expected deterministic UUID %s, got %s", i, lines1[i].ID, lines2[i].ID)
		}
	}

	// Verify distinct lines within the same journal entry have distinct IDs
	if lines1[0].ID == lines1[1].ID || lines1[1].ID == lines1[2].ID {
		t.Errorf("expected different line indices to have distinct UUIDs: %v", lines1)
	}

	// Verify a different payment source ID generates completely distinct IDs
	diffSourceID := uuid.New()
	linesDiff, err := MakeLines(propID, diffSourceID, "payment", now, specs)
	if err != nil {
		t.Fatalf("MakeLines diff failed: %v", err)
	}
	for i := range lines1 {
		if lines1[i].ID == linesDiff[i].ID {
			t.Errorf("different source ID must yield different line ID for index %d", i)
		}
	}
}

func TestLedgerOutboxWorker_PaymentMirror_Allocations(t *testing.T) {
	mirror := &mockMirrorer{}
	worker := &LedgerOutboxWorker{mirrorer: mirror}

	propID := uuid.New()
	paymentID := uuid.New()
	now := time.Now().UTC()

	payload := domain.PaymentMirrorPayload{
		PropertyID: propID,
		PaymentID:  paymentID,
		Allocations: []domain.PaymentAllocationItemPayload{
			{AmountPaise: 800000, DueKind: string(domain.DueKindRent)},
			{AmountPaise: 200000, DueKind: string(domain.DueKindElectricity)},
		},
		UnappliedPaise: 50000,
		IsUnapplied:    false,
		MatchedAt:      now,
		MatchedBy:      domain.MatchedByCashfree,
		AmountPaise:    1050000,
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	evt := &domain.LedgerOutboxEvent{
		EventType:      "payment_mirror",
		PropertyID:     propID,
		SourceID:       paymentID,
		Payload:        rawPayload,
		IdempotencyKey: "payment_mirror:" + paymentID.String(),
		MaxAttempts:    5,
	}

	if err := worker.dispatch(context.Background(), evt); err != nil {
		t.Fatalf("dispatch payment_mirror: %v", err)
	}

	if len(mirror.paymentCalls) != 1 {
		t.Fatalf("expected 1 payment call, got %d", len(mirror.paymentCalls))
	}
	call := mirror.paymentCalls[0]
	if call.PaymentID != paymentID {
		t.Errorf("expected paymentID %s, got %s", paymentID, call.PaymentID)
	}
	if call.PropertyID != propID {
		t.Errorf("expected propertyID %s, got %s", propID, call.PropertyID)
	}
	if call.AmountPaise != 1050000 {
		t.Errorf("expected amount 1050000, got %d", call.AmountPaise)
	}
	if call.UnappliedPaise != 50000 {
		t.Errorf("expected unapplied 50000, got %d", call.UnappliedPaise)
	}
	if len(call.Allocations) != 2 {
		t.Fatalf("expected 2 allocations, got %d", len(call.Allocations))
	}
	if call.Allocations[0].AmountPaise != 800000 || call.Allocations[0].DueKind != string(domain.DueKindRent) {
		t.Errorf("unexpected allocation 0: %+v", call.Allocations[0])
	}
	if call.Allocations[1].AmountPaise != 200000 || call.Allocations[1].DueKind != string(domain.DueKindElectricity) {
		t.Errorf("unexpected allocation 1: %+v", call.Allocations[1])
	}
}

func TestLedgerOutboxWorker_PaymentMirror_Unapplied(t *testing.T) {
	mirror := &mockMirrorer{}
	worker := &LedgerOutboxWorker{mirrorer: mirror}

	propID := uuid.New()
	paymentID := uuid.New()
	now := time.Now().UTC()

	payload := domain.PaymentMirrorPayload{
		PropertyID:  propID,
		PaymentID:   paymentID,
		IsUnapplied: true,
		MatchedAt:   now,
		MatchedBy:   domain.MatchedByCashfree,
		AmountPaise: 500000,
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	evt := &domain.LedgerOutboxEvent{
		EventType:      "payment_mirror",
		PropertyID:     propID,
		SourceID:       paymentID,
		Payload:        rawPayload,
		IdempotencyKey: "payment_mirror:" + paymentID.String(),
		MaxAttempts:    5,
	}

	if err := worker.dispatch(context.Background(), evt); err != nil {
		t.Fatalf("dispatch unapplied payment_mirror: %v", err)
	}

	if len(mirror.unappliedCalls) != 1 {
		t.Fatalf("expected 1 unapplied call, got %d", len(mirror.unappliedCalls))
	}
	call := mirror.unappliedCalls[0]
	if call.PaymentID != paymentID {
		t.Errorf("expected paymentID %s, got %s", paymentID, call.PaymentID)
	}
	if call.PropertyID != propID {
		t.Errorf("expected propertyID %s, got %s", propID, call.PropertyID)
	}
	if call.AmountPaise != 500000 {
		t.Errorf("expected amount 500000, got %d", call.AmountPaise)
	}
	if !call.IsUnapplied {
		t.Errorf("expected IsUnapplied=true")
	}
}

func TestLedgerOutboxWorker_PaymentMirror_MalformedPayload(t *testing.T) {
	mirror := &mockMirrorer{}
	worker := &LedgerOutboxWorker{mirrorer: mirror}

	evt := &domain.LedgerOutboxEvent{
		EventType: "payment_mirror",
		Payload:   []byte("invalid json"),
	}

	err := worker.dispatch(context.Background(), evt)
	if err == nil {
		t.Fatalf("expected error on malformed payload, got nil")
	}
}
