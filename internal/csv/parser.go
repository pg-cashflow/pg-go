package csv

import (
	"bufio"
	"bytes"
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
	TxnID              string
	AmountPaise        int
	Type               RowType
	Date               time.Time
	Note               string
	BalancePaise       *int64
	Occurrence         int // 1-based index for identical same-day transactions within the file
	PayerPhone         string
	PayerVPA           string
	PayerName          string
	IsReversal         bool
	IsInternalTransfer bool
}

// ParseResult holds parsed statement rows, opening balance anchor, and warnings.
type ParseResult struct {
	Rows                []Row
	OpeningBalancePaise *int64
	Warnings            []string
}

var (
	txnIDAliases      = []string{"txn_id", "txnid", "transaction_id", "transactionid", "utr", "ref_no", "refno", "reference", "upi_ref", "upiref", "upi_txn_id", "transaction_ref", "chq_ref_no", "chq_ref", "chqno", "chq_no", "cheque_no", "cheque_number", "ref_no_cheque_no", "ref_no_chq_no", "chq_no_ref_no"}
	depositAliases    = []string{"deposit_amt", "deposit_amount", "deposit", "credit_amt", "credit_amount", "credit", "cr", "deposit_amount_inr", "cr_amt"}
	withdrawalAliases = []string{"withdrawal_amt", "withdrawal_amount", "withdrawal", "debit_amt", "debit_amount", "debit", "dr", "withdrawal_amount_inr", "dr_amt"}
	amountAliases     = []string{"amount", "amt", "transaction_amount", "txn_amount", "net_amount"}
	indicatorAliases  = []string{"type", "txn_type", "transaction_type", "dr_cr", "cr_dr", "indicator", "drcr"}
	dateAliases       = []string{"date", "txn_date", "txndate", "transaction_date", "value_date", "valuedate", "posting_date", "tran_date", "value_dt", "post_date"}
	noteAliases       = []string{"note", "narration", "remarks", "description", "particular", "particulars", "details", "memo", "transaction_remarks", "transaction_reference", "txn_reference", "txn_ref"}
	balanceAliases    = []string{"closing_balance", "balance", "bal", "closing_bal", "balance_inr"}
)

// Parse reads a bank statement CSV and returns sanitized transaction rows.
// Backward-compatible wrapper for ParseWithMeta.
func Parse(r io.Reader) ([]Row, error) {
	res, err := ParseWithMeta(r)
	if err != nil {
		return nil, err
	}
	return res.Rows, nil
}

