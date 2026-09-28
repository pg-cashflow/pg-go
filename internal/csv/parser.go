package csv

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// ErrUnknownSchema is returned when required columns cannot be mapped.
// Callers must reject the entire file — never partial-import.
var ErrUnknownSchema = errors.New("csv: unknown schema — required columns missing")

// RowType distinguishes incoming credits from outgoing debits in a statement.
type RowType string

const (
	RowTypeCredit RowType = "credit"
	RowTypeDebit  RowType = "debit"
)

// Row is one sanitized bank-statement transaction.
type Row struct {
	TxnID        string
	AmountPaise  int
	Type         RowType
	Date         time.Time
	Note         string
	BalancePaise *int64
	Occurrence   int // 1-based index for identical same-day transactions within the file
}

var (
	txnIDAliases      = []string{"txn_id", "txnid", "transaction_id", "transactionid", "utr", "ref_no", "refno", "reference", "upi_ref", "upiref", "upi_txn_id", "transaction_ref", "chq_ref_no", "chq_ref", "chqno", "cheque_no", "cheque_number", "ref_no_cheque_no"}
	depositAliases    = []string{"deposit_amt", "deposit_amount", "deposit", "credit_amt", "credit_amount", "credit", "cr", "deposit_amount_inr", "cr_amt"}
	withdrawalAliases = []string{"withdrawal_amt", "withdrawal_amount", "withdrawal", "debit_amt", "debit_amount", "debit", "dr", "withdrawal_amount_inr", "dr_amt"}
	amountAliases     = []string{"amount", "amt", "transaction_amount", "txn_amount", "net_amount"}
	dateAliases       = []string{"date", "txn_date", "txndate", "transaction_date", "value_date", "valuedate", "posting_date", "tran_date", "value_dt"}
	noteAliases       = []string{"note", "narration", "remarks", "description", "particular", "particulars", "details", "memo", "transaction_remarks"}
	balanceAliases    = []string{"closing_balance", "balance", "bal", "closing_bal", "balance_inr"}
)

// Parse reads a bank statement CSV. All required columns must be present or
// ErrUnknownSchema is returned with no rows.
func Parse(r io.Reader) ([]Row, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.ReuseRecord = true
	cr.LazyQuotes = true

	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrUnknownSchema
		}
		return nil, fmt.Errorf("csv: read header: %w", err)
	}

	idx, err := mapHeaders(header)
	if err != nil {
		return nil, err
	}

	occurrenceMap := make(map[string]int)
	var out []Row
	line := 1
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv: line %d: %w", line+1, err)
		}
		line++
		if isBlank(rec) {
			continue
		}
		row, err := parseRow(rec, idx)
		if err != nil {
			return nil, fmt.Errorf("csv: line %d: %w", line, err)
		}

		// Calculate occurrence index for identical entries within this file
		key := fmt.Sprintf("%s|%d|%s|%s", row.Date.Format("2006-01-02"), row.AmountPaise, row.Type, row.TxnID)
		occurrenceMap[key]++
		row.Occurrence = occurrenceMap[key]

		out = append(out, row)
	}
	return out, nil
}

type colIndex struct {
	txnID      int
	amount     int // unified amount column, if present
	deposit    int // separate deposit/credit column, if present
	withdrawal int // separate withdrawal/debit column, if present
	date       int
	note       int
	balance    int // closing balance column, if present
}

func mapHeaders(header []string) (colIndex, error) {
	norm := make([]string, len(header))
	for i, h := range header {
		norm[i] = normalizeHeader(h)
	}
	find := func(aliases []string) int {
		for i, h := range norm {
			for _, a := range aliases {
				if h == a {
					return i
				}
			}
		}
		return -1
	}
	idx := colIndex{
		txnID:      find(txnIDAliases),
		amount:     find(amountAliases),
		deposit:    find(depositAliases),
		withdrawal: find(withdrawalAliases),
		date:       find(dateAliases),
		note:       find(noteAliases),
		balance:    find(balanceAliases),
	}

	// Must have: date, note, and at least one amount representation (either deposit/withdrawal or unified amount)
	hasAmount := idx.amount >= 0 || (idx.deposit >= 0 || idx.withdrawal >= 0)
	if idx.date < 0 || idx.note < 0 || !hasAmount {
		return colIndex{}, ErrUnknownSchema
	}
	return idx, nil
}

