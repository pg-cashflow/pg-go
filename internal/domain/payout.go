package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type PayeeType string

const (
	PayeeTypeStaff           PayeeType = "staff"
	PayeeTypeVendor          PayeeType = "vendor"
	PayeeTypeTenantDeposit   PayeeType = "tenant_deposit"
	PayeeTypeGuardianDeposit PayeeType = "guardian_deposit"
)

type PayoutPayee struct {
	ID                     uuid.UUID  `json:"id"`
	PropertyID             uuid.UUID  `json:"property_id"`
	PayeeType              PayeeType  `json:"payee_type"`
	Name                   string     `json:"name"`
	Phone                  *string    `json:"phone,omitempty"`
	AccountNumberEncrypted []byte     `json:"-"`
	AccountNumberLast4     *string    `json:"account_number_last4,omitempty"`
	AccountNumberHash      string     `json:"account_number_hash"`
	IFSC                   *string    `json:"ifsc,omitempty"`
	BankName               *string    `json:"bank_name,omitempty"`
	UPIVPA                 *string    `json:"upi_vpa,omitempty"`
	KeyVersion             int        `json:"key_version"`
	IsVerified             bool       `json:"is_verified"`
	VerifiedBy             *uuid.UUID `json:"verified_by,omitempty"`
	VerifiedAt             *time.Time `json:"verified_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

type DepartureStatus string

const (
	DeparturePending   DepartureStatus = "pending"
	DepartureInspected DepartureStatus = "inspected"
	DepartureApproved  DepartureStatus = "approved"
	DepartureRefunded  DepartureStatus = "refunded"
	DepartureCancelled DepartureStatus = "cancelled"
)

type DeductionStatus string

const (
	DeductionAgreed   DeductionStatus = "agreed"
	DeductionDisputed DeductionStatus = "disputed"
	DeductionWaived   DeductionStatus = "waived"
)

type TenantDeparture struct {
	ID                         uuid.UUID       `json:"id"`
	TenantID                   uuid.UUID       `json:"tenant_id"`
	PropertyID                 uuid.UUID       `json:"property_id"`
	NoticeGivenAt              time.Time       `json:"notice_given_at"`
	PlannedVacateDate          time.Time       `json:"planned_vacate_date"`
	ActualVacateDate           *time.Time      `json:"actual_vacate_date,omitempty"`
	InspectedAt                *time.Time      `json:"inspected_at,omitempty"`
	SLADeadlineAt              *time.Time      `json:"sla_deadline_at,omitempty"`
	DepositAmountPaise         int64           `json:"deposit_amount_paise"`
	UnusedRentRefundPaise      int64           `json:"unused_rent_refund_paise"`
	ProratedRentOwedPaise      int64           `json:"prorated_rent_owed_paise"`
	OutstandingDuesNettedPaise int64           `json:"outstanding_dues_netted_paise"`
	DeductionsPaise            int64           `json:"deductions_paise"`
	NetRefundPaise             int64           `json:"net_refund_paise"`
	ReceivableBalancePaise     int64           `json:"receivable_balance_paise"`
	Status                     DepartureStatus `json:"status"`
	Notes                      *string         `json:"notes,omitempty"`
	CreatedAt                  time.Time       `json:"created_at"`
	UpdatedAt                  time.Time       `json:"updated_at"`
}

type DepartureDeduction struct {
	ID                   uuid.UUID       `json:"id"`
	DepartureID          uuid.UUID       `json:"departure_id"`
	Description          string          `json:"description"`
	AmountPaise          int64           `json:"amount_paise"`
	EvidencePhotoKey     *string         `json:"evidence_photo_key,omitempty"`
	Status               DeductionStatus `json:"status"`
	TenantAcknowledgedAt *time.Time      `json:"tenant_acknowledged_at,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
}

type DepartureDueAdjustment struct {
	ID             uuid.UUID `json:"id"`
	DepartureID    uuid.UUID `json:"departure_id"`
	DueID          uuid.UUID `json:"due_id"`
	AmountPaise    int64     `json:"amount_paise"`
	AdjustmentType string    `json:"adjustment_type"` // unused_rent_reversal
	CreatedAt      time.Time `json:"created_at"`
}

type PayoutBatchStatus string

const (
	BatchDraft           PayoutBatchStatus = "draft"
	BatchApproved        PayoutBatchStatus = "approved"
	BatchProcessing      PayoutBatchStatus = "processing"
	BatchCompleted       PayoutBatchStatus = "completed"
	BatchPartiallyFailed PayoutBatchStatus = "partially_failed"
	BatchCancelled       PayoutBatchStatus = "cancelled"
)

type PayoutBatch struct {
	ID               uuid.UUID         `json:"id"`
	PropertyID       uuid.UUID         `json:"property_id"`
	BatchNumber      string            `json:"batch_number"`
	FormatType       string            `json:"format_type"` // instruction_sheet
	Status           PayoutBatchStatus `json:"status"`
	TotalAmountPaise int64             `json:"total_amount_paise"`
	ItemCount        int               `json:"item_count"`
	CreatedBy        uuid.UUID         `json:"created_by"`
	ApprovedBy       *uuid.UUID        `json:"approved_by,omitempty"`
	ApprovedAt       *time.Time        `json:"approved_at,omitempty"`
	FileChecksum     *string           `json:"file_checksum,omitempty"`
	Notes            *string           `json:"notes,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type PayoutItemStatus string

const (
	PayoutPending   PayoutItemStatus = "pending"
	PayoutSucceeded PayoutItemStatus = "succeeded"
	PayoutFailed    PayoutItemStatus = "failed"
	PayoutRejected  PayoutItemStatus = "rejected"
	PayoutCancelled PayoutItemStatus = "cancelled"
)

type PayoutItem struct {
	ID              uuid.UUID        `json:"id"`
	BatchID         *uuid.UUID       `json:"batch_id,omitempty"`
	PayeeID         uuid.UUID        `json:"payee_id"`
	DepartureID     *uuid.UUID       `json:"departure_id,omitempty"`
	ReferenceNumber string           `json:"reference_number"`
	AmountPaise     int64            `json:"amount_paise"`
	Purpose         string           `json:"purpose"`
	PeriodLabel     string           `json:"period_label"`
	Status          PayoutItemStatus `json:"status"`
	UTR             *string          `json:"utr,omitempty"`
	SettledAt       *time.Time       `json:"settled_at,omitempty"`
	FailureReason   *string          `json:"failure_reason,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// ComputeBatchChecksum computes HMAC-SHA256 over deterministic concatenation of payout items.
func ComputeBatchChecksum(secret []byte, items []PayoutItem) string {
	h := hmac.New(sha256.New, secret)
	for _, it := range items {
		depStr := ""
		if it.DepartureID != nil {
			depStr = it.DepartureID.String()
		}
		data := fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s\n",
			it.ID.String(), it.PayeeID.String(), depStr, it.AmountPaise, it.Purpose, it.PeriodLabel, it.ReferenceNumber,
		)
		h.Write([]byte(data))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeAccountHash computes SHA-256 hex string of bank account or UPI VPA for deduplication.
func ComputeAccountHash(salt []byte, identifier string) string {
	h := hmac.New(sha256.New, salt)
	h.Write([]byte(identifier))
	return hex.EncodeToString(h.Sum(nil))
}