// ParseWithMeta reads a bank statement CSV. It scans initial rows to locate the header row,
// safely skipping account preamble/metadata lines common in bank exports (e.g. SBI).
// Sniffs content-type to reject HTML and binary Excel exports fail-closed.
// All required columns must be present in the detected header or ErrUnknownSchema is returned.
// Footer, summary, and opening-balance pre-data rows without populated money cells are tolerated.
func ParseWithMeta(r io.Reader) (ParseResult, error) {
	br := bufio.NewReader(r)
	peekBytes, _ := br.Peek(512)
	if len(peekBytes) > 0 {
		trimmed := strings.TrimSpace(string(peekBytes))
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "<!DOCTYPE") || strings.HasPrefix(upper, "<HTML") || strings.HasPrefix(upper, "<TABLE") || strings.HasPrefix(upper, "<?XML") {
			return ParseResult{}, errors.New("html file detected — please export as CSV")
		}
		if len(peekBytes) >= 8 && bytes.Equal(peekBytes[:8], []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}) {
			return ParseResult{}, errors.New("binary Excel (.xls) file detected — please export as CSV")
		}
		if len(peekBytes) >= 4 && bytes.Equal(peekBytes[:4], []byte{0x50, 0x4B, 0x03, 0x04}) {
			return ParseResult{}, errors.New("Excel (.xlsx) file detected — please export as CSV")
		}
	}

	cr := csv.NewReader(br)
	cr.TrimLeadingSpace = true
	cr.ReuseRecord = true
	cr.LazyQuotes = true
	cr.FieldsPerRecord = -1 // Allow variable columns for prelude metadata rows

	var idx colIndex
	var headerFound bool
	line := 0

	// Scan up to 50 rows looking for the header row
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ParseResult{}, fmt.Errorf("csv: read header error: %w", err)
		}
		line++
		if isBlank(rec) {
			continue
		}
		colIdx, err := mapHeaders(rec)
		if err == nil {
			idx = colIdx
			headerFound = true
			break
		}
		if line >= 50 {
			break
		}
	}

	if !headerFound {
		return ParseResult{}, ErrUnknownSchema
	}

	var openingBalance *int64
	occurrenceMap := make(map[string]int)
	var out []Row

	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ParseResult{}, fmt.Errorf("csv: line %d: %w", line+1, err)
		}
		line++
		if isBlank(rec) {
			continue
		}

		hasDep := idx.deposit >= 0 && idx.deposit < len(rec) && strings.TrimSpace(StripFormulaChars(rec[idx.deposit])) != ""
		hasWdl := idx.withdrawal >= 0 && idx.withdrawal < len(rec) && strings.TrimSpace(StripFormulaChars(rec[idx.withdrawal])) != ""
		hasAmt := idx.amount >= 0 && idx.amount < len(rec) && strings.TrimSpace(StripFormulaChars(rec[idx.amount])) != ""

		// Check if row has enough columns for date
		if len(rec) <= idx.date {
			if len(out) > 0 {
				// Reached footer/summary notes at end of table
				break
			}
			// Pre-data row with no deposit/withdrawal/amount (e.g. opening balance banner)
			if !hasDep && !hasWdl && !hasAmt {
				if ob := tryExtractOpeningBalance(rec, idx); ob != nil && openingBalance == nil {
					openingBalance = ob
				}
				continue
			}
			return ParseResult{}, fmt.Errorf("csv: line %d: too few columns", line)
		}

		dateRaw := StripFormulaChars(rec[idx.date])
		dt, dateErr := parseDate(dateRaw)
		if dateErr != nil {
			if len(out) > 0 {
				// End-of-table summary/footer notes reached (e.g. "** Computer generated **", "Total:")
				break
			}
			// Pre-data row with non-date text (e.g. "Your Opening Balance on 01-08-26: ₹0.81")
			if !hasDep && !hasWdl && !hasAmt {
				if ob := tryExtractOpeningBalance(rec, idx); ob != nil && openingBalance == nil {
					openingBalance = ob
				}
				continue // Safely skip pre-data descriptive row
			}
			// If any money cell is populated, reject fail-closed!
			return ParseResult{}, fmt.Errorf("csv: line %d: %w", line, dateErr)
		}

		// If date parses, but before data rows start and all money cells are empty:
		if len(out) == 0 && !hasDep && !hasWdl && !hasAmt {
			noteText := ""
			if idx.note >= 0 && idx.note < len(rec) {
				noteText = strings.ToUpper(strings.TrimSpace(StripFormulaChars(rec[idx.note])))
			}
			if strings.Contains(noteText, "OPENING") || strings.Contains(noteText, "B/F") ||
				strings.Contains(noteText, "BROUGHT FORWARD") || strings.Contains(noteText, "BALANCE") {
				if ob := tryExtractOpeningBalance(rec, idx); ob != nil && openingBalance == nil {
					openingBalance = ob
				}
				continue
			}
		}

		// Row has a valid date and is a data candidate — must parse valid amount and note (fail-closed for money)
		row, err := parseRowWithDate(rec, idx, dt)
		if err != nil {
			return ParseResult{}, fmt.Errorf("csv: line %d: %w", line, err)
		}

		// Calculate occurrence index for identical entries within this file
		key := fmt.Sprintf("%s|%d|%s|%s", row.Date.Format("2006-01-02"), row.AmountPaise, row.Type, row.TxnID)
		occurrenceMap[key]++
		row.Occurrence = occurrenceMap[key]

		out = append(out, row)
	}

	var warnings []string
	if openingBalance == nil {
		warnings = append(warnings, "missing opening balance anchor")
	}

	return ParseResult{
		Rows:                out,
		OpeningBalancePaise: openingBalance,
		Warnings:            warnings,
	}, nil
}

