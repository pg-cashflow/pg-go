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

// Row is one sanitized bank-statement transaction.
type Row struct {
	TxnID       string
	AmountPaise int
	Date        time.Time
	Note        string
}

var (
	txnIDAliases  = []string{"txn_id", "txnid", "transaction_id", "transactionid", "utr", "ref_no", "refno", "reference", "upi_ref", "upiref", "upi_txn_id", "transaction_ref"}
	amountAliases = []string{"amount", "amt", "debit", "credit", "transaction_amount", "txn_amount", "withdrawal", "deposit"}
	dateAliases   = []string{"date", "txn_date", "txndate", "transaction_date", "value_date", "valuedate", "posting_date"}
	noteAliases   = []string{"note", "narration", "remarks", "description", "particular", "particulars", "details", "memo"}
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
		out = append(out, row)
	}
	return out, nil
}

type colIndex struct {
	txnID, amount, date, note int
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
		txnID:  find(txnIDAliases),
		amount: find(amountAliases),
		date:   find(dateAliases),
		note:   find(noteAliases),
	}
	if idx.txnID < 0 || idx.amount < 0 || idx.date < 0 || idx.note < 0 {
		return colIndex{}, ErrUnknownSchema
	}
	return idx, nil
}

func normalizeHeader(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	h = strings.ReplaceAll(h, " ", "_")
	h = strings.ReplaceAll(h, "-", "_")
	return h
}

func parseRow(rec []string, idx colIndex) (Row, error) {
	need := max(idx.txnID, idx.amount, idx.date, idx.note) + 1
	if len(rec) < need {
		return Row{}, fmt.Errorf("too few columns")
	}
	txnID := StripFormulaChars(rec[idx.txnID])
	note := StripFormulaChars(rec[idx.note])
	amtRaw := StripFormulaChars(rec[idx.amount])
	dateRaw := StripFormulaChars(rec[idx.date])

	if txnID == "" {
		return Row{}, fmt.Errorf("empty txn_id")
	}
	amountPaise, err := parseAmountPaise(amtRaw)
	if err != nil {
		return Row{}, err
	}
	dt, err := parseDate(dateRaw)
	if err != nil {
		return Row{}, err
	}
	return Row{
		TxnID:       txnID,
		AmountPaise: amountPaise,
		Date:        dt,
		Note:        note,
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
	// Rupees with optional decimals → paise.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("amount: %w", err)
	}
	if f < 0 {
		f = -f
	}
	return int(f*100 + 0.5), nil
}

var dateLayouts = []string{
	"2006-01-02",
	"02-01-2006",
	"02/01/2006",
	"01/02/2006",
	"2-1-2006",
	"2/1/2006",
	"2006/01/02",
	"02 Jan 2006",
	"02-Jan-2006",
	time.RFC3339,
}

func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
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

func max(a, b, c, d int) int {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	if d > m {
		m = d
	}
	return m
}
