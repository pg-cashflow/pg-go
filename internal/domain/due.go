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
	DueKindRent        DueKind = "rent"
	DueKindDeposit     DueKind = "deposit"
	DueKindElectricity DueKind = "electricity"
	DueKindWater       DueKind = "water"
)

type Due struct {
	ID             uuid.UUID  `json:"id"`
	DueCode        string     `json:"due_code"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	PropertyID     uuid.UUID  `json:"property_id"`
	Kind                    DueKind    `json:"kind"`
	Amount                  int        `json:"amount"` // paise current payable
	OriginalAmount          int        `json:"original_amount"` // immutable
	ContractualCeilingPaise *int       `json:"contractual_ceiling_paise,omitempty"` // persisted ceiling for prorated/vacated dues
	PeriodStart             time.Time  `json:"period_start"`
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

type PaymentOptionType string

const (
	OptionOldest1 PaymentOptionType = "oldest_1"
	OptionOldest2 PaymentOptionType = "oldest_2"
	OptionAll     PaymentOptionType = "all"
)

type DueSummary struct {
	ID          uuid.UUID `json:"id"`
	DueCode     string    `json:"due_code"`
	DueDate     time.Time `json:"due_date"`
	AmountPaise int       `json:"amount_paise"`
	Kind        DueKind   `json:"kind"`
}

type PaymentOption struct {
	OptionType  PaymentOptionType `json:"option_type"`
	Label       string            `json:"label"`
	DueCount    int               `json:"due_count"`
	AmountPaise int               `json:"amount_paise"`
	DueIDs      []uuid.UUID       `json:"due_ids"`
	Dues        []DueSummary      `json:"dues"`
}

// CalculatePaymentOptions takes all dues for a tenant, filters open (pending/partial) dues,
// sorts them FIFO (by DueDate ASC, PeriodStart ASC, ID ASC), and returns discrete payment options:
// - 1 open due: 1 option ("Pay Due")
// - 2 open dues: 2 options ("Oldest Due", "Clear All (2 Dues)")
// - 3+ open dues: 3 options ("Oldest Due", "Oldest 2 Dues", "Clear All (N Dues)")
func CalculatePaymentOptions(dues []*Due) []PaymentOption {
	var open []*Due
	for _, d := range dues {
		if d != nil && (d.Status == DueStatusPending || d.Status == DueStatusPartial) && d.Amount > 0 {
			open = append(open, d)
		}
	}
	if len(open) == 0 {
		return []PaymentOption{}
	}

	// Sort FIFO: oldest due_date first, then period_start, then ID
	for i := 0; i < len(open)-1; i++ {
		for j := i + 1; j < len(open); j++ {
			swap := false
			if !open[i].DueDate.Equal(open[j].DueDate) {
				swap = open[i].DueDate.After(open[j].DueDate)
			} else if !open[i].PeriodStart.Equal(open[j].PeriodStart) {
				swap = open[i].PeriodStart.After(open[j].PeriodStart)
			} else {
				swap = open[i].ID.String() > open[j].ID.String()
			}
			if swap {
				open[i], open[j] = open[j], open[i]
			}
		}
	}

	toSummaries := func(list []*Due) ([]uuid.UUID, []DueSummary, int) {
		ids := make([]uuid.UUID, len(list))
		sums := make([]DueSummary, len(list))
		total := 0
		for i, d := range list {
			ids[i] = d.ID
			sums[i] = DueSummary{
				ID:          d.ID,
				DueCode:     d.DueCode,
				DueDate:     d.DueDate,
				AmountPaise: d.Amount,
				Kind:        d.Kind,
			}
			total += d.Amount
		}
		return ids, sums, total
	}

	n := len(open)
	if n == 1 {
		ids, sums, total := toSummaries(open[:1])
		return []PaymentOption{
			{
				OptionType:  OptionOldest1,
				Label:       "Pay Due",
				DueCount:    1,
				AmountPaise: total,
				DueIDs:      ids,
				Dues:        sums,
			},
		}
	}

	if n == 2 {
		ids1, sums1, total1 := toSummaries(open[:1])
		idsAll, sumsAll, totalAll := toSummaries(open)
		return []PaymentOption{
			{
				OptionType:  OptionOldest1,
				Label:       "Oldest Due (1 Month)",
				DueCount:    1,
				AmountPaise: total1,
				DueIDs:      ids1,
				Dues:        sums1,
			},
			{
				OptionType:  OptionAll,
				Label:       "Clear All Dues (2 Months)",
				DueCount:    2,
				AmountPaise: totalAll,
				DueIDs:      idsAll,
				Dues:        sumsAll,
			},
		}
	}

	// 3 or more open dues
	ids1, sums1, total1 := toSummaries(open[:1])
	ids2, sums2, total2 := toSummaries(open[:2])
	idsAll, sumsAll, totalAll := toSummaries(open)
	return []PaymentOption{
		{
			OptionType:  OptionOldest1,
			Label:       "Oldest Due (1 Month)",
			DueCount:    1,
			AmountPaise: total1,
			DueIDs:      ids1,
			Dues:        sums1,
		},
		{
			OptionType:  OptionOldest2,
			Label:       "Oldest 2 Months",
			DueCount:    2,
			AmountPaise: total2,
			DueIDs:      ids2,
			Dues:        sums2,
		},
		{
			OptionType:  OptionAll,
			Label:       fmt.Sprintf("Clear All Dues (%d Months)", n),
			DueCount:    n,
			AmountPaise: totalAll,
			DueIDs:      idsAll,
			Dues:        sumsAll,
		},
	}
}

// RecomputeDueStatusMath is the canonical single-source-of-truth status and remaining amount derivation.
func RecomputeDueStatusMath(netPaid int64, contractualCeiling int) (DueStatus, int) {
	if netPaid >= int64(contractualCeiling) {
		return DueStatusPaid, 0
	} else if netPaid > 0 {
		return DueStatusPartial, contractualCeiling - int(netPaid)
	} else {
		return DueStatusPending, contractualCeiling
	}
}
