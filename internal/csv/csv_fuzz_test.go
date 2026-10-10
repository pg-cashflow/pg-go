package csv

import (
	"bytes"
	"strings"
	"testing"
)

func FuzzParseBankCSV(f *testing.F) {
	// Seed with valid and representative bank statement fragments
	seeds := []string{
		"Date,Narration,Chq/Ref No,Withdrawal,Deposit,Balance\n01/01/2026,UPI-123456789012-RENT,123456789012,,15000.00,50000.00\n",
		"Txn Date,Description,Ref No,Debit,Credit,Balance\n02/01/2026,SALARY TRANSFER,REF001,5000.00,,45000.00\n",
		"Value Date,Transaction Remarks,Reference Number,Withdrawal (Dr),Deposit (Cr),Closing Balance\n03/01/2026,IMPS/P2A/123/RENT,IMPS123,,12000.00,57000.00\n",
		"<!DOCTYPE html><html><body>Export Error</body></html>",
		"Random garbage text without headers\nMore lines\n",
		"Date,Narration,Chq/Ref No,Withdrawal,Deposit,Balance\n",
	}

	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		res, err := ParseWithMeta(r)
		if err == nil {
			// Invariant: parsed rows must have valid, non-negative occurrence count
			for _, row := range res.Rows {
				if row.Occurrence < 0 {
					t.Errorf("negative row occurrence: %d", row.Occurrence)
				}
				if strings.Contains(row.TxnID, "\x00") {
					t.Errorf("TxnID contains null byte: %q", row.TxnID)
				}
			}
		}
		// Invariant: ParseWithMeta must never panic on arbitrary bytes
	})
}
