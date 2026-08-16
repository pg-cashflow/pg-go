package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type EventType string

const (
	EvtDueCreated             EventType = "DueCreated"
	EvtDuePaidOnTime          EventType = "DuePaidOnTime"
	EvtDuePaidLate            EventType = "DuePaidLate"
	EvtDueWaived              EventType = "DueWaived"
	EvtDueProrated            EventType = "DueProrated"
	EvtTenantCreated          EventType = "TenantCreated"
	EvtTenantVacated          EventType = "TenantVacated"
	EvtNoticeGiven            EventType = "NoticeGiven"
	EvtPhoneAttached          EventType = "PhoneAttached"
	EvtRentAmountChanged      EventType = "RentAmountChanged"
	EvtPaymentMatched         EventType = "PaymentMatched"
	EvtPaymentMatchFailed     EventType = "PaymentMatchFailed"
	EvtCashPaymentRecorded    EventType = "CashPaymentRecorded"
	EvtCreditApplied          EventType = "CreditApplied"
	EvtQRViewed               EventType = "QRViewed"
	EvtConsentGiven           EventType = "ConsentGiven"
	EvtDepositTermsAccepted   EventType = "DepositTermsAccepted"
	EvtDepositSettled         EventType = "DepositSettled"
	EvtReminderSent           EventType = "ReminderSent"
	EvtReminderFailed         EventType = "ReminderFailed"
	EvtFinancialSummarySent   EventType = "FinancialSummarySent"
	EvtJoinRequested          EventType = "JoinRequested"
	EvtJoinApproved           EventType = "JoinApproved"
	EvtJoinRejected           EventType = "JoinRejected"
	EvtPaymentReportSubmitted EventType = "PaymentReportSubmitted"
	EvtPaymentReportRejected  EventType = "PaymentReportRejected"
)

type Event struct {
	ID         int64           `json:"-"`
	TenantID   *uuid.UUID      `json:"tenant_id,omitempty"` // nil for property-scoped events
	PropertyID uuid.UUID       `json:"property_id"`
	EventType  EventType       `json:"event_type"`
	DueID      *uuid.UUID      `json:"due_id,omitempty"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

type DuePaidPayload struct {
	DueID           string `json:"due_id"`
	DueCode         string `json:"due_code"`
	AmountPaise     int64  `json:"amount_paise"`
	PaidAt          string `json:"paid_at"`
	DaysEarlyOrLate int    `json:"days_early_or_late"`
	MatchedBy       string `json:"matched_by"`
}

type DueProratedPayload struct {
	DueID         string `json:"due_id"`
	OriginalPaise int64  `json:"original_amount_paise"`
	ProratedPaise int64  `json:"prorated_amount_paise"`
	DaysOccupied  int    `json:"days_occupied"`
	DaysInPeriod  int    `json:"days_in_period"`
}

type NoticeGivenPayload struct {
	TenantID         string `json:"tenant_id"`
	NoticeGivenAt    string `json:"notice_given_at"`
	NoticePeriodDays int    `json:"notice_period_days"`
	ExpectedVacateBy string `json:"expected_vacate_by"`
}

type DepositTermsAcceptedPayload struct {
	Channel          string `json:"channel"`
	NoticePeriodDays int    `json:"notice_period_days"`
}

type DepositSettledPayload struct {
	DepositDueID    string `json:"deposit_due_id"`
	OriginalPaise   int64  `json:"original_amount_paise"`
	RefundedPaise   int64  `json:"refunded_amount_paise"`
	NoticeDaysGiven int    `json:"notice_days_given"`
	PolicyMet       bool   `json:"policy_met"`
	Reason          string `json:"reason"`
}

type RentAmountChangedPayload struct {
	TenantID      string `json:"tenant_id"`
	OldPaise      int64  `json:"old_rent_paise"`
	NewPaise      int64  `json:"new_rent_paise"`
	EffectiveFrom string `json:"effective_from"`
}

type CashPaymentRecordedPayload struct {
	DueID       string `json:"due_id"`
	DueCode     string `json:"due_code"`
	AmountPaise int64  `json:"amount_paise"`
	RecordedBy  string `json:"recorded_by_user_id"`
	Note        string `json:"note,omitempty"`
}

type CreditAppliedPayload struct {
	TenantID       string `json:"tenant_id"`
	DueID          string `json:"due_id"`
	CreditPaise    int64  `json:"credit_applied_paise"`
	RemainingPaise int64  `json:"remaining_credit_paise"`
}

type QRViewedPayload struct {
	DueID   string `json:"due_id"`
	TokenID string `json:"token_id"`
	IPHash  string `json:"ip_hash"`
}

type FinancialSummarySentPayload struct {
	Period  string `json:"period"`
	Cadence string `json:"cadence"`
	SentTo  string `json:"sent_to"`
}

type DueCreatedPayload struct {
	DueID       string `json:"due_id"`
	DueCode     string `json:"due_code"`
	Kind        string `json:"kind"`
	AmountPaise int64  `json:"amount_paise"`
}

type ReminderPayload struct {
	DueID        string `json:"due_id"`
	ReminderType string `json:"reminder_type"`
	Channel      string `json:"channel"`
	Error        string `json:"error,omitempty"`
}

type PaymentMatchedPayload struct {
	DueID       string `json:"due_id"`
	PaymentID   string `json:"payment_id"`
	MatchedBy   string `json:"matched_by"`
	AmountPaise int64  `json:"amount_paise"`
}

type PhoneAttachedPayload struct {
	TenantID string `json:"tenant_id"`
	Phone    string `json:"phone"`
}

type ConsentGivenPayload struct {
	Channel string `json:"channel"`
	Purpose string `json:"purpose"`
}
