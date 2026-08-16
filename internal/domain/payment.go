package domain

import (
	"time"

	"github.com/google/uuid"
)

type MatchedBy string

const (
	MatchedByDueCode          MatchedBy = "due_code"
	MatchedByAmountDateWindow MatchedBy = "amount_date_window"
	MatchedByManual           MatchedBy = "manual"
	MatchedByCash             MatchedBy = "cash"
	MatchedByCashfree         MatchedBy = "cashfree"
)

type Payment struct {
	ID         uuid.UUID  `json:"id"`
	DueID      uuid.UUID  `json:"due_id"`
	TenantID   uuid.UUID  `json:"tenant_id"`
	UPITxnID   *string    `json:"upi_txn_id,omitempty"`
	Amount     int        `json:"amount"`
	MatchedBy  MatchedBy  `json:"matched_by"`
	RecordedBy *uuid.UUID `json:"recorded_by,omitempty"`
	MatchedAt  time.Time  `json:"matched_at"`
	RawNote    *string    `json:"raw_note,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}
