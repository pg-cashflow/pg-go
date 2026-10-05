package payment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type correctionStubPayments struct {
	created     []*domain.Payment
	byTxn       map[string]*domain.Payment
	corrections []*domain.FinancialCorrection
}

func (s *correctionStubPayments) Create(_ context.Context, p *domain.Payment) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	cp := *p
	s.created = append(s.created, &cp)
	if p.UPITxnID != nil {
		if s.byTxn == nil {
			s.byTxn = map[string]*domain.Payment{}
		}
		s.byTxn[*p.UPITxnID] = &cp
	}
	return nil
}

func (s *correctionStubPayments) GetByUPITxnID(_ context.Context, txnID string) (*domain.Payment, error) {
	if s.byTxn == nil {
		return nil, pgx.ErrNoRows
	}
	p, ok := s.byTxn[txnID]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *p
	return &cp, nil
}

func (s *correctionStubPayments) GetByID(_ context.Context, id uuid.UUID) (*domain.Payment, error) {
	for _, p := range s.created {
		if p.ID == id {
			cp := *p
			return &cp, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (s *correctionStubPayments) RecordCorrection(_ context.Context, c *domain.FinancialCorrection) error {
	s.corrections = append(s.corrections, c)
	return nil
}

func TestVerifyPayment(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()
	recorder := uuid.New()

	due := &domain.Due{
		ID:             dueID,
		PropertyID:     propID,
		TenantID:       tenantID,
		DueCode:        "DUE-01",
		Amount:         500000,
		OriginalAmount: 500000,
		Status:         domain.DueStatusPending,
		DueDate:        time.Now().UTC(),
	}

	dues := &stubDues{byID: map[uuid.UUID]*domain.Due{dueID: due}}
	pays := &correctionStubPayments{}
	tenants := &stubTenants{byID: map[uuid.UUID]*domain.Tenant{tenantID: {ID: tenantID, PropertyID: propID}}}
	pub := &recordingPublisher{}
	svc := NewService(dues, pays, tenants, nil, pub)

	t.Run("zero or negative amount rejected", func(t *testing.T) {
		_, err := svc.VerifyPayment(ctx, VerifyPaymentInput{
			PropertyID:  propID,
			DueID:       dueID,
			AmountPaise: 0,
			UTR:         "UTR123456",
			RecordedBy:  recorder,
		})
		if err == nil {
			t.Fatal("expected error for zero amount")
		}
	})

	t.Run("property mismatch rejected", func(t *testing.T) {
		otherProp := uuid.New()
		_, err := svc.VerifyPayment(ctx, VerifyPaymentInput{
			PropertyID:  otherProp,
			DueID:       dueID,
			AmountPaise: 500000,
			UTR:         "UTR123456",
			RecordedBy:  recorder,
		})
		if err == nil {
			t.Fatal("expected error for property mismatch")
		}
	})

	t.Run("successful verification and UTR normalization", func(t *testing.T) {
		p, err := svc.VerifyPayment(ctx, VerifyPaymentInput{
			PropertyID:  propID,
			DueID:       dueID,
			AmountPaise: 500000,
			UTR:         "  utr123456  ",
			RecordedBy:  recorder,
			Note:        "Verified via bank statement",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p == nil {
			t.Fatal("expected payment returned")
		}
		if p.UPITxnID == nil || *p.UPITxnID != "UTR123456" {
			t.Fatalf("expected normalized UTR UTR123456, got %v", p.UPITxnID)
		}

		updatedDue, _ := dues.GetByID(ctx, dueID)
		if updatedDue.Status != domain.DueStatusPaid {
			t.Fatalf("expected due to be marked paid, got status: %s", updatedDue.Status)
		}
	})

	t.Run("idempotent replay returns existing payment", func(t *testing.T) {
		p, err := svc.VerifyPayment(ctx, VerifyPaymentInput{
			PropertyID:  propID,
			DueID:       dueID,
			AmountPaise: 500000,
			UTR:         "UTR123456",
			RecordedBy:  recorder,
		})
		if err != nil {
			t.Fatalf("unexpected error on idempotent replay: %v", err)
		}
		if p == nil {
			t.Fatal("expected existing payment returned on replay")
		}
	})

	t.Run("duplicate UTR for different amount rejected", func(t *testing.T) {
		_, err := svc.VerifyPayment(ctx, VerifyPaymentInput{
			PropertyID:  propID,
			DueID:       dueID,
			AmountPaise: 300000,
			UTR:         "UTR123456",
			RecordedBy:  recorder,
		})
		if err == nil {
			t.Fatal("expected error for duplicate UTR with mismatched transaction")
		}
	})
}

func TestCorrectPayment(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()
	paymentID := uuid.New()
	user := uuid.New()

	utr := "ORIGINAL123"
	origPayment := &domain.Payment{
		ID:         paymentID,
		DueID:      dueID,
		TenantID:   tenantID,
		Amount:     500000,
		UPITxnID:   &utr,
		MatchedBy:  domain.MatchedByManual,
		RecordedBy: &user,
	}

	due := &domain.Due{
		ID:             dueID,
		PropertyID:     propID,
		TenantID:       tenantID,
		DueCode:        "DUE-01",
		Amount:         0,
		OriginalAmount: 500000,
		Status:         domain.DueStatusPaid,
	}

	dues := &stubDues{byID: map[uuid.UUID]*domain.Due{dueID: due}}
	pays := &correctionStubPayments{
		created: []*domain.Payment{origPayment},
		byTxn:   map[string]*domain.Payment{utr: origPayment},
	}
	tenants := &stubTenants{byID: map[uuid.UUID]*domain.Tenant{tenantID: {ID: tenantID, PropertyID: propID}}}
	pub := &recordingPublisher{}
	svc := NewService(dues, pays, tenants, nil, pub)

	t.Run("requires reason", func(t *testing.T) {
		_, err := svc.CorrectPayment(ctx, CorrectPaymentInput{
			PropertyID:      propID,
			PaymentID:       paymentID,
			CorrectedAmount: 450000,
			Reason:          "",
			CorrectedBy:     user,
		})
		if err == nil {
			t.Fatal("expected error for missing reason")
		}
	})

	t.Run("successful correction creates reversal and corrected entry", func(t *testing.T) {
		corr, err := svc.CorrectPayment(ctx, CorrectPaymentInput{
			PropertyID:      propID,
			PaymentID:       paymentID,
			CorrectedAmount: 400000, // Reduced from 500,000 to 400,000 paise (leaving 100,000 due)
			CorrectedUTR:    "CORRECTED123",
			Reason:          "Bank charge was 4000, not 5000",
			CorrectedBy:     user,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if corr == nil {
			t.Fatal("expected correction record returned")
		}
		if corr.OriginalPaymentID != paymentID {
			t.Fatalf("expected original payment ID %v, got %v", paymentID, corr.OriginalPaymentID)
		}
		if len(pays.corrections) != 1 {
			t.Fatalf("expected 1 audit correction recorded, got %d", len(pays.corrections))
		}

		// Verify reversing and corrected entries exist
		if len(pays.created) != 3 { // 1 original + 1 reversal + 1 corrected
			t.Fatalf("expected 3 payments in history, got %d", len(pays.created))
		}
		reversal := pays.created[1]
		if reversal.Amount != -500000 {
			t.Fatalf("expected reversal amount -500000, got %d", reversal.Amount)
		}
		corrected := pays.created[2]
		if corrected.Amount != 400000 {
			t.Fatalf("expected corrected amount 400000, got %d", corrected.Amount)
		}

		// Verify due is now partial with 100,000 paise remaining
		updatedDue, _ := dues.GetByID(ctx, dueID)
		if updatedDue.Status != domain.DueStatusPartial || updatedDue.Amount != 100000 {
			t.Fatalf("expected due to be partial with 100000 paise remaining, got status: %s, amount: %d", updatedDue.Status, updatedDue.Amount)
		}
	})
}
