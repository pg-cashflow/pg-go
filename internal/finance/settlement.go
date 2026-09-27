package finance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrDuplicateSettlement         = errors.New("finance: settlement already processed")
	ErrInvalidSettlementData       = errors.New("finance: invalid settlement record")
	ErrSettlementUnbalanced        = errors.New("finance: settlement gross does not equal net plus charges")
	ErrSettlementCSVNotImplemented = errors.New("finance: settlement csv import awaiting cashfree report schema grounding")
	ErrSettlementNotFound          = errors.New("finance: settlement not found")
)

// SettlementRecord represents an individual settlement line item from Cashfree.
type SettlementRecord struct {
	SettlementID     string    `json:"settlement_id"`
	TransferUTR      string    `json:"transfer_utr"`
	TransferTime     time.Time `json:"transfer_time"`
	GrossAmountPaise int64     `json:"gross_amount_paise"`
	NetAmountPaise   int64     `json:"net_amount_paise"`
	ServiceFeePaise  int64     `json:"service_fee_paise"`
	ServiceTaxPaise  int64     `json:"service_tax_paise"`
	AdjustmentPaise  int64     `json:"adjustment_paise"`
	PaymentCount     int       `json:"payment_count"`
}

// SettlementImportResult summarizes the outcome of importing a batch of settlements.
type SettlementImportResult struct {
	TotalProcessed     int   `json:"total_processed"`
	TotalSucceeded     int   `json:"total_succeeded"`
	TotalSkipped       int   `json:"total_skipped"`
	NetBankPaise       int64 `json:"net_bank_paise"`
	FeeExpensePaise    int64 `json:"fee_expense_paise"`
	GrossClearingPaise int64 `json:"gross_clearing_paise"`
}

// ProcessSettlement posts the balanced settlement journal entry:
// Dr bank (net settlement transferred to owner)
// Dr payment_processing_expense (service fee + service tax / MDR + GST)
// Dr/Cr gateway_adjustment (netted refunds/chargebacks/disputes)
// Cr gateway_clearing (gross funds collected from tenant)
func (s *Service) ProcessSettlement(ctx context.Context, propertyID uuid.UUID, rec SettlementRecord) error {
	if s == nil || s.Store == nil {
		return ErrDisabled
	}
	if rec.SettlementID == "" {
		return fmt.Errorf("%w: missing settlement_id", ErrInvalidSettlementData)
	}
	if rec.GrossAmountPaise <= 0 || rec.NetAmountPaise <= 0 {
		return fmt.Errorf("%w: non-positive settlement amount", ErrInvalidSettlementData)
	}
	expectedGross := rec.NetAmountPaise + rec.ServiceFeePaise + rec.ServiceTaxPaise + rec.AdjustmentPaise
	if rec.GrossAmountPaise != expectedGross {
		return fmt.Errorf("%w: gross %d != net %d + fee %d + tax %d + adj %d",
			ErrSettlementUnbalanced, rec.GrossAmountPaise, rec.NetAmountPaise, rec.ServiceFeePaise, rec.ServiceTaxPaise, rec.AdjustmentPaise)
	}

	at := rec.TransferTime
	if at.IsZero() {
		at = s.Now()
	}

	totalFees := rec.ServiceFeePaise + rec.ServiceTaxPaise
	specs := []LineSpec{
		{Account: domain.AcctBank, Debit: rec.NetAmountPaise, LineKind: "settlement_bank_dr"},
		{Account: domain.AcctGatewayClearing, Credit: rec.GrossAmountPaise, LineKind: "settlement_clearing_cr"},
	}
	if totalFees > 0 {
		specs = append(specs, LineSpec{
			Account: domain.AcctPaymentProcessingExpense, Debit: totalFees, LineKind: "settlement_fee_dr",
		})
	}
	if rec.AdjustmentPaise > 0 {
		specs = append(specs, LineSpec{
			Account: domain.AcctGatewayAdjustment, Debit: rec.AdjustmentPaise, LineKind: "settlement_adjustment_dr",
		})
	} else if rec.AdjustmentPaise < 0 {
		specs = append(specs, LineSpec{
			Account: domain.AcctGatewayAdjustment, Credit: -rec.AdjustmentPaise, LineKind: "settlement_adjustment_cr",
		})
	}

	// Deterministic UUID based on settlement ID for idempotent journal entries
	sourceID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("cashfree_settlement:"+rec.SettlementID))
	lines, err := MakeLines(propertyID, sourceID, "gateway_settlement", at, specs)
	if err != nil {
		return err
	}

	err = s.Store.InsertJournal(ctx, lines)
	if errors.Is(err, ErrDuplicateIdempotency) {
		return ErrDuplicateSettlement
	}
	if err != nil {
		return fmt.Errorf("insert settlement journal: %w", err)
	}

	s.publish(ctx, propertyID, domain.EvtPaymentReconciled, map[string]any{
		"settlement_id": rec.SettlementID,
		"transfer_utr":  rec.TransferUTR,
		"net_paise":     rec.NetAmountPaise,
		"fees_paise":    totalFees,
		"adj_paise":     rec.AdjustmentPaise,
		"gross_paise":   rec.GrossAmountPaise,
	})

	return nil
}

// ParseAndImportSettlementCSV is a held stub awaiting real Cashfree merchant dashboard report grounding.
// Ingestion is primarily driven via verified webhooks and order-level API polling.
func (s *Service) ParseAndImportSettlementCSV(ctx context.Context, propertyID uuid.UUID, r io.Reader) (*SettlementImportResult, error) {
	return nil, ErrSettlementCSVNotImplemented
}
