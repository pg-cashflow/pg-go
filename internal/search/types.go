package search

import (
	"github.com/google/uuid"
)

// EntityType identifies a searchable hit kind.
type EntityType string

const (
	TypeTenant        EntityType = "tenant"
	TypeDue           EntityType = "due"
	TypePayment       EntityType = "payment"
	TypePaymentReport EntityType = "payment_report"
	TypeJoinRequest   EntityType = "join_request"
	TypeEvent         EntityType = "event"
	TypeInspection    EntityType = "inspection"
	TypeHazard        EntityType = "hazard"
	TypeViolation     EntityType = "violation"
	TypeDocument      EntityType = "document"
)

// Mode selects lexical-only or hybrid (lexical + vector RRF).
type Mode string

const (
	ModeLexical Mode = "lexical"
	ModeHybrid  Mode = "hybrid"
)

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
