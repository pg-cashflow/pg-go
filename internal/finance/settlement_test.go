package finance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestSettlementImportAndJournalBalance(t *testing.T) {
	st := NewMemoryStore()
	svc := NewService(st, nil)
	ctx := context.Background()
	pid := uuid.New()

	// 1. Simulate initial rent payment via Cashfree: ₹5,500
	// Dr gateway_clearing ₹5,500, Cr rent_revenue ₹5,500
	at := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	paymentID := uuid.New()
	lines, err := MakeLines(pid, paymentID, "payment", at, []LineSpec{
		{Account: domain.AcctGatewayClearing, Debit: 550000, LineKind: "cash_in"},
		{Account: domain.AcctRentRevenue, Credit: 550000, LineKind: "rent_collected"},
	})
	if err != nil {
		t.Fatalf("make payment lines: %v", err)
	}
	if err := st.InsertJournal(ctx, lines); err != nil {
		t.Fatalf("insert payment lines: %v", err)
	}

	// Verify clearing drift report shows un-settled balance
	driftRep, err := svc.GetClearingDriftReport(ctx, pid)
	if err != nil {
		t.Fatalf("clearing drift report: %v", err)
	}
	if driftRep.NetUnclearedPaise != 550000 {
		t.Fatalf("expected net uncleared 550000, got %d", driftRep.NetUnclearedPaise)
	}

	// 2. Process Cashfree Settlement via ProcessSettlement:
	// Gross: ₹5,500.00 (550000 paise)
	// MDR Service Charge: ₹80.00 (8000 paise)
	// GST on MDR: ₹14.40 (1440 paise)
	// Net Settled to Bank: ₹5,405.60 (540560 paise)
	rec := SettlementRecord{
		SettlementID:     "SETTLE_001",
		TransferUTR:      "UTR12345678",
		TransferTime:     time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
		GrossAmountPaise: 550000,
		NetAmountPaise:   540560,
		ServiceFeePaise:  8000,
		ServiceTaxPaise:  1440,
		AdjustmentPaise:  0,
	}
	if err := svc.ProcessSettlement(ctx, pid, rec); err != nil {
		t.Fatalf("process settlement: %v", err)
	}

	// Verify clearing account is fully cleared (0 remaining)
	driftRep2, err := svc.GetClearingDriftReport(ctx, pid)
	if err != nil {
		t.Fatalf("clearing drift report 2: %v", err)
	}
	if driftRep2.NetUnclearedPaise != 0 {
		t.Fatalf("expected net uncleared 0 after settlement, got %d", driftRep2.NetUnclearedPaise)
	}

	// Verify Bank balance received net settlement
	bankDr, bankCr, _ := st.SumAccount(ctx, pid, domain.AcctBank, time.Time{}, time.Now().Add(24*time.Hour))
	if bankDr-bankCr != 540560 {
		t.Fatalf("expected bank balance 540560, got %d", bankDr-bankCr)
	}

	// Verify Processing Expense recorded
	feeDr, _, _ := st.SumAccount(ctx, pid, domain.AcctPaymentProcessingExpense, time.Time{}, time.Now().Add(24*time.Hour))
	if feeDr != 9440 {
		t.Fatalf("expected fee expense 9440, got %d", feeDr)
	}

	// 3. Test Idempotency Replay (Processing identical settlement returns ErrDuplicateSettlement)
	if err := svc.ProcessSettlement(ctx, pid, rec); err != ErrDuplicateSettlement {
		t.Fatalf("expected ErrDuplicateSettlement, got: %v", err)
	}

	// 4. Test Settlement with Positive Adjustment (Netted refund)
	recWithAdj := SettlementRecord{
		SettlementID:     "SETTLE_002",
		TransferUTR:      "UTR87654321",
		TransferTime:     time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
		GrossAmountPaise: 500000,
		NetAmountPaise:   390560,
		ServiceFeePaise:  8000,
		ServiceTaxPaise:  1440,
		AdjustmentPaise:  100000, // ₹1,000 refund deducted from settlement
	}
	if err := svc.ProcessSettlement(ctx, pid, recWithAdj); err != nil {
		t.Fatalf("process settlement with adjustment: %v", err)
	}
	adjDr, _, _ := st.SumAccount(ctx, pid, domain.AcctGatewayAdjustment, time.Time{}, time.Now().Add(24*time.Hour))
	if adjDr != 100000 {
		t.Fatalf("expected gateway adjustment Dr 100000, got %d", adjDr)
	}

	// 5. Test Unbalanced Settlement Rejection
	badRec := SettlementRecord{
		SettlementID:     "SETTLE_BAD",
		GrossAmountPaise: 550000,
		NetAmountPaise:   500000,
		ServiceFeePaise:  1000,
		ServiceTaxPaise:  0,
		AdjustmentPaise:  0,
	}
	if err := svc.ProcessSettlement(ctx, pid, badRec); err == nil {
		t.Fatalf("expected unbalanced settlement to fail, got nil err")
	}

	// 6. Test CSV Stub returns ErrSettlementCSVNotImplemented
	_, err = svc.ParseAndImportSettlementCSV(ctx, pid, strings.NewReader("dummy"))
	if err != ErrSettlementCSVNotImplemented {
		t.Fatalf("expected ErrSettlementCSVNotImplemented, got: %v", err)
	}
}

