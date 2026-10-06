package domain

import (
	"time"

	"github.com/google/uuid"
)

type DepositSettlementStatus string

const (
	DepositSettlementSettling DepositSettlementStatus = "settling"
	DepositSettlementSettled  DepositSettlementStatus = "settled"
	DepositSettlementFailed   DepositSettlementStatus = "failed"
)

type DepositSettlement struct {
	ID                   uuid.UUID               `json:"id"`
	PropertyID           uuid.UUID               `json:"property_id"`
	TenantID             uuid.UUID               `json:"tenant_id"`
	DepositDueID         uuid.UUID               `json:"deposit_due_id"`
	IdempotencyKey       string                  `json:"idempotency_key"`
	OriginalDepositPaise int64                   `json:"original_deposit_paise"`
	RefundedPaise        int64                   `json:"refunded_paise"`
	DeductionsPaise      int64                   `json:"deductions_paise"`
	Status               DepositSettlementStatus `json:"status"`
	Reason               string                  `json:"reason"`
	SettledAt            time.Time               `json:"settled_at"`
	CreatedAt            time.Time               `json:"created_at"`
	UpdatedAt            time.Time               `json:"updated_at"`
}

type DepositSettlementMirrorPayload struct {
	PropertyID           uuid.UUID `json:"property_id"`
	SettlementID         uuid.UUID `json:"settlement_id"`
	TenantID             uuid.UUID `json:"tenant_id"`
	DepositDueID         uuid.UUID `json:"deposit_due_id"`
	OriginalDepositPaise int64     `json:"original_deposit_paise"`
	RefundedPaise        int64     `json:"refunded_paise"`
	DeductionsPaise      int64     `json:"deductions_paise"`
	OccurredAt           time.Time `json:"occurred_at"`
}
