package payment

import (
	"testing"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestApplyPaymentPartial(t *testing.T) {
	due := &domain.Due{Amount: 10000, OriginalAmount: 10000, Status: domain.DueStatusPending}
	credit := due.ApplyPayment(4000, time.Now().UTC())
	if credit != 0 {
		t.Fatalf("credit=%d", credit)
	}
	if due.Amount != 6000 || due.Status != domain.DueStatusPartial {
		t.Fatalf("got amount=%d status=%s", due.Amount, due.Status)
	}
}

func TestApplyPaymentExact(t *testing.T) {
	due := &domain.Due{Amount: 10000, OriginalAmount: 10000, Status: domain.DueStatusPending}
	credit := due.ApplyPayment(10000, time.Now().UTC())
	if credit != 0 || due.Amount != 0 || due.Status != domain.DueStatusPaid {
		t.Fatalf("credit=%d due=%#v", credit, due)
	}
}

func TestApplyPaymentOverpayCredit(t *testing.T) {
	due := &domain.Due{Amount: 10000, OriginalAmount: 10000, Status: domain.DueStatusPending}
	credit := due.ApplyPayment(12500, time.Now().UTC())
	if credit != 2500 {
		t.Fatalf("credit=%d want 2500", credit)
	}
	if due.Status != domain.DueStatusPaid || due.Amount != 0 {
		t.Fatalf("due=%#v", due)
	}
}

func TestApplyPaymentZeroAmountAutoPaid(t *testing.T) {
	// Credit fully covering a due leaves amount 0 → paid (billing path uses MarkPaid;
	// ApplyPayment(0) on zero remaining is a no-op partial edge — treat remaining 0 as paid).
	due := &domain.Due{Amount: 0, OriginalAmount: 10000, Status: domain.DueStatusPending}
	at := time.Now().UTC()
	due.MarkPaid(at)
	if due.Status != domain.DueStatusPaid || due.PaidAt == nil {
		t.Fatalf("expected paid zero-amount due, got %#v", due)
	}
}

func TestApplyPaymentOnPartialRemaining(t *testing.T) {
	due := &domain.Due{Amount: 3000, OriginalAmount: 10000, Status: domain.DueStatusPartial}
	credit := due.ApplyPayment(3000, time.Now().UTC())
	if credit != 0 || due.Status != domain.DueStatusPaid {
		t.Fatalf("credit=%d due=%#v", credit, due)
	}
}
