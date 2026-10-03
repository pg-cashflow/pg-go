package search

import (
	"errors"

	"github.com/google/uuid"
)

var (
	ErrQueryRequired = errors.New("query required")
	ErrQueryTooShort = errors.New("query too short")
	ErrQueryTooLong  = errors.New("query too long")
)

// EntityType identifies a searchable hit kind.
type EntityType string

const (
	TypeTenant          EntityType = "tenant"
	TypeDue             EntityType = "due"
	TypePayment         EntityType = "payment"
	TypePaymentReport   EntityType = "payment_report"
	TypeJoinRequest     EntityType = "join_request"
	TypeEvent           EntityType = "event"
	TypeInspection      EntityType = "inspection"
	TypeHazard          EntityType = "hazard"
	TypeViolation       EntityType = "violation"
	TypeDocument        EntityType = "document"
	TypeBankTransaction EntityType = "bank_transaction"
	TypeSettlement      EntityType = "settlement"
	TypePayout          EntityType = "payout"
	TypeRefund          EntityType = "refund"
)

// Mode selects the search strategy. ADR-012: only ModeLexical is active; ModeHybrid is removed.
type Mode string

const ModeLexical Mode = "lexical"

// Result is one federated search hit returned to the client.
type Result struct {
	Type     EntityType `json:"type"`
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Subtitle string     `json:"subtitle"`
	Path     string     `json:"path"`
	Score    float64    `json:"-"`
}

// Params are validated search inputs after RBAC scope is applied.
type Params struct {
	Query      string
	Tokens     []Token
	Limit      int
	PerType    int
	Mode       Mode
	Types      []EntityType
	PropertyID uuid.UUID
	TenantID   *uuid.UUID // set for tenant role scoping
	Role       string
}
