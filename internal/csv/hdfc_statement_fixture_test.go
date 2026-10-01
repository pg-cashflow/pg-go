package csv_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/csv"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// TestParse_HDFC_Statement_50RowsTieOut validates the real-world characteristics
// of an HDFC Bank current account statement:
// - Preamble metadata (Account Branch, Address, Account Number, Dates) skipped
// - 50 rows total: 20 credits, 30 debits
// - Opening balance anchor + sum(credits) - sum(debits) = closing balance exact tie-out
// - Deduplication and occurrence tracking for same-day same-amount debits
// - UPI-REV reversal credit sharing reference with original debit
// - Payer phone numbers and VPAs correctly extracted from HDFC UPI narrations
// - Multi-line footer summary tolerance without false errors
// - Zero dedup hash collisions across all 50 rows
func TestParse_HDFC_Statement_50RowsTieOut(t *testing.T) {
	var sb strings.Builder

	// 1. Realistic HDFC Bank preamble
	sb.WriteString("HDFC BANK ENQUIRY FACILITY\n")
	sb.WriteString("Account Branch : KORAMANGALA BANGALORE\n")
	sb.WriteString("Address : 100 FEET ROAD 4TH BLOCK KORAMANGALA BANGALORE 560034\n")
	sb.WriteString("City : BANGALORE  State : KARNATAKA  Phone : 080-61234567\n")
	sb.WriteString("Account No : 50200012345678\n")
	sb.WriteString("Account Description : CURRENT ACCOUNT - DOMESTIC\n")
	sb.WriteString("Currency : INR\n")
	sb.WriteString("Statement From : 01/08/2026 To : 31/08/2026\n")

	// 2. Standard HDFC Header
	sb.WriteString("Date,Narration,Chq./Ref.No.,Value Dt,Withdrawal Amt.,Deposit Amt.,Closing Balance\n")

	// 3. Opening balance row
	sb.WriteString("01/08/2026,OPENING BALANCE,000000000000,01/08/2026,,,50000.00\n")

	// Start running balance: 50,000.00 INR = 5,000,000 paise
	runningBalPaise := int64(5000000)

	type testRowSpec struct {
		date      string
		narration string
		ref       string
		credit    int64 // paise
		debit     int64 // paise
		isUPI     bool
		isRev     bool
	}

	var specs []testRowSpec

	// Rows 1..30: 15 UPI credits (Rent) and 15 debits (Maintenance, Utilities, Supplies)
	for i := 1; i <= 15; i++ {
		// Credit: Tenant rent payment via HDFC UPI
		specs = append(specs, testRowSpec{
			date:      fmt.Sprintf("%02d/08/2026", (i%28)+1),
			narration: fmt.Sprintf("UPI-TENANT%02d-4245998877%02d-HDFC-98765000%02d-PG RENT AUG", i, i, i),
			ref:       fmt.Sprintf("4245998877%02d", i),
			credit:    1200000, // ₹12,000.00
			isUPI:     true,
		})

		// Debit: Vendor/Operations
		specs = append(specs, testRowSpec{
			date:      fmt.Sprintf("%02d/08/2026", (i%28)+1),
			narration: fmt.Sprintf("NEFT DR-VENDOR%02d SUPPLIES-HDFC0000123-N%011d", i, i),
			ref:       fmt.Sprintf("N%011d", i),
			debit:     800000, // ₹8,000.00
		})
	}
	// Currently: 15 credits, 15 debits (30 rows)

	// Rows 31..34: Same-day same-amount transactions on 15/08/2026 (Occurrence tracking test)
	// Two identical ₹2,500 groceries debits on 15/08/2026 with same reference
	specs = append(specs, testRowSpec{
		date:      "15/08/2026",
		narration: "POS 401234XXXXXX1234 DMART BANGALORE",
		ref:       "POS001",
		debit:     250000, // ₹2,500.00
	})
	specs = append(specs, testRowSpec{
		date:      "15/08/2026",
		narration: "POS 401234XXXXXX1234 DMART BANGALORE",
		ref:       "POS001",
		debit:     250000, // ₹2,500.00
	})

	// Two identical ₹15,000 rent credits on 15/08/2026 from different rooms/tenants
	specs = append(specs, testRowSpec{
		date:      "15/08/2026",
		narration: "UPI-ANANYA SHARMA-524511223344-ICIC-9811122233-ROOM 101 RENT",
		ref:       "524511223344",
		credit:    1500000, // ₹15,000.00
		isUPI:     true,
	})
	specs = append(specs, testRowSpec{
		date:      "15/08/2026",
		narration: "UPI-PRIYA VERMA-524511223355-SBIN-9822233344-ROOM 102 RENT",
		ref:       "524511223355",
		credit:    1500000, // ₹15,000.00
		isUPI:     true,
	})
	// Now: 17 credits, 17 debits (34 rows)

	// Rows 35..38: Reversal scenario on 20/08/2026:
	// A ₹5,000 electricity bill debit, its UPI-REV reversal credit, and another unrelated debit
	specs = append(specs, testRowSpec{
		date:      "20/08/2026",
		narration: "UPI-BESCOM-624588990011-HDFC-bescom@billdesk-POWER BILL",
		ref:       "624588990011",
		debit:     500000, // ₹5,000.00
	})
	specs = append(specs, testRowSpec{
		date:      "20/08/2026",
		narration: "UPI-REV-BESCOM-624588990011-HDFC-bescom@billdesk-REVERSAL",
		ref:       "624588990011",
		credit:    500000, // ₹5,000.00
		isRev:     true,
	})
	specs = append(specs, testRowSpec{
		date:      "20/08/2026",
		narration: "UPI-WATER TANKER-624588990099-PAYTM-9899001122-WATER",
		ref:       "624588990099",
		debit:     150000, // ₹1,500.00
	})
	specs = append(specs, testRowSpec{
		date:      "21/08/2026",
		narration: "UPI-ROHIT GUPTA-724512345601-HDFC-rohit@okhdfcbank-ROOM 203",
		ref:       "724512345601",
		credit:    1400000, // ₹14,000.00
		isUPI:     true,
	})
	// Now: 19 credits, 19 debits (38 rows)

	// Rows 39..50: 1 additional credit + 11 small operational debits to reach exactly 20 credits, 30 debits = 50 rows
	specs = append(specs, testRowSpec{
		date:      "22/08/2026",
		narration: "UPI-VIKRAM SINGH-824512345601-AXIS-9833344455-ROOM 305",
		ref:       "824512345601",
		credit:    1600000, // ₹16,000.00
		isUPI:     true,
	})
	for j := 1; j <= 11; j++ {
		specs = append(specs, testRowSpec{
			date:      fmt.Sprintf("%02d/08/2026", 20+(j%10)),
			narration: fmt.Sprintf("UPI-MISC EXPENSE %02d-9245112233%02d-HDFC-97112233%02d-SUPPLIES", j, j, j),
			ref:       fmt.Sprintf("9245112233%02d", j),
			debit:     50000, // ₹500.00
		})
	}
	// Total: 20 credits, 30 debits = 50 rows!

	// Build CSV body rows with exact running balance
	for _, s := range specs {
		if s.credit > 0 {
			runningBalPaise += s.credit
			sb.WriteString(fmt.Sprintf("%s,%s,%s,%s,,%.2f,%.2f\n",
				s.date, s.narration, s.ref, s.date,
				float64(s.credit)/100.0, float64(runningBalPaise)/100.0))
		} else {
			runningBalPaise -= s.debit
			sb.WriteString(fmt.Sprintf("%s,%s,%s,%s,%.2f,,%.2f\n",
				s.date, s.narration, s.ref, s.date,
				float64(s.debit)/100.0, float64(runningBalPaise)/100.0))
		}
	}

	// 4. Realistic HDFC footer summary notes
	sb.WriteString("\n")
	sb.WriteString("******** STATEMENT SUMMARY ********\n")
	sb.WriteString("Total Debits Count: 30 Total Debits Amount: 1,37,000.00\n")
	sb.WriteString("Total Credits Count: 20 Total Credits Amount: 2,45,000.00\n")
	sb.WriteString("Net Difference: 1,08,000.00\n")
	sb.WriteString("Closing Balance as on 31/08/2026: INR 1,58,000.00\n")
	sb.WriteString("******** END OF STATEMENT ********\n")

	// Parse CSV
	res, err := csv.ParseWithMeta(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("ParseWithMeta failed on HDFC statement: %v", err)
	}

	// 1. Opening Balance Anchor Check
	if res.OpeningBalancePaise == nil || *res.OpeningBalancePaise != 5000000 {
		t.Fatalf("expected opening balance anchor 5000000 paise (₹50,000), got %v", res.OpeningBalancePaise)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("expected 0 warnings, got %v", res.Warnings)
	}

	// 2. Validate Row counts
	if len(res.Rows) != 50 {
		t.Fatalf("expected 50 rows, got %d", len(res.Rows))
	}

	var parsedCredits, parsedDebits int
	var sumCredits, sumDebits int64
	var upiPhoneOrVPACount int
	var reversalFound bool

	for _, r := range res.Rows {
		if r.Type == csv.RowTypeCredit {
			parsedCredits++
			sumCredits += int64(r.AmountPaise)
			if (r.PayerPhone != "" || r.PayerVPA != "") && !r.IsReversal {
				upiPhoneOrVPACount++
			}
			if r.IsReversal {
				reversalFound = true
			}
		} else {
			parsedDebits++
			sumDebits += int64(r.AmountPaise)
		}
	}

	if parsedCredits != 20 {
		t.Errorf("expected 20 credits, got %d", parsedCredits)
	}
	if parsedDebits != 30 {
		t.Errorf("expected 30 debits, got %d", parsedDebits)
	}
	if upiPhoneOrVPACount != 19 {
		t.Errorf("expected 19 non-reversal UPI credits with payer phone or VPA extracted, got %d", upiPhoneOrVPACount)
	}
	if !reversalFound {
		t.Errorf("expected 20/08 reversal credit to be marked IsReversal")
	}

	// 3. Exact Tie-out check: Opening + Credits - Debits == Closing
	lastRowClosing := *res.Rows[49].BalancePaise
	calculatedClosing := *res.OpeningBalancePaise + sumCredits - sumDebits
	if calculatedClosing != 15800000 || lastRowClosing != 15800000 || calculatedClosing != lastRowClosing {
		t.Errorf("tie-out failed: opening(%d) + credits(%d) - debits(%d) = %d; last row closing=%d, expected=15800000",
			*res.OpeningBalancePaise, sumCredits, sumDebits, calculatedClosing, lastRowClosing)
	}

	// 4. Same-day occurrence tracking check
	// Rows on 15/08 with identical amount must have occurrence 1 and 2
	var dmartOccurrences []int
	for _, r := range res.Rows {
		if strings.Contains(r.Note, "DMART") {
			dmartOccurrences = append(dmartOccurrences, r.Occurrence)
		}
	}
	if len(dmartOccurrences) != 2 || dmartOccurrences[0] != 1 || dmartOccurrences[1] != 2 {
		t.Errorf("expected DMART debits to have occurrences [1, 2], got %v", dmartOccurrences)
	}

	// 5. Deduplication Hash uniqueness & zero collision check
	propID := uuid.New()
	acctID := uuid.New()
	seenHashes := make(map[string]int)

	for i, r := range res.Rows {
		hash := domain.ComputeBankTxnDedupHash(
			propID,
			&acctID,
			r.Date,
			int64(r.AmountPaise),
			string(r.Type),
			r.TxnID,
			r.BalancePaise,
			r.Occurrence,
		)
		if prevRow, exists := seenHashes[hash]; exists {
			t.Errorf("hash collision between row %d and row %d: hash=%s", prevRow, i, hash)
		}
		seenHashes[hash] = i
	}
	if len(seenHashes) != 50 {
		t.Errorf("expected 50 unique dedupe hashes, got %d", len(seenHashes))
	}

	// 6. Test date disambiguation for Indian DD/MM/YYYY format
	for _, r := range res.Rows {
		if r.Date.Year() != 2026 || r.Date.Month() != time.August {
			t.Errorf("row %s has incorrect date parsing: %v", r.TxnID, r.Date)
		}
	}
}
