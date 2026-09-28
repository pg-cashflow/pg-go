package domain

import (
	"time"

	"github.com/google/uuid"
)

type BankAccountType string

const (
	BankAccountTypeSavings BankAccountType = "savings"
	BankAccountTypeCurrent BankAccountType = "current"
)

type BankAccount struct {
	ID                 uuid.UUID       `json:"id"`
	PropertyID         uuid.UUID       `json:"property_id"`
	BankName           string          `json:"bank_name"`
	AccountType        BankAccountType `json:"account_type"`
	AccountNumberLast4 string          `json:"account_number_last4"`
	Label              string          `json:"label"`
	StatementProfile   string          `json:"statement_profile"` // e.g. "sbi", "hdfc", "generic"
	IsActive           bool            `json:"is_active"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}