type colIndex struct {
	txnID      int
	amount     int // unified amount column, if present
	indicator  int // transaction direction indicator column (Dr/Cr, Type), if present
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
		indicator:  find(indicatorAliases),
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

func parseRowWithDate(rec []string, idx colIndex, dt time.Time) (Row, error) {
	requiredIndices := []int{idx.date, idx.note}
	if idx.txnID >= 0 {
		requiredIndices = append(requiredIndices, idx.txnID)
	}
	if idx.amount >= 0 {
		requiredIndices = append(requiredIndices, idx.amount)
	}
	if idx.indicator >= 0 {
		requiredIndices = append(requiredIndices, idx.indicator)
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

	var rowType RowType
	var amountPaise int

	// Split column resolution takes priority over unified amount
	if idx.deposit >= 0 || idx.withdrawal >= 0 {
		var depRaw, wdlRaw string
		if idx.deposit >= 0 && idx.deposit < len(rec) {
			depRaw = strings.TrimSpace(StripFormulaChars(rec[idx.deposit]))
		}
		if idx.withdrawal >= 0 && idx.withdrawal < len(rec) {
			wdlRaw = strings.TrimSpace(StripFormulaChars(rec[idx.withdrawal]))
		}

		if depRaw != "" {
			depPaise, depErr := parseAmountPaise(depRaw)
			if depErr != nil {
				return Row{}, fmt.Errorf("invalid deposit amount %q: %w", depRaw, depErr)
			}
			if depPaise > 0 {
				rowType = RowTypeCredit
				amountPaise = depPaise
			}
		}

		if wdlRaw != "" {
			wdlPaise, wdlErr := parseAmountPaise(wdlRaw)
			if wdlErr != nil {
				return Row{}, fmt.Errorf("invalid withdrawal amount %q: %w", wdlRaw, wdlErr)
			}
			if wdlPaise > 0 {
				if rowType == RowTypeCredit {
					return Row{}, fmt.Errorf("row has both deposit and withdrawal amounts")
				}
				rowType = RowTypeDebit
				amountPaise = wdlPaise
			}
		}

		if amountPaise == 0 {
			return Row{}, fmt.Errorf("row has no positive deposit or withdrawal amount")
		}
	} else if idx.amount >= 0 {
		amtRaw := StripFormulaChars(rec[idx.amount])
		if amtRaw == "" {
			return Row{}, fmt.Errorf("empty amount")
		}

		if idx.indicator >= 0 {
			ind := strings.ToUpper(strings.TrimSpace(StripFormulaChars(rec[idx.indicator])))
			amt, err := parseAmountPaise(amtRaw)
			if err != nil {
				return Row{}, err
			}
			if ind == "CR" || ind == "CREDIT" {
				rowType = RowTypeCredit
				amountPaise = amt
			} else if ind == "DR" || ind == "DEBIT" {
				rowType = RowTypeDebit
				amountPaise = amt
			} else {
				return Row{}, fmt.Errorf("unrecognized transaction direction indicator %q", ind)
			}
		} else {
			upperAmt := strings.ToUpper(amtRaw)
			isDr := strings.Contains(upperAmt, "DR") || strings.HasPrefix(strings.TrimSpace(amtRaw), "-")
			isCr := strings.Contains(upperAmt, "CR") || strings.HasPrefix(strings.TrimSpace(amtRaw), "+")

			amtSigned, err := parseSignedAmountPaise(amtRaw)
			if err != nil {
				return Row{}, err
			}
			if isDr {
				rowType = RowTypeDebit
				if amtSigned < 0 {
					amtSigned = -amtSigned
				}
				amountPaise = int(amtSigned)
			} else if isCr {
				rowType = RowTypeCredit
				amountPaise = int(amtSigned)
			} else {
				// Default unsigned positive unified amounts to credit
				rowType = RowTypeCredit
				amountPaise = int(amtSigned)
			}
		}
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

	var payerPhone, payerVPA, payerName string
	var isReversal, isInternalTransfer bool

	upperNote := strings.ToUpper(note)
	if strings.Contains(upperNote, "REVERSAL") || strings.Contains(upperNote, "REV-UPI") || strings.Contains(upperNote, "UPI/REV/") {
		isReversal = true
	}
	if strings.Contains(upperNote, "SWEEP") || strings.Contains(upperNote, "MOD TO") || strings.Contains(upperNote, "TO MOD") || strings.Contains(upperNote, "MOD BAL") || strings.Contains(upperNote, "AUTO SWEEP") {
		isInternalTransfer = true
	}

	// SBI UPI pattern: UPI/(CR|REV|DR)/<12-digit ref>/<payer name>/<bank>/<payer phone or VPA>/<remark>
	if strings.HasPrefix(upperNote, "UPI/") {
		parts := strings.Split(note, "/")
		if len(parts) >= 6 {
			dir := strings.ToUpper(parts[1])
			if dir == "REV" {
				isReversal = true
			}
			ref := strings.TrimSpace(parts[2])
			if (txnID == "" || txnID == "-" || strings.Trim(txnID, "0") == "") && ref != "" {
				txnID = ref
			}
			payerName = strings.TrimSpace(parts[3])
			phoneOrVPA := strings.TrimSpace(parts[5])
			if strings.Contains(phoneOrVPA, "@") {
				payerVPA = phoneOrVPA
			} else {
				digits := strings.Map(func(r rune) rune {
					if r >= '0' && r <= '9' {
						return r
					}
					return -1
				}, phoneOrVPA)
				if len(digits) == 10 {
					payerPhone = digits
				} else if len(digits) == 12 && strings.HasPrefix(digits, "91") {
					payerPhone = digits[2:]
				}
			}
		}
	}

	return Row{
		TxnID:              txnID,
		AmountPaise:        amountPaise,
		Type:               rowType,
		Date:               dt,
		Note:               note,
		BalancePaise:       balancePaise,
		PayerPhone:         payerPhone,
		PayerVPA:           payerVPA,
		PayerName:          payerName,
		IsReversal:         isReversal,
		IsInternalTransfer: isInternalTransfer,
	}, nil
}

func tryExtractOpeningBalance(rec []string, idx colIndex) *int64 {
	// 1. If balance column has a value
	if idx.balance >= 0 && idx.balance < len(rec) {
		balRaw := StripFormulaChars(rec[idx.balance])
		if balRaw != "" {
			if bal, err := parseSignedAmountPaise(balRaw); err == nil {
				return &bal
			}
		}
	}
	// 2. Scan across all cells for opening balance label and candidate amount
	for i, cell := range rec {
		s := strings.TrimSpace(cell)
		upper := strings.ToUpper(s)
		if strings.Contains(upper, "OPENING BALANCE") || strings.Contains(upper, "BALANCE AS ON") {
			// A. If amount is in same cell after colon
			idxColon := strings.LastIndex(s, ":")
			if idxColon >= 0 && idxColon < len(s)-1 {
				candidate := strings.TrimSpace(s[idxColon+1:])
				if bal, err := parseSignedAmountPaise(candidate); err == nil {
					return &bal
				}
			}
			// B. If amount is the last word in same cell
			words := strings.Fields(s)
			if len(words) > 1 {
				candidate := words[len(words)-1]
				if bal, err := parseSignedAmountPaise(candidate); err == nil {
					return &bal
				}
			}
			// C. Check subsequent adjacent cells on this row (e.g. table conversion splits label and amount)
			for j := i + 1; j < len(rec); j++ {
				adj := strings.TrimSpace(StripFormulaChars(rec[j]))
				if adj != "" {
					if bal, err := parseSignedAmountPaise(adj); err == nil {
						return &bal
					}
				}
			}
		}
	}
	return nil
}

// parseSignedAmountPaise parses decimal amounts into integer paise without floating point drift.
// Rejects amounts with more than 2 decimal places (fail-closed for money).
// Tolerates Indian comma formatting (e.g. 1,26,948.00), parenthesised accounting (18,500.00),
// and trailing/leading Cr/Dr indicators, including parenthesised (Cr)/(Dr).
func parseSignedAmountPaise(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}

	upper := strings.ToUpper(s)
	isCreditSuffix := false
	isDebitSuffix := false

	if strings.HasSuffix(upper, "(CR)") {
		isCreditSuffix = true
		upper = strings.TrimSuffix(upper, "(CR)")
	} else if strings.HasSuffix(upper, "(DR)") {
		isDebitSuffix = true
		upper = strings.TrimSuffix(upper, "(DR)")
	} else if strings.HasSuffix(upper, "CR") {
		isCreditSuffix = true
		upper = strings.TrimSuffix(upper, "CR")
	} else if strings.HasSuffix(upper, "DR") {
		isDebitSuffix = true
		upper = strings.TrimSuffix(upper, "DR")
	}

	upper = strings.TrimSpace(upper)

	isParenthesesNegative := false
	if strings.HasPrefix(upper, "(") && strings.HasSuffix(upper, ")") {
		isParenthesesNegative = true
		upper = strings.TrimPrefix(upper, "(")
		upper = strings.TrimSuffix(upper, ")")
		upper = strings.TrimSpace(upper)
	}

	s = upper
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimPrefix(s, "₹")
	s = strings.TrimPrefix(s, "RS.")
	s = strings.TrimPrefix(s, "INR")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount after stripping symbols")
	}

	negative := false
	if strings.HasPrefix(s, "-") {
		negative = true
		s = strings.TrimPrefix(s, "-")
	} else if strings.HasPrefix(s, "+") {
		s = strings.TrimPrefix(s, "+")
	} else if isParenthesesNegative || isDebitSuffix {
		negative = true
	} else if isCreditSuffix {
		negative = false
	}

	s = strings.TrimSpace(s)
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return 0, fmt.Errorf("invalid decimal format %q", s)
	}

	rupees, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid rupees %q: %w", parts[0], err)
	}

	var paise int64
	if len(parts) == 2 {
		dec := parts[1]
		if len(dec) == 1 {
			dec += "0"
		} else if len(dec) > 2 {
			return 0, fmt.Errorf("amount %q has more than 2 decimal places", s)
		}
		p, err := strconv.ParseInt(dec, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid paise %q: %w", parts[1], err)
		}
		paise = p
	}

	total := rupees*100 + paise
	if negative {
		total = -total
	}
	return total, nil
}

func parseAmountPaise(s string) (int, error) {
	paise, err := parseSignedAmountPaise(s)
	if err != nil {
		return 0, err
	}
	if paise < 0 {
		paise = -paise
	}
	return int(paise), nil
}

// Indian commercial banking date formats (Day-first and ISO only; strictly NO US month-first).
// Includes unpadded single-digit day formats (e.g. 1 Jun 2020) common in SBI exports.
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
	"2-Jan-2006",
	"2 Jan 2006",
	"2-Jan-06",
	"2 Jan 06",
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
