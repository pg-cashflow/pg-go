package finance

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrDuplicateSettlement   = errors.New("finance: settlement already processed")
	ErrInvalidSettlementData = errors.New("finance: invalid settlement record")
	ErrSettlementUnbalanced  = errors.New("finance: settlement gross does not equal net plus charges")
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
	PaymentCount     int       `json:"payment_count"`
}

// SettlementImportResult summarizes the outcome of importing a batch of settlements.
type SettlementImportResult struct {
	TotalProcessed int   `json:"total_processed"`
	TotalSucceeded int   `json:"total_succeeded"`
	TotalSkipped   int   `json:"total_skipped"`
	NetBankPaise   int64 `json:"net_bank_paise"`
	FeeExpensePaise int64 `json:"fee_expense_paise"`
	GrossClearingPaise int64 `json:"gross_clearing_paise"`
}

// ProcessSettlement posts the balanced settlement journal entry:
// Dr bank (net settlement transferred to owner)
// Dr payment_processing_expense (service fee + service tax / MDR + GST)
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
	expectedGross := rec.NetAmountPaise + rec.ServiceFeePaise + rec.ServiceTaxPaise
	if rec.GrossAmountPaise != expectedGross {
		return fmt.Errorf("%w: gross %d != net %d + fee %d + tax %d",
			ErrSettlementUnbalanced, rec.GrossAmountPaise, rec.NetAmountPaise, rec.ServiceFeePaise, rec.ServiceTaxPaise)
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
		"gross_paise":   rec.GrossAmountPaise,
	})

	return nil
}

// ParseAndImportSettlementCSV parses Cashfree settlement CSV export and posts balanced journal entries.
func (s *Service) ParseAndImportSettlementCSV(ctx context.Context, propertyID uuid.UUID, r io.Reader) (*SettlementImportResult, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true

	headers, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read csv headers: %w", err)
	}

	colMap := make(map[string]int)
	for i, h := range headers {
		colMap[strings.ToLower(strings.TrimSpace(h))] = i
	}

	requiredCols := []string{"settlement_id", "settlement_amount", "gross_amount"}
	for _, req := range requiredCols {
		if _, ok := colMap[req]; !ok {
			return nil, fmt.Errorf("csv missing required column: %s", req)
		}
	}

	result := &SettlementImportResult{}

	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read csv row: %w", err)
		}

		settlementID := strings.TrimSpace(row[colMap["settlement_id"]])
		if settlementID == "" {
			continue
		}

		netPaise, err := parseRupeeOrPaise(row[colMap["settlement_amount"]])
		if err != nil {
			return nil, fmt.Errorf("parse settlement_amount for %s: %w", settlementID, err)
		}

		grossPaise, err := parseRupeeOrPaise(row[colMap["gross_amount"]])
		if err != nil {
			return nil, fmt.Errorf("parse gross_amount for %s: %w", settlementID, err)
		}

		var feePaise, taxPaise int64
		if idx, ok := colMap["service_charge"]; ok {
			feePaise, _ = parseRupeeOrPaise(row[idx])
		}
		if idx, ok := colMap["service_tax"]; ok {
			taxPaise, _ = parseRupeeOrPaise(row[idx])
		}

		var utr string
		if idx, ok := colMap["transfer_utr"]; ok {
			utr = strings.TrimSpace(row[idx])
		}

		var transferTime time.Time
		if idx, ok := colMap["transfer_time"]; ok {
			tStr := strings.TrimSpace(row[idx])
			if tStr != "" {
				if parsed, err := time.Parse(time.RFC3339, tStr); err == nil {
					transferTime = parsed
				} else if parsed, err := time.Parse("2006-01-02 15:04:05", tStr); err == nil {
					transferTime = parsed
				}
			}
		}

		rec := SettlementRecord{
			SettlementID:     settlementID,
			TransferUTR:      utr,
			TransferTime:     transferTime,
			GrossAmountPaise: grossPaise,
			NetAmountPaise:   netPaise,
			ServiceFeePaise:  feePaise,
			ServiceTaxPaise:  taxPaise,
		}

		result.TotalProcessed++

		err = s.ProcessSettlement(ctx, propertyID, rec)
		if errors.Is(err, ErrDuplicateSettlement) {
			result.TotalSkipped++
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("process settlement %s: %w", settlementID, err)
		}

		result.TotalSucceeded++
		result.NetBankPaise += netPaise
		result.FeeExpensePaise += (feePaise + taxPaise)
		result.GrossClearingPaise += grossPaise
	}

	return result, nil
}

func parseRupeeOrPaise(val string) (int64, error) {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0, nil
	}
	if strings.Contains(val, ".") {
		parts := strings.Split(val, ".")
		rupees, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, err
		}
		dec := parts[1]
		if len(dec) == 1 {
			dec += "0"
		} else if len(dec) > 2 {
			dec = dec[:2]
		}
		paise, err := strconv.ParseInt(dec, 10, 64)
		if err != nil {
			return 0, err
		}
		if rupees < 0 {
			return rupees*100 - paise, nil
		}
		return rupees*100 + paise, nil
	}
	return strconv.ParseInt(val, 10, 64)
}
