package domain

import (
	"encoding/json"
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
	ID                   uuid.UUID       `json:"id"`
	PropertyID           uuid.UUID       `json:"property_id"`
	UserID               uuid.UUID       `json:"user_id"`
	Phone                string          `json:"phone"`
	Name                 string          `json:"name"`
	AadhaarLast4         *string         `json:"aadhaar_last4,omitempty"`
	PermanentAddress     string          `json:"permanent_address,omitempty"`
	CurrentAddress       string          `json:"current_address,omitempty"`
	ParentName           string          `json:"parent_name,omitempty"`
	EmergencyPhone       string          `json:"emergency_phone,omitempty"`
	JoinedOn             *time.Time      `json:"joined_on,omitempty"`
	Status               JoinStatus      `json:"status"`
	TenantID             *uuid.UUID      `json:"tenant_id,omitempty"`
	SlotInviteID         *uuid.UUID      `json:"slot_invite_id,omitempty"`
	OccupationType       OccupationType  `json:"occupation_type,omitempty"`
	DateOfBirth          *time.Time      `json:"date_of_birth,omitempty"`
	ProfileSchemaVersion int             `json:"profile_schema_version"`
	OccupationDetails    json.RawMessage `json:"occupation_details,omitempty"`
	EmergencyContactName string          `json:"emergency_contact_name,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

type PaymentReportStatus string

const (
	ReportPendingReview PaymentReportStatus = "pending_review"
	ReportConfirmed     PaymentReportStatus = "confirmed"
	ReportRejected      PaymentReportStatus = "rejected"
)

type PaymentReport struct {
	ID            uuid.UUID           `json:"id"`
	DueID         uuid.UUID           `json:"due_id"`
	TenantID      uuid.UUID           `json:"tenant_id"`
	PropertyID    uuid.UUID           `json:"property_id"`
	UPITxnID      string              `json:"upi_txn_id"`
	Amount        int64               `json:"amount"`
	HasImage      bool                `json:"has_image"`
	Status        PaymentReportStatus `json:"status"`
	ReportedBy    uuid.UUID           `json:"reported_by"`
	ReviewedBy    *uuid.UUID          `json:"reviewed_by,omitempty"`
	ReviewedAt    *time.Time          `json:"reviewed_at,omitempty"`
	Note          *string             `json:"note,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	ImageBytes    []byte              `json:"-"`
	ImageHash     *string             `json:"image_hash,omitempty"`
	IsDuplicate   bool                `json:"is_duplicate"`
	OCRAmount     *int64              `json:"ocr_amount,omitempty"`
	OCRUTR        *string             `json:"ocr_utr,omitempty"`
	OCRTxnDate    *time.Time          `json:"ocr_txn_date,omitempty"`
	OCRConfidence *float32            `json:"ocr_confidence,omitempty"`
}

type PaymentIntentStatus string

const (
	IntentInitiating      PaymentIntentStatus = "initiating"
	IntentCreated         PaymentIntentStatus = "created"
	IntentPaid            PaymentIntentStatus = "paid"
	IntentExpired         PaymentIntentStatus = "expired"
	IntentFailed          PaymentIntentStatus = "failed"
	IntentSuperseded      PaymentIntentStatus = "superseded"
	IntentDispatchUnknown PaymentIntentStatus = "dispatch_unknown"
	IntentNeedsReview     PaymentIntentStatus = "needs_review"
)

type PaymentIntentDue struct {
	ID              uuid.UUID           `json:"id"`
	PaymentIntentID uuid.UUID           `json:"payment_intent_id"`
	DueID           uuid.UUID           `json:"due_id"`
	AmountPaise     int64               `json:"amount_paise"`
	Status          PaymentIntentStatus `json:"status"`
	CreatedAt       time.Time           `json:"created_at"`
}

type PaymentIntent struct {
	ID               uuid.UUID           `json:"id"`
	PropertyID       uuid.UUID           `json:"-"`
	DueID            uuid.UUID           `json:"due_id"`
	Provider         string              `json:"provider"`
	ProviderOrderID  string              `json:"provider_order_id"`
	PaymentSessionID *string             `json:"payment_session_id,omitempty"`
	AmountPaise      int64               `json:"amount_paise"`
	Status           PaymentIntentStatus `json:"status"`
	ExpiresAt        *time.Time          `json:"expires_at,omitempty"`
	CFPaymentID      *string             `json:"cf_payment_id,omitempty"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
	IdempotencyKey   uuid.UUID           `json:"idempotency_key"`
}

// PaymentIntentQuarantine is a durable provider-order recovery record. One row
// represents one Cashfree order and lists every due that remains reserved.
type PaymentIntentQuarantine struct {
	ID                 int64         `json:"id"`
	PaymentIntentID    uuid.UUID     `json:"payment_intent_id"`
	DueID              uuid.UUID     `json:"due_id"`
	PropertyID         uuid.UUID     `json:"property_id"`
	DueIDs             []uuid.UUID   `json:"due_ids"`
	ProviderOrderID    string        `json:"provider_order_id"`
	AmountPaise        int64         `json:"amount_paise"`
	OriginalStatus     string        `json:"original_status"`
	QuarantineReason   string        `json:"quarantine_reason"`
	Status             string        `json:"status"`
	NextCheckAt        time.Time     `json:"next_check_at"`
	AttemptCount       int           `json:"attempt_count"`
	LastProviderStatus *string       `json:"last_provider_status,omitempty"`
	LastError          *string       `json:"last_error,omitempty"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
	Intent             PaymentIntent `json:"-"`
}

type PayIntent struct {
	Mode        string `json:"mode"` // manual | cashfree
	VPA         string `json:"vpa,omitempty"`
	UPILink     string `json:"upi_link,omitempty"`
	Note        string `json:"note"`
	DueCode     string `json:"due_code"`
	AmountPaise int64  `json:"amount_paise"`
	QRPNGURL    string `json:"qr_png_url,omitempty"`
	Payable     bool   `json:"payable"`
	// AvailabilityStatus gives a typed, non-PII reason when Payable is false
	// (e.g. PAID, WAIVED, KYC_REQUIRED, GUARDIAN_CONSENT_REQUIRED,
	// AGE_VERIFICATION_REQUIRED, NO_OPEN_DUES, TEMPORARILY_UNAVAILABLE).
	AvailabilityStatus string      `json:"availability_status,omitempty"`
	OrderID            string      `json:"order_id,omitempty"`
	PaymentSessionID   string      `json:"payment_session_id,omitempty"`
	DueCount           int         `json:"due_count,omitempty"`
	DueIDs             []uuid.UUID `json:"due_ids,omitempty"`
	// FallbackReason is set when the property is in cashfree mode but the backend
	// fell back to manual. Possible values: "missing_phone", "gateway_down".
	FallbackReason string `json:"fallback_reason,omitempty"`
}
