package csv_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/csv"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// TestParse_SBI_AugustStatement_66RowsTieOut validates the real-world characteristics
// of the August SBI account statement:
// - 66 rows total: 15 credits, 51 debits
// - Opening ₹0.81 + sum(credits) - sum(debits) = closing ₹101.18 exact tie-out
// - Deduplication and occurrence tracking for same-day same-amount debits (11-08 and 13-08)
// - UPI/REV reversal credit on 13-08 shares reference with original debit but differs by rowType
// - Payer phone numbers correctly extracted from all 12 UPI credits
func TestParse_SBI_AugustStatement_66RowsTieOut(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("Date,Transaction Reference,Ref.No./Chq.No.,Credit,Debit,Balance\n")
	sb.WriteString("Your Opening Balance on 01-08-26:,₹0.81,,,,\n")

	// We generate 66 rows that tie out:
	// Running balance starts at 81 paise (₹0.81) and ends at 10118 paise (₹101.18).
	runningBalPaise := int64(81)

	type testRowSpec struct {
		date      string
		narration string
		ref       string
		credit    int64 // paise
		debit     int64 // paise
		isUPI     bool
		isRev     bool
		isPayPal  bool
	}

	var specs []testRowSpec

	// Row 1..10: Initial mix of UPI credits and debits
	for i := 1; i <= 8; i++ {
		// UPI Credit
		specs = append(specs, testRowSpec{
			date:      fmt.Sprintf("%02d-08-26", i),
			narration: fmt.Sprintf("UPI/CR/4245123456%02d/TENANT%02d/SBIN/98765432%02d/Rent", i, i, i),
			ref:       "-",
			credit:    1000000, // ₹10,000
			isUPI:     true,
		})
		// Debit
		specs = append(specs, testRowSpec{
			date:      fmt.Sprintf("%02d-08-26", i),
			narration: fmt.Sprintf("UPI/DR/8899001122%02d/VENDOR%02d/HDFC/91234567%02d/Bills", i, i, i),
			ref:       "-",
			debit:     900000, // ₹9,000
		})
	}
	// Currently: 8 credits, 8 debits (16 rows)

	// Add 11-08: Same-day, same-amount pair of debits (₹1,000 and ₹1,000)
	specs = append(specs, testRowSpec{
		date:      "11-08-26",
		narration: "UPI/DR/111122223301/STORE/PAYTM/9988776655/Snacks",
		ref:       "-",
		debit:     100000, // ₹1,000
	})
	specs = append(specs, testRowSpec{
		date:      "11-08-26",
		narration: "UPI/DR/111122223302/STORE/PAYTM/9988776655/Tea",
		ref:       "-",
		debit:     100000, // ₹1,000
	})
	// Now: 8 credits, 10 debits (18 rows)

	// Add 13-08: Reversal scenario:
	// ₹2,000 debit and its ₹2,000 UPI/REV reversal credit sharing reference 778899001122
	// Plus a second ₹2,000 debit on same day (occurrence testing)
	specs = append(specs, testRowSpec{
		date:      "13-08-26",
		narration: "UPI/DR/778899001122/MERCHANT/ICIC/9555444333/Purchase",
		ref:       "-",
		debit:     200000, // ₹2,000
	})
	specs = append(specs, testRowSpec{
		date:      "13-08-26",
		narration: "UPI/REV/778899001122/MERCHANT/ICIC/9555444333/REV",
		ref:       "-",
		credit:    200000, // ₹2,000
		isRev:     true,
	})
	specs = append(specs, testRowSpec{
		date:      "13-08-26",
		narration: "UPI/DR/778899001199/OTHER/HDFC/9444333222/Fuel",
		ref:       "-",
		debit:     200000, // ₹2,000
	})
	// Now: 9 credits, 12 debits (21 rows)

	// Add 4 more UPI credits (reaching 12 UPI credits)
	for i := 14; i <= 17; i++ {
		specs = append(specs, testRowSpec{
			date:      fmt.Sprintf("%02d-08-26", i),
			narration: fmt.Sprintf("UPI/CR/4245123456%02d/TENANT%02d/SBIN/98765432%02d/Rent", i, i, i),
			ref:       "-",
			credit:    500000, // ₹5,000
			isUPI:     true,
		})
	}
	// Now: 13 credits, 12 debits (25 rows)

	// Add 2 NEFT credits (1 PayPal, 1 regular) -> reaching 15 credits total!
	specs = append(specs, testRowSpec{
		date:      "20-08-26",
		narration: "NEFT/PAYPAL PTE LTD/PAYPAL12345",
		ref:       "PAYPAL12345",
		credit:    250000, // ₹2,500
		isPayPal:  true,
	})
	specs = append(specs, testRowSpec{
		date:      "22-08-26",
		narration: "NEFT/TRANSFER/NEFT998877",
		ref:       "NEFT998877",
		credit:    300000, // ₹3,000
	})
	// Now: 15 credits, 12 debits (27 rows)

	// Remaining rows to reach 66 total (51 debits total -> need 39 more debits)
	// We adjust debits so total debits = total credits - 10037 paise (10118 - 81 = 10037 paise net increase)
	var currentCredits, currentDebits int64
	for _, s := range specs {
		currentCredits += s.credit
		currentDebits += s.debit
	}

	targetNetIncrease := int64(10118 - 81) // 10037 paise
	neededTotalDebits := currentCredits - targetNetIncrease
	remainingDebitPaise := neededTotalDebits - currentDebits

	numRemainingDebits := 39 // 12 + 39 = 51 debits
	baseDebit := remainingDebitPaise / int64(numRemainingDebits)
	remainderDebit := remainingDebitPaise % int64(numRemainingDebits)

	for i := 0; i < numRemainingDebits; i++ {
		d := baseDebit
		if i == 0 {
			d += remainderDebit
		}
		day := (i % 8) + 23
		specs = append(specs, testRowSpec{
			date:      fmt.Sprintf("%02d-08-26", day),
			narration: fmt.Sprintf("UPI/DR/DEBIT%04d/EXPENSE/SBIN/9111222333/Daily", i),
			ref:       "-",
			debit:     d,
		})
	}

	// Verify total count
	if len(specs) != 66 {
		t.Fatalf("setup error: expected 66 specs, got %d", len(specs))
	}

	// Build CSV and verify running balance on each row
	for _, s := range specs {
		if s.credit > 0 {
			runningBalPaise += s.credit
			sb.WriteString(fmt.Sprintf("%s,%s,%s,%.2f,,%.2f\n",
				s.date, s.narration, s.ref, float64(s.credit)/100.0, float64(runningBalPaise)/100.0))
		} else {
			runningBalPaise -= s.debit
			sb.WriteString(fmt.Sprintf("%s,%s,%s,,%.2f,%.2f\n",
				s.date, s.narration, s.ref, float64(s.debit)/100.0, float64(runningBalPaise)/100.0))
		}
	}

	// Final running balance must be 101.18
	if runningBalPaise != 10118 {
		t.Fatalf("setup balance mismatch: expected 10118 paise, got %d", runningBalPaise)
	}

	// 1. Parse via ParseWithMeta
	res, err := csv.ParseWithMeta(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("failed parsing 66-row August statement: %v", err)
	}

	// 2. Validate Opening Balance anchor
	if res.OpeningBalancePaise == nil || *res.OpeningBalancePaise != 81 {
		t.Fatalf("expected opening balance anchor 81 paise, got %v", res.OpeningBalancePaise)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("expected 0 warnings, got %v", res.Warnings)
	}

	// 3. Validate Row counts
	if len(res.Rows) != 66 {
		t.Fatalf("expected 66 rows, got %d", len(res.Rows))
	}

	var parsedCredits, parsedDebits int
	var sumCredits, sumDebits int64
	var upiPhoneCount int
	var reversalFound bool

	for _, r := range res.Rows {
		if r.Type == csv.RowTypeCredit {
			parsedCredits++
			sumCredits += int64(r.AmountPaise)
			if r.PayerPhone != "" && !r.IsReversal {
				upiPhoneCount++
			}
			if r.IsReversal {
				reversalFound = true
			}
		} else {
			parsedDebits++
			sumDebits += int64(r.AmountPaise)
		}
	}

	if parsedCredits != 15 {
		t.Errorf("expected 15 credits, got %d", parsedCredits)
	}
	if parsedDebits != 51 {
		t.Errorf("expected 51 debits, got %d", parsedDebits)
	}
	if upiPhoneCount != 12 {
		t.Errorf("expected 12 UPI credits with payer phone extracted, got %d", upiPhoneCount)
	}
	if !reversalFound {
		t.Errorf("expected 13-08 reversal credit to be marked IsReversal")
	}

	// 4. Exact Tie-out check
	// Opening + Credits - Debits == Closing
	lastRowClosing := *res.Rows[65].BalancePaise
	calculatedClosing := *res.OpeningBalancePaise + sumCredits - sumDebits
	if calculatedClosing != 10118 || lastRowClosing != 10118 || calculatedClosing != lastRowClosing {
		t.Errorf("tie-out failed: opening(%d) + credits(%d) - debits(%d) = %d; last row closing=%d, expected=10118",
			*res.OpeningBalancePaise, sumCredits, sumDebits, calculatedClosing, lastRowClosing)
	}

	// 5. Deduplication Hash uniqueness & collision check
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
	if len(seenHashes) != 66 {
		t.Errorf("expected 66 unique dedupe hashes, got %d", len(seenHashes))
	}
}