func TestDepositReserveAndCapitalPaybackReports(t *testing.T) {
	st := NewMemoryStore()
	svc := NewService(st, nil)
	ctx := context.Background()
	pid := uuid.New()
	owner := uuid.New()

	// Initial capital: ₹1,00,000 (10,000,000 paise)
	_, err := svc.AddCapital(ctx, pid, owner, domain.CapitalInitial, 10000000, "Initial Seed", "cap-seed-1")
	if err != nil {
		t.Fatalf("add capital: %v", err)
	}

	// Tenant deposit collected: ₹15,000 (1,500,000 paise)
	// Dr bank ₹15,000, Cr deposit_liability ₹15,000
	at := time.Now().UTC()
	depLines, _ := MakeLines(pid, uuid.New(), "payment", at, []LineSpec{
		{Account: domain.AcctBank, Debit: 1500000, LineKind: "cash_in"},
		{Account: domain.AcctDepositLiability, Credit: 1500000, LineKind: "deposit_liability"},
	})
	_ = st.InsertJournal(ctx, depLines)

	// Check Deposit Reserve Report -> Bank has 1,00,000 + 15,000 = 1,15,000 >= 15,000 (No deficit)
	resRep, err := svc.GetDepositReserveReport(ctx, pid)
	if err != nil {
		t.Fatalf("deposit reserve report: %v", err)
	}
	if resRep.IsDeficit || resRep.TotalDepositLiabilityPaise != 1500000 || resRep.ReserveSurplusDeficitPaise <= 0 {
		t.Fatalf("unexpected reserve report: %+v", resRep)
	}

	// Capital Payback Report: Invested ₹1,00,000, OCF ₹0
	payRep, err := svc.GetCapitalPaybackReport(ctx, pid)
	if err != nil {
		t.Fatalf("capital payback report: %v", err)
	}
	if payRep.IsBreakevenAchieved || payRep.UnrecoveredCapitalPaise != 10000000 {
		t.Fatalf("unexpected payback report before revenue: %+v", payRep)
	}

	// Rent revenue collected: ₹1,20,000 (12,000,000 paise)
	rentLines, _ := MakeLines(pid, uuid.New(), "payment", at, []LineSpec{
		{Account: domain.AcctBank, Debit: 12000000, LineKind: "cash_in"},
		{Account: domain.AcctRentRevenue, Credit: 12000000, LineKind: "rent_collected"},
	})
	_ = st.InsertJournal(ctx, rentLines)

	// Capital Payback Report after rent revenue: Cumulative OCF (120k) >= Net Deployed (100k) -> Breakeven Achieved!
	payRep2, err := svc.GetCapitalPaybackReport(ctx, pid)
	if err != nil {
		t.Fatalf("capital payback report 2: %v", err)
	}
	if !payRep2.IsBreakevenAchieved || payRep2.UnrecoveredCapitalPaise != 0 {
		t.Fatalf("expected breakeven achieved, got: %+v", payRep2)
	}
}

func TestSubBusinessDays(t *testing.T) {
	// Wednesday 2026-09-30 -> subtracting 3 business days:
	// Tuesday (1), Monday (2), Friday 2026-09-25 (3) - skips Saturday & Sunday
	wed := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	got := SubBusinessDays(wed, 3)
	expectedFri := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if !got.Equal(expectedFri) {
		t.Fatalf("expected 3 business days before Wednesday to be Friday %v, got %v", expectedFri, got)
	}

	// Monday 2026-09-28 -> subtracting 1 business day:
	// Should be Friday 2026-09-25 (skips Sunday and Saturday)
	mon := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	gotMon := SubBusinessDays(mon, 1)
	if !gotMon.Equal(expectedFri) {
		t.Fatalf("expected 1 business day before Monday to be Friday %v, got %v", expectedFri, gotMon)
	}
}
