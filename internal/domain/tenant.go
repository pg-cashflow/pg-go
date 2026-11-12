package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type TenantStatus string

const (
	TenantStatusPendingAllocation TenantStatus = "pending_allocation"
	TenantStatusActive            TenantStatus = "active"
	TenantStatusVacated           TenantStatus = "vacated"
)

type Tenant struct {
	ID                        uuid.UUID       `json:"id"`
	PropertyID                uuid.UUID       `json:"property_id"`
	Name                      string          `json:"name"`
	Phone                     *string         `json:"phone,omitempty"`
	RoomNumber                *string         `json:"room_number,omitempty"`
	RoomID                    *uuid.UUID      `json:"room_id,omitempty"`
	AadhaarLast4              *string         `json:"aadhaar_last4,omitempty"`
	RentAmount                int64           `json:"rent_amount"` // paise; 0 while pending_allocation
	DueDay                    *int16          `json:"due_day,omitempty"`
	NoticePeriodDays          int16           `json:"notice_period_days"`
	NoticeGivenAt             *time.Time      `json:"notice_given_at,omitempty"`
	CreditBalancePaise        int64           `json:"credit_balance_paise"`
	Status                    TenantStatus    `json:"status"`
	PermanentAddress          string          `json:"permanent_address,omitempty"`
	CurrentAddress            string          `json:"current_address,omitempty"`
	ParentName                string          `json:"parent_name,omitempty"`
	EmergencyPhone            string          `json:"emergency_phone,omitempty"`
	JoinedOn                  *time.Time      `json:"joined_on,omitempty"` // date only
	HasIDPhoto                bool            `json:"has_id_photo"`
	IDPhotoBytes              []byte          `json:"-"`
	MajorityDate              *time.Time      `json:"majority_date,omitempty"`
	GuardianName              *string         `json:"guardian_name,omitempty"`
	GuardianPhone             *string         `json:"guardian_phone,omitempty"`
	GuardianRelation          *string         `json:"guardian_relation,omitempty"`
	GuardianKYCReferenceID    *string         `json:"guardian_kyc_reference_id,omitempty"`
	GuardianConsentVerifiedAt *time.Time      `json:"guardian_consent_verified_at,omitempty"`
	GuardianConsentMethod     *string         `json:"guardian_consent_method,omitempty"`
	IsGamificationDisabled    bool            `json:"is_gamification_disabled"`
	OccupationType            OccupationType  `json:"occupation_type,omitempty"`
	DateOfBirth               *time.Time      `json:"date_of_birth,omitempty"`
	OccupationDetails         json.RawMessage `json:"occupation_details,omitempty"`
	EmergencyContactName      string          `json:"emergency_contact_name,omitempty"`
	CreatedAt                 time.Time       `json:"created_at"`
	UpdatedAt                 time.Time       `json:"updated_at"`
}

// AgeState represents the age classification of a tenant.
type AgeState string

const (
	AgeStateUnknown AgeState = "unknown"
	AgeStateMinor   AgeState = "minor"
	AgeStateAdult   AgeState = "adult"
)

// AgeStatus determines whether the tenant is positively confirmed as an adult,
// positively confirmed as a minor, or has an unknown age.
// Unknown is NEVER treated as adult.
func (t Tenant) AgeStatus(now time.Time) AgeState {
	if t.MajorityDate == nil {
		return AgeStateUnknown
	}
	if t.MajorityDate.After(now) {
		return AgeStateMinor
	}
	return AgeStateAdult
}

func (t Tenant) IsMinor(now time.Time) bool {
	return t.AgeStatus(now) == AgeStateMinor
}

func (t Tenant) GamificationActive(now time.Time) bool {
	if t.IsGamificationDisabled || t.IsMinor(now) {
		return false
	}
	return true
}

func (t Tenant) IsPhoneLess() bool {

	return t.Phone == nil || *t.Phone == ""
}

// CanSignIn returns true if tenant status permits authentication and session renewal.
func (t *Tenant) CanSignIn() bool {
	if t == nil {
		return false
	}
	switch t.Status {
	case TenantStatusActive, TenantStatusPendingAllocation:
		return true
	default:
		return false
	}
}

func (t Tenant) ContactMode() string {
	if t.IsPhoneLess() {
		return "cash_only"
	}
	return "self_service"
}

func (t Tenant) IsPayable() bool {
	return t.Status == TenantStatusActive
}

// Payable returns whether the tenant may initiate rent payment (status + optional KYC gate).
func (t Tenant) Payable(requireKYCBeforePay bool, kycVerified bool) bool {
	if !t.IsPayable() {
		return false
	}
	if requireKYCBeforePay && !kycVerified {
		return false
	}
	return true
}

type NewTenantInput struct {
	PropertyID           uuid.UUID
	Name                 string
	Phone                *string
	RoomNumber           *string
	RoomID               *uuid.UUID
	AadhaarLast4         *string
	RentAmount           int64
	DueDay               int16
	NoticePeriodDays     int16
	PermanentAddress     string
	CurrentAddress       string
	ParentName           string
	EmergencyPhone       string
	JoinedOn             *time.Time
	IDPhotoBytes         []byte
	OccupationType       OccupationType
	DateOfBirth          *time.Time
	OccupationDetails    json.RawMessage
	EmergencyContactName string
	GuardianName         *string
	GuardianPhone        *string
	GuardianRelation     *string
}
