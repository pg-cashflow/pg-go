package domain

import (
	"time"

	"github.com/google/uuid"
)

type TenantStatus string

const (
	TenantStatusActive  TenantStatus = "active"
	TenantStatusVacated TenantStatus = "vacated"
)

type Tenant struct {
	ID                 uuid.UUID    `json:"id"`
	PropertyID         uuid.UUID    `json:"property_id"`
	Name               string       `json:"name"`
	Phone              *string      `json:"phone,omitempty"`
	RoomNumber         *string      `json:"room_number,omitempty"`
	AadhaarLast4       *string      `json:"aadhaar_last4,omitempty"`
	RentAmount         int          `json:"rent_amount"` // paise
	DueDay             int16        `json:"due_day"`
	NoticePeriodDays   int16        `json:"notice_period_days"`
	NoticeGivenAt      *time.Time   `json:"notice_given_at,omitempty"`
	CreditBalancePaise int          `json:"credit_balance_paise"`
	Status             TenantStatus `json:"status"`
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

type NewTenantInput struct {
	PropertyID       uuid.UUID
	Name             string
	Phone            *string
	RoomNumber       *string
	RentAmount       int
	DueDay           int16
	NoticePeriodDays int16
}