func normalizeHeader(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	h = strings.ReplaceAll(h, " ", "_")
	h = strings.ReplaceAll(h, "-", "_")
	h = strings.ReplaceAll(h, ".", "_")
	h = strings.ReplaceAll(h, "/", "_")
	h = strings.ReplaceAll(h, "(", "")
	h = strings.ReplaceAll(h, ")", "")
	for strings.Contains(h, "__") {
		h = strings.ReplaceAll(h, "__", "_")
	}
	return strings.Trim(h, "_")
}

func parseRow(rec []string, idx colIndex) (Row, error) {
	requiredIndices := []int{idx.date, idx.note}
	if idx.txnID >= 0 {
		requiredIndices = append(requiredIndices, idx.txnID)
	}
	if idx.amount >= 0 {
		requiredIndices = append(requiredIndices, idx.amount)
	}
	if idx.deposit >= 0 {
		requiredIndices = append(requiredIndices, idx.deposit)
	}
	if idx.withdrawal >= 0 {
		requiredIndices = append(requiredIndices, idx.withdrawal)
	}
	if idx.balance >= 0 {
		requiredIndices = append(requiredIndices, idx.balance)
	}

	maxIdx := 0
	for _, i := range requiredIndices {
		if i > maxIdx {
			maxIdx = i
		}
	}
	if len(rec) <= maxIdx {
		return Row{}, fmt.Errorf("too few columns")
	}

	var txnID string
	if idx.txnID >= 0 {
		txnID = StripFormulaChars(rec[idx.txnID])
	}
	note := StripFormulaChars(rec[idx.note])
	dateRaw := StripFormulaChars(rec[idx.date])

	dt, err := parseDate(dateRaw)
	if err != nil {
		return Row{}, err
	}

	var rowType RowType
	var amountPaise int

	// Split column resolution takes priority over unified amount
	if idx.deposit >= 0 || idx.withdrawal >= 0 {
		var depRaw, wdlRaw string
		if idx.deposit >= 0 {
			depRaw = StripFormulaChars(rec[idx.deposit])
		}
		if idx.withdrawal >= 0 {
			wdlRaw = StripFormulaChars(rec[idx.withdrawal])
		}

		depPaise, depErr := parseAmountPaise(depRaw)
		wdlPaise, wdlErr := parseAmountPaise(wdlRaw)

		if depErr == nil && depPaise > 0 {
			rowType = RowTypeCredit
			amountPaise = depPaise
		} else if wdlErr == nil && wdlPaise > 0 {
			rowType = RowTypeDebit
			amountPaise = wdlPaise
		} else {
			// Row has neither valid deposit nor withdrawal
			return Row{}, fmt.Errorf("row has no positive deposit or withdrawal amount")
		}
	} else {
		amtRaw := StripFormulaChars(rec[idx.amount])
		amt, err := parseAmountPaise(amtRaw)
		if err != nil {
			return Row{}, err
		}
		rowType = RowTypeCredit
		amountPaise = amt
	}

	var balancePaise *int64
	if idx.balance >= 0 {
		balRaw := StripFormulaChars(rec[idx.balance])
		if balRaw != "" {
			if bal, err := parseSignedAmountPaise(balRaw); err == nil {
				balancePaise = &bal
			}
		}
	}

	return Row{
		TxnID:        txnID,
		AmountPaise:  amountPaise,
		Type:         rowType,
		Date:         dt,
		Note:         note,
		BalancePaise: balancePaise,
	}, nil
}

func parseAmountPaise(s string) (int, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimPrefix(s, "₹")
	s = strings.TrimPrefix(s, "Rs.")
	s = strings.TrimPrefix(s, "INR")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("amount: %w", err)
	}
	if f < 0 {
		f = -f
	}
	return int(f*100 + 0.5), nil
}

func parseSignedAmountPaise(s string) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimPrefix(s, "₹")
	s = strings.TrimPrefix(s, "Rs.")
	s = strings.TrimPrefix(s, "INR")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty balance")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("balance: %w", err)
	}
	return int64(f*100 + 0.5), nil
}

// Indian commercial banking date formats (Day-first and ISO only; strictly NO US month-first).
var dateLayouts = []string{
	"2006-01-02",
	"2006/01/02",
	"02-01-2006",
	"02/01/2006",
	"02-01-06",
	"02/01/06",
	"02-Jan-2006",
	"02 Jan 2006",
	"02-Jan-06",
	"02 Jan 06",
	"2-1-2006",
	"2/1/2006",
	"2-1-06",
	"2/1/06",
	time.RFC3339,
}

func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			year := t.Year()
			if year < 100 {
				year += 2000
			}
			return time.Date(year, t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date %q", s)
}

func isBlank(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
