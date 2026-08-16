package domain

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
)

type DueStatus string

const (
	DueStatusPending DueStatus = "pending"
	DueStatusPartial DueStatus = "partial"
	DueStatusPaid    DueStatus = "paid"
	DueStatusWaived  DueStatus = "waived"
)

type DueKind string

const (
	DueKindRent    DueKind = "rent"
	DueKindDeposit DueKind = "deposit"
)

type Due struct {
	ID             uuid.UUID  `json:"id"`
	DueCode        string     `json:"due_code"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	PropertyID     uuid.UUID  `json:"property_id"`
	Kind           DueKind    `json:"kind"`
	Amount         int        `json:"amount"`          // paise current payable
	OriginalAmount int        `json:"original_amount"` // immutable
	PeriodStart    time.Time  `json:"period_start"`
	PeriodEnd      time.Time  `json:"period_end"`
	DueDate        time.Time  `json:"due_date"`
	Status         DueStatus  `json:"status"`
	PaidAt         *time.Time `json:"paid_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const dueCodeAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// GenerateDueCode returns a 6-char base36 code. Caller retries on UNIQUE conflict.
func GenerateDueCode() (string, error) {
	const n = 6
	out := make([]byte, n)
	max := big.NewInt(int64(len(dueCodeAlphabet)))
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = dueCodeAlphabet[v.Int64()]
	}
	return string(out), nil
}

// UPINote encodes tn= as PG-<due_code> (11 chars) to survive bank narration truncation.
func UPINote(dueCode string) string {
	return fmt.Sprintf("PG-%s", dueCode)
}

// MarkPaid sets status paid and paid_at.
func (d *Due) MarkPaid(at time.Time) {
	d.Status = DueStatusPaid
	d.PaidAt = &at
}

// Prorate updates current amount for mid-cycle vacate. original_amount stays immutable.
func (d *Due) Prorate(proratedPaise int) {
	d.Amount = proratedPaise
}

// ApplyPayment reduces remaining amount. Returns overpayment paise (credit) if any.
// UPI may leave status=partial; cash (D2) must pass amount == remaining and fully settle.
func (d *Due) ApplyPayment(paidPaise int, at time.Time) (creditPaise int) {
	if paidPaise >= d.Amount {
		creditPaise = paidPaise - d.Amount
		d.Amount = 0
		d.MarkPaid(at)
		return creditPaise
	}
	d.Amount -= paidPaise
	d.Status = DueStatusPartial
	return 0
}

// DaysInPeriod returns inclusive calendar days in the billing period.
func DaysInPeriod(start, end time.Time) int {
	s := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	e := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	return int(e.Sub(s).Hours()/24) + 1
}

// ProrateAmount computes floor(original * daysOccupied / daysInPeriod).
func ProrateAmount(originalPaise, daysOccupied, daysInPeriod int) int {
	if daysInPeriod <= 0 || daysOccupied <= 0 {
		return 0
	}
	if daysOccupied >= daysInPeriod {
		return originalPaise
	}
	return originalPaise * daysOccupied / daysInPeriod
}
