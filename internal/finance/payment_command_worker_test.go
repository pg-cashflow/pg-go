package finance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubRefundDispatcher struct {
	calledWithID uuid.UUID
	err          error
}

func (s *stubRefundDispatcher) DispatchRefund(ctx context.Context, refundID uuid.UUID) error {
	s.calledWithID = refundID
	return s.err
}

func TestPaymentCommandWorker_ExecuteCommand(t *testing.T) {
	refundID := uuid.New()
	propID := uuid.New()

	payloadBytes, err := json.Marshal(domain.RefundDispatchPayload{
		PropertyID: propID,
		RefundID:   refundID,
	})
	if err != nil {
		t.Fatalf("marshal payload error: %v", err)
	}

	t.Run("successful refund dispatch", func(t *testing.T) {
		dispatcher := &stubRefundDispatcher{}
		w := NewPaymentCommandWorker(nil, nil, dispatcher)

		cmd := &domain.PaymentCommand{
			ID:          1,
			CommandType: domain.PaymentCommandRefundDispatch,
			PropertyID:  propID,
			SourceID:    refundID,
			Payload:     payloadBytes,
		}

		if err := w.executeCommand(context.Background(), cmd); err != nil {
			t.Fatalf("expected nil error, got: %v", err)
		}
		if dispatcher.calledWithID != refundID {
			t.Errorf("expected dispatcher called with refund ID %s, got %s", refundID, dispatcher.calledWithID)
		}
	})

	t.Run("dispatcher error propagates", func(t *testing.T) {
		dispatcher := &stubRefundDispatcher{err: errors.New("gateway network error")}
		w := NewPaymentCommandWorker(nil, nil, dispatcher)

		cmd := &domain.PaymentCommand{
			ID:          2,
			CommandType: domain.PaymentCommandRefundDispatch,
			PropertyID:  propID,
			SourceID:    refundID,
			Payload:     payloadBytes,
		}

		if err := w.executeCommand(context.Background(), cmd); err == nil {
			t.Fatal("expected error from dispatcher, got nil")
		}
	})

	t.Run("unrecognized command type fails", func(t *testing.T) {
		w := NewPaymentCommandWorker(nil, nil, nil)
		cmd := &domain.PaymentCommand{
			ID:          3,
			CommandType: "unknown_command",
		}
		if err := w.executeCommand(context.Background(), cmd); err == nil {
			t.Fatal("expected error for unknown command type, got nil")
		}
	})
}

func TestPaymentCommandWorker_ComputeBackoff(t *testing.T) {
	w := NewPaymentCommandWorker(nil, nil, nil)

	b1 := w.computeBackoff(1)
	if b1 < 5*time.Second || b1 > 7*time.Second {
		t.Errorf("expected 5-6.25s for attempt 1, got %v", b1)
	}

	b2 := w.computeBackoff(2)
	if b2 < 10*time.Second || b2 > 13*time.Second {
		t.Errorf("expected 10-12.5s for attempt 2, got %v", b2)
	}

	b3 := w.computeBackoff(3)
	if b3 < 20*time.Second || b3 > 26*time.Second {
		t.Errorf("expected 20-25s for attempt 3, got %v", b3)
	}

	bMax := w.computeBackoff(15)
	if bMax > 900*time.Second {
		t.Errorf("expected backoff capped at 900s, got %v", bMax)
	}
}
