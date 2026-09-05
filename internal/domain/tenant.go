package domain

import (
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
	ID                 uuid.UUID    `json:"id"`
	PropertyID         uuid.UUID    `json:"property_id"`
	Name               string       `json:"name"`
	Phone              *string      `json:"phone,omitempty"`
	RoomNumber         *string      `json:"room_number,omitempty"`
	RoomID             *uuid.UUID   `json:"room_id,omitempty"`
	AadhaarLast4       *string      `json:"aadhaar_last4,omitempty"`
	RentAmount         int          `json:"rent_amount"` // paise; 0 while pending_allocation
	DueDay             *int16       `json:"due_day,omitempty"`
	NoticePeriodDays   int16        `json:"notice_period_days"`
	NoticeGivenAt      *time.Time   `json:"notice_given_at,omitempty"`
	CreditBalancePaise int          `json:"credit_balance_paise"`
	Status             TenantStatus `json:"status"`
	PermanentAddress   string       `json:"permanent_address,omitempty"`
	CurrentAddress     string       `json:"current_address,omitempty"`
	ParentName         string       `json:"parent_name,omitempty"`
	EmergencyPhone     string       `json:"emergency_phone,omitempty"`
	JoinedOn           *time.Time   `json:"joined_on,omitempty"` // date only
	HasIDPhoto         bool         `json:"has_id_photo"`
	IDPhotoBytes       []byte       `json:"-"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
}

func (t Tenant) IsPhoneLess() bool {
	return t.Phone == nil || *t.Phone == ""
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

type NewTenantInput struct {
	PropertyID       uuid.UUID
	Name             string
	Phone            *string
	RoomNumber       *string
	RoomID           *uuid.UUID
	AadhaarLast4     *string
	RentAmount       int
	DueDay           int16
	NoticePeriodDays int16
	PermanentAddress string
	CurrentAddress   string
	ParentName       string
	EmergencyPhone   string
	JoinedOn         *time.Time
	IDPhotoBytes     []byte
}
