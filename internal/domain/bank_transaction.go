package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type BankTransactionStatus string

const (
	BankTxnMatched        BankTransactionStatus = "matched"
	BankTxnSuggestedMatch BankTransactionStatus = "suggested_match"
	BankTxnUnmatched      BankTransactionStatus = "unmatched"
	BankTxnRefunded       BankTransactionStatus = "refunded"
	BankTxnIgnoredDebit   BankTransactionStatus = "ignored_debit"
)

type BankTransaction struct {
	ID                  uuid.UUID             `json:"id"`
	PropertyID          uuid.UUID             `json:"property_id"`
	TxnID               string                `json:"txn_id"`
	AmountPaise         int64                 `json:"amount_paise"`
	RowType             string                `json:"row_type"`
	TxnDate             time.Time             `json:"txn_date"`
	Narration           string                `json:"narration"`
	ClosingBalancePaise *int64                `json:"closing_balance_paise,omitempty"`
	OccurrenceIndex     int                   `json:"occurrence_index"`
	DedupHash           string                `json:"dedup_hash"`
	Status              BankTransactionStatus `json:"status"`
	MatchedDueID        *uuid.UUID            `json:"matched_due_id,omitempty"`
	SuggestedDueID      *uuid.UUID            `json:"suggested_due_id,omitempty"`
	ConfidenceScore     float64               `json:"confidence_score"`
	MatchedAt           *time.Time            `json:"matched_at,omitempty"`
	MatchedBy           *uuid.UUID            `json:"matched_by,omitempty"`
	JournalEntryID      *uuid.UUID            `json:"journal_entry_id,omitempty"`
	CreatedAt           time.Time             `json:"created_at"`
	UpdatedAt           time.Time             `json:"updated_at"`
}

// ComputeBankTxnDedupHash generates a deterministic SHA-256 composite deduplication hash.
// If closing balance is available, it forms the tie-breaker; otherwise, the occurrence index
// within the statement file ensures legitimate same-day duplicate entries survive.
func ComputeBankTxnDedupHash(propertyID uuid.UUID, txnDate time.Time, amountPaise int64, rowType string, txnID string, balancePaise *int64, occurrence int) string {
	dateStr := txnDate.Format("2006-01-02")
	var tieBreaker string
	if balancePaise != nil {
		tieBreaker = fmt.Sprintf("bal:%d", *balancePaise)
	} else {
		tieBreaker = fmt.Sprintf("occ:%d", occurrence)
	}
	raw := fmt.Sprintf("%s|%s|%d|%s|%s|%s",
		propertyID.String(),
		dateStr,
		amountPaise,
		strings.ToLower(strings.TrimSpace(rowType)),
		strings.TrimSpace(txnID),
		tieBreaker,
	)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

type BankTransactionFilter struct {
	Status   *BankTransactionStatus
	FromDate *time.Time
	ToDate   *time.Time
	Limit    int
	Offset   int
}

