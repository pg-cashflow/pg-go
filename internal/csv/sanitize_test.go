package csv

import (
	"strings"
	"testing"
)

func TestStripFormulaChars(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"=1+1", "1+1"},
		{"+cmd", "cmd"},
		{"-2", "2"},
		{"@SUM(A1)", "SUM(A1)"},
		{"  =HYPERLINK()", "HYPERLINK()"},
		{"=+@-safe", "safe"},
		{"normal", "normal"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := StripFormulaChars(tc.in); got != tc.want {
			t.Errorf("StripFormulaChars(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseKnownSchema(t *testing.T) {
	in := "Txn ID,Amount,Date,Narration\n" +
		"UTR123,1500.50,2026-08-01,PG-A3X9KR rent\n" +
		"=CMD,200,02-08-2026,=note\n"
	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("len=%d", len(rows))
	}
	if rows[0].TxnID != "UTR123" || rows[0].AmountPaise != 150050 {
		t.Fatalf("row0=%+v", rows[0])
	}
	if rows[1].TxnID != "CMD" || rows[1].Note != "note" {
		t.Fatalf("row1 sanitized=%+v", rows[1])
	}
}

func TestParseUnknownSchema(t *testing.T) {
	_, err := Parse(strings.NewReader("foo,bar\n1,2\n"))
	if err != ErrUnknownSchema {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

// Assumed-layout fixture for HDFC Bank CSV export
func TestParse_HDFC_AssumedLayoutFixture(t *testing.T) {
	in := "Date,Narration,Chq./Ref.No.,Value Dt,Withdrawal Amt.,Deposit Amt.,Closing Balance\n" +
		"01/09/26,UPI-RAMESH KUMAR-424512345678-HDFC0001234-PG-A3X9KR RENT,424512345678,01/09/26,,15000.00,125000.50\n" +
		"02/09/26,UPI-SURESH PATEL-424698765432-PYTM0123456-ROOM 204,424698765432,02/09/26,,12000.00,137000.50\n" +
		"03/09/26,NEFT DR-MAINTENANCE EXPENSE-N09261234567,N09261234567,03/09/26,3500.00,,133500.50\n"

	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("HDFC fixture parse failed: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}

	// Row 0: Deposit (Credit)
	if rows[0].Type != RowTypeCredit || rows[0].AmountPaise != 1500000 {
		t.Errorf("row0: expected Credit 1500000 paise, got type=%s amt=%d", rows[0].Type, rows[0].AmountPaise)
	}
	if rows[0].Date.Year() != 2026 || rows[0].Date.Month() != 9 || rows[0].Date.Day() != 1 {
		t.Errorf("row0: unexpected date %v (expected 2026-09-01)", rows[0].Date)
	}
	if rows[0].BalancePaise == nil || *rows[0].BalancePaise != 12500050 {
		t.Errorf("row0: unexpected balance %v", rows[0].BalancePaise)
	}

	// Row 1: Deposit (Credit)
	if rows[1].Type != RowTypeCredit || rows[1].AmountPaise != 1200000 {
		t.Errorf("row1: expected Credit 1200000 paise, got type=%s amt=%d", rows[1].Type, rows[1].AmountPaise)
	}

	// Row 2: Withdrawal (Debit)
	if rows[2].Type != RowTypeDebit || rows[2].AmountPaise != 350000 {
		t.Errorf("row2: expected Debit 350000 paise, got type=%s amt=%d", rows[2].Type, rows[2].AmountPaise)
	}
}

// Assumed-layout fixture for ICICI Bank CSV export
func TestParse_ICICI_AssumedLayoutFixture(t *testing.T) {
	in := "Transaction Date,Value Date,Cheque No.,Transaction Remarks,Withdrawal Amount (INR ),Deposit Amount (INR ),Balance (INR )\n" +
		"01/09/2026,01/09/2026,-,UPI/424512345678/Rent Room 102/HDFC/ramesh@upi,0.00,15000.00,150000.00\n" +
		"03/09/2026,03/09/2026,-,ATM/CASH-WDL/MUMBAI,2000.00,0.00,148000.00\n"

	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ICICI fixture parse failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Type != RowTypeCredit || rows[0].AmountPaise != 1500000 {
		t.Errorf("row0: expected Credit 1500000 paise, got type=%s amt=%d", rows[0].Type, rows[0].AmountPaise)
	}
	if rows[1].Type != RowTypeDebit || rows[1].AmountPaise != 200000 {
		t.Errorf("row1: expected Debit 200000 paise, got type=%s amt=%d", rows[1].Type, rows[1].AmountPaise)
	}
}

func TestParse_OccurrenceTracking_WhenBalanceMissing(t *testing.T) {
	// Two identical UPI deposits on same day without balance column
	in := "Txn ID,Amount,Date,Narration\n" +
		"UPI1001,5000.00,2026-09-10,UPI/Payment\n" +
		"UPI1001,5000.00,2026-09-10,UPI/Payment\n"

	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Occurrence != 1 {
		t.Errorf("expected row 0 occurrence 1, got %d", rows[0].Occurrence)
	}
	if rows[1].Occurrence != 2 {
		t.Errorf("expected row 1 occurrence 2, got %d", rows[1].Occurrence)
	}
}

func TestParse_DayFirstDateDisambiguation(t *testing.T) {
	// 05/06/2026 MUST be parsed as 5th June 2026 (Day 5, Month 6), not 6th May 2026!
	in := "Txn ID,Amount,Date,Narration\n" +
		"UTR999,1000.00,05/06/2026,Rent\n"

	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Date.Day() != 5 || rows[0].Date.Month() != 6 {
		t.Fatalf("date disambiguation failed: expected Day=5 Month=6, got Day=%d Month=%d (%v)",
			rows[0].Date.Day(), rows[0].Date.Month(), rows[0].Date)
	}
}

func TestParse_SBI_PreambleMetadata_HeaderDetection(t *testing.T) {
	// SBI export format typically begins with account information prelude before table header
	in := "Account Name: Ramesh Kumar\n" +
		"Account Number: 00000012345678901\n" +
		"Branch: KORAMANGALA BANGALORE\n" +
		"Drawing Power: 0.00\n" +
		"Interest Rate: 0.00 % p.a.\n" +
		"MOD Balance: 0.00\n" +
		"CIF No: 88990011223\n" +
		"IFS Code: SBIN0001234\n" +
		"(Amounts in INR)\n" +
		"\n" +
		"Txn Date,Value Date,Description,Ref No./Cheque No.,Debit,Credit,Balance\n" +
		"01/09/2026,01/09/2026,TRANSFER FROM RAMESH KUMAR - RENT,TRANSFER424512,,15000.00,85000.00\n" +
		"02/09/2026,02/09/2026,ATM WDL-KORAMANGALA,ATM998877,2000.00,,83000.00\n"

	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("SBI preamble parse failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Type != RowTypeCredit || rows[0].AmountPaise != 1500000 {
		t.Errorf("row0: expected Credit 1500000 paise, got type=%s amt=%d", rows[0].Type, rows[0].AmountPaise)
	}
	if rows[0].TxnID != "TRANSFER424512" {
		t.Errorf("row0: expected TxnID TRANSFER424512, got %q", rows[0].TxnID)
	}
	if rows[1].Type != RowTypeDebit || rows[1].AmountPaise != 200000 {
		t.Errorf("row1: expected Debit 200000 paise, got type=%s amt=%d", rows[1].Type, rows[1].AmountPaise)
	}
}

// Assumed-layout fixture derived from SBI PDF export
func TestParse_SBI_UnpaddedDate_And_CrSuffix_DerivedFromPDF(t *testing.T) {
	in := "Txn Date,Value Date,Description,Ref No./Cheque No.,Debit,Credit,Balance\n" +
		"1 Jun 2020,1 Jun 2020,TRANSFER FROM RAMESH KUMAR - RENT,424512345678,,15000.00,\"1,26,948.00 Cr\"\n" +
		"15 Jun 2020,15 Jun 2020,ATM CASH WDL,ATM445566,2000.00,,\"1,24,948.00 Cr\"\n"

	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	// Verify unpadded day parsed correctly (1 Jun 2020 -> Day 1, Month 6, Year 2020)
	if rows[0].Date.Day() != 1 || rows[0].Date.Month() != 6 || rows[0].Date.Year() != 2020 {
		t.Errorf("row0: unexpected date %v", rows[0].Date)
	}
	if rows[0].Type != RowTypeCredit || rows[0].AmountPaise != 1500000 {
		t.Errorf("row0: unexpected type=%s amount=%d", rows[0].Type, rows[0].AmountPaise)
	}
	// Verify integer balance parsing with Indian grouping commas and Cr suffix
	if rows[0].BalancePaise == nil || *rows[0].BalancePaise != 12694800 {
		t.Errorf("row0: expected balance 12694800 paise, got %v (deref=%d)", rows[0].BalancePaise, *rows[0].BalancePaise)
	}

	if rows[1].Date.Day() != 15 || rows[1].Date.Month() != 6 || rows[1].Date.Year() != 2020 {
		t.Errorf("row1: unexpected date %v", rows[1].Date)
	}
	if rows[1].Type != RowTypeDebit || rows[1].AmountPaise != 200000 {
		t.Errorf("row1: unexpected type=%s amount=%d", rows[1].Type, rows[1].AmountPaise)
	}
	if rows[1].BalancePaise == nil || *rows[1].BalancePaise != 12494800 {
		t.Errorf("row1: expected balance 12494800 paise, got %v (deref=%d)", rows[1].BalancePaise, *rows[1].BalancePaise)
	}
}

// Assumed-layout fixture derived from older SBI export layout
func TestParse_SBI_OlderLayout_PostDate_ChqNo(t *testing.T) {
	in := "Post Date,Details,Chq.No,Debit,Credit,Balance\n" +
		"01/09/2026,UPI-PG RENT PAYMENT,9988776655,,10000.00,50000.00 Cr\n" +
		"05/09/2026,BANK CHARGES,-,50.00,,49950.00 Cr\n"

	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("older SBI layout parse failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Type != RowTypeCredit || rows[0].AmountPaise != 1000000 {
		t.Errorf("row0: expected Credit 1000000 paise, got %s %d", rows[0].Type, rows[0].AmountPaise)
	}
	if rows[0].TxnID != "9988776655" {
		t.Errorf("row0: expected TxnID 9988776655, got %q", rows[0].TxnID)
	}
	if rows[1].Type != RowTypeDebit || rows[1].AmountPaise != 5000 {
		t.Errorf("row1: expected Debit 5000 paise, got %s %d", rows[1].Type, rows[1].AmountPaise)
	}
}

func TestParse_FooterSummaryTolerance(t *testing.T) {
	in := "Txn Date,Description,Ref No.,Debit,Credit,Balance\n" +
		"01/09/2026,RENT PAYMENT,REF101,,15000.00,100000.00\n" +
		"02/09/2026,WATER BILL,REF102,1200.00,,98800.00\n" +
		"** This is a computer generated statement and does not require signature **\n" +
		"Statement Summary: Total Debits: 1200.00, Total Credits: 15000.00\n" +
		"End of Statement\n"

	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse failed with footer rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected exactly 2 data rows, got %d", len(rows))
	}
	if rows[0].AmountPaise != 1500000 || rows[1].AmountPaise != 120000 {
		t.Errorf("rows parsed incorrectly: %+v", rows)
	}
}

func TestParse_ValidDateBadAmount_FailClosed(t *testing.T) {
	// A row with a valid date but no valid deposit or withdrawal must fail-closed
	in := "Txn Date,Description,Ref No.,Debit,Credit,Balance\n" +
		"01/09/2026,CORRUPTED AMOUNT ROW,REF101,NOT_AN_AMOUNT,,100000.00\n"

	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatalf("expected fail-closed error on row with valid date but bad amount, got nil")
	}
}

func TestParse_UnifiedAmount_WithDrCrIndicator(t *testing.T) {
	in := "Txn Date,Description,Ref No.,Amount,Dr/Cr\n" +
		"01/09/2026,RENT PAYMENT,REF101,15000.00,CR\n" +
		"02/09/2026,EXPENSE,REF102,2500.00,DR\n"

	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unified amount parse failed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Type != RowTypeCredit || rows[0].AmountPaise != 1500000 {
		t.Errorf("row0: expected Credit 1500000 paise, got %s %d", rows[0].Type, rows[0].AmountPaise)
	}
	if rows[1].Type != RowTypeDebit || rows[1].AmountPaise != 250000 {
		t.Errorf("row1: expected Debit 250000 paise, got %s %d", rows[1].Type, rows[1].AmountPaise)
	}
}

// Assumed-layout fixture derived from screenshot:
// Date | Transaction Reference | Ref.No./Chq.No. | Credit | Debit | Balance
// Followed by pre-data opening balance banner: "Your Opening Balance on 01-08-26: ₹0.81"
func TestParse_SBI_ScreenshotLayout_WithOpeningBalance(t *testing.T) {
	in := "Date,Transaction Reference,Ref.No./Chq.No.,Credit,Debit,Balance\n" +
		"Your Opening Balance on 01-08-26: ₹0.81,,,,,\n" +
		"01-08-26,RENT PAYMENT FROM TENANT,UPI12345678,15000.00,,15000.81\n" +
		"02-08-26,MAINTENANCE EXPENSE,CHQ0001,,2500.00,12500.81\n"

	res, err := ParseWithMeta(strings.NewReader(in))
	if err != nil {
		t.Fatalf("screenshot layout parse failed: %v", err)
	}

	// 1. Verify opening balance anchor
	if res.OpeningBalancePaise == nil {
		t.Fatalf("expected opening balance anchor to be captured, got nil")
	}
	if *res.OpeningBalancePaise != 81 {
		t.Errorf("expected opening balance 81 paise, got %d", *res.OpeningBalancePaise)
	}

	// 2. Verify rows
	if len(res.Rows) != 2 {
		t.Fatalf("expected 2 transaction rows, got %d", len(res.Rows))
	}

	// Row 0: Credit
	r0 := res.Rows[0]
	if r0.Type != RowTypeCredit || r0.AmountPaise != 1500000 {
		t.Errorf("row0: expected Credit 1500000 paise, got %s %d", r0.Type, r0.AmountPaise)
	}
	if r0.TxnID != "UPI12345678" {
		t.Errorf("row0: expected TxnID UPI12345678, got %q", r0.TxnID)
	}
	if r0.Note != "RENT PAYMENT FROM TENANT" {
		t.Errorf("row0: expected note 'RENT PAYMENT FROM TENANT', got %q", r0.Note)
	}
	if r0.Date.Day() != 1 || r0.Date.Month() != 8 || r0.Date.Year() != 2026 {
		t.Errorf("row0: expected 2026-08-01, got %v", r0.Date)
	}
	if r0.BalancePaise == nil || *r0.BalancePaise != 1500081 {
		t.Errorf("row0: expected balance 1500081 paise, got %v", r0.BalancePaise)
	}

	// Row 1: Debit
	r1 := res.Rows[1]
	if r1.Type != RowTypeDebit || r1.AmountPaise != 250000 {
		t.Errorf("row1: expected Debit 250000 paise, got %s %d", r1.Type, r1.AmountPaise)
	}
	if r1.TxnID != "CHQ0001" {
		t.Errorf("row1: expected TxnID CHQ0001, got %q", r1.TxnID)
	}
	if r1.Note != "MAINTENANCE EXPENSE" {
		t.Errorf("row1: expected note 'MAINTENANCE EXPENSE', got %q", r1.Note)
	}
	if r1.Date.Day() != 2 || r1.Date.Month() != 8 || r1.Date.Year() != 2026 {
		t.Errorf("row1: expected 2026-08-02, got %v", r1.Date)
	}
	if r1.BalancePaise == nil || *r1.BalancePaise != 1250081 {
		t.Errorf("row1: expected balance 1250081 paise, got %v", r1.BalancePaise)
	}
}

func TestParse_RejectMoreThanTwoDecimals(t *testing.T) {
	in := "Date,Transaction Reference,Ref.No./Chq.No.,Credit,Debit,Balance\n" +
		"01-08-26,TEST FRACTIONAL PAISE,REF100,12.999,,100.00\n"

	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatalf("expected error rejecting >2 decimal places, got nil")
	}
	if !strings.Contains(err.Error(), "more than 2 decimal places") {
		t.Errorf("expected 'more than 2 decimal places' error, got %v", err)
	}
}

func TestParse_PreDataRowWithAmount_FailsClosed(t *testing.T) {
	// A pre-data row before first valid row that has populated money cells but invalid date must fail closed
	in := "Date,Transaction Reference,Ref.No./Chq.No.,Credit,Debit,Balance\n" +
		"Corrupted Pre-Data Banner,Some Ref,REF999,1000.00,,1000.00\n" +
		"01-08-26,RENT PAYMENT,REF101,15000.00,,16000.00\n"

	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatalf("expected fail-closed error when pre-data row has populated amount cells, got nil")
	}
}




