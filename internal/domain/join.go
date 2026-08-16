package domain

import (
	"time"

	"github.com/google/uuid"
)

type JoinStatus string

const (
	JoinPending  JoinStatus = "pending"
	JoinApproved JoinStatus = "approved"
	JoinRejected JoinStatus = "rejected"
)

type JoinRequest struct {
	ID           uuid.UUID  `json:"id"`
	PropertyID   uuid.UUID  `json:"property_id"`
	UserID       uuid.UUID  `json:"user_id"`
	Phone        string     `json:"phone"`
	Name         string     `json:"name"`
	AadhaarLast4 *string    `json:"aadhaar_last4,omitempty"`
	Status       JoinStatus `json:"status"`
	TenantID     *uuid.UUID `json:"tenant_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type PaymentReportStatus string

const (
	ReportPendingReview PaymentReportStatus = "pending_review"
	ReportConfirmed     PaymentReportStatus = "confirmed"
	ReportRejected      PaymentReportStatus = "rejected"
)

type PaymentReport struct {
	ID          uuid.UUID           `json:"id"`
	DueID       uuid.UUID           `json:"due_id"`
	TenantID    uuid.UUID           `json:"tenant_id"`
	PropertyID  uuid.UUID           `json:"property_id"`
	UPITxnID    string              `json:"upi_txn_id"`
	Amount      int                 `json:"amount"`
	HasImage    bool                `json:"has_image"`
	Status      PaymentReportStatus `json:"status"`
	ReportedBy  uuid.UUID           `json:"reported_by"`
	ReviewedBy  *uuid.UUID          `json:"reviewed_by,omitempty"`
	ReviewedAt  *time.Time          `json:"reviewed_at,omitempty"`
	Note        *string             `json:"note,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	ImageBytes  []byte              `json:"-"`
}

type PaymentIntentStatus string

const (
	IntentCreated PaymentIntentStatus = "created"
	IntentPaid    PaymentIntentStatus = "paid"
	IntentExpired PaymentIntentStatus = "expired"
	IntentFailed  PaymentIntentStatus = "failed"
)

type PaymentIntent struct {
	ID                uuid.UUID           `json:"id"`
	DueID             uuid.UUID           `json:"due_id"`
	Provider          string              `json:"provider"`
	ProviderOrderID   string              `json:"provider_order_id"`
	PaymentSessionID  *string             `json:"payment_session_id,omitempty"`
	AmountPaise       int                 `json:"amount_paise"`
	Status            PaymentIntentStatus `json:"status"`
	ExpiresAt         *time.Time          `json:"expires_at,omitempty"`
	CFPaymentID       *string             `json:"cf_payment_id,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
}

type PayIntent struct {
	Mode              string `json:"mode"` // manual | cashfree
	VPA               string `json:"vpa,omitempty"`
	UPILink           string `json:"upi_link,omitempty"`
	Note              string `json:"note"`
	DueCode           string `json:"due_code"`
	AmountPaise       int    `json:"amount_paise"`
	QRPNGURL          string `json:"qr_png_url"`
	Payable           bool   `json:"payable"`
	PaymentSessionID  string `json:"payment_session_id,omitempty"`
}
