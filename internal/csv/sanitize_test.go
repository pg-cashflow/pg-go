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

