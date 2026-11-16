package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrMakerCannotApprove      = errors.New("maker cannot approve own refund request; self-approval prohibited")
	ErrDuplicateApprover       = errors.New("one owner cannot satisfy both approvals; distinct approver required")
	ErrStaleApproval           = errors.New("stale approval: material transaction details changed; approval invalidated")
	ErrRefundRequestNotPending = errors.New("refund approval request is not pending")
	ErrCrossPropertyApproval   = errors.New("approver does not belong to the property")
	ErrUserRevoked             = errors.New("user is not active or authorized to approve")
	ErrThresholdUnresolved     = errors.New("single-owner authorization threshold is unresolved")
)

const (
	RefundApprovalPending           = "pending"
	RefundApprovalPartiallyApproved = "partially_approved"
	RefundApprovalApproved          = "approved"
	RefundApprovalRejected          = "rejected"
	RefundApprovalDispatched        = "dispatched"
	RefundApprovalCancelled         = "cancelled"
)

// RefundApprovalDecision records an individual owner's immutable approval of a refund request.
type RefundApprovalDecision struct {
	ApproverID   uuid.UUID `json:"approver_id"`
	ApproverRole string    `json:"approver_role"`
	ApprovedAt   time.Time `json:"approved_at"`
	StepUpMethod string    `json:"step_up_method"`
	SnapshotHash string    `json:"snapshot_hash"`
	Note         string    `json:"note,omitempty"`
}

// RefundApprovalRequest represents a financial maker-checker request for a payment refund.
type RefundApprovalRequest struct {
	ID                uuid.UUID                `json:"id"`
	PropertyID        uuid.UUID                `json:"property_id"`
	PaymentID         uuid.UUID                `json:"payment_id"`
	TenantID          uuid.UUID                `json:"tenant_id"`
	AmountPaise       int64                    `json:"amount_paise"`
	Reason            string                   `json:"reason"`
	IdempotencyKey    string                   `json:"idempotency_key"`
	RequestedBy       uuid.UUID                `json:"requested_by"`
	RequestedByRole   string                   `json:"requested_by_role"`
	Status            string                   `json:"status"`
	RequiredApprovals int                      `json:"required_approvals"`
	MaterialHash      string                   `json:"material_hash"`
	Approvals         []RefundApprovalDecision `json:"approvals"`
	RejectedBy        *uuid.UUID               `json:"rejected_by,omitempty"`
	RejectedAt        *time.Time               `json:"rejected_at,omitempty"`
	RejectionReason   *string                  `json:"rejection_reason,omitempty"`
	RefundID          *uuid.UUID               `json:"refund_id,omitempty"`
	CreatedAt         time.Time                `json:"created_at"`
	UpdatedAt         time.Time                `json:"updated_at"`
}

// ComputeRefundMaterialHash computes a deterministic SHA-256 hash over immutable transaction details.
func ComputeRefundMaterialHash(propertyID, paymentID, tenantID uuid.UUID, amountPaise int64, reason string) string {
	raw := fmt.Sprintf("prop:%s|pay:%s|ten:%s|amt:%d|rsn:%s",
		propertyID.String(),
		paymentID.String(),
		tenantID.String(),
		amountPaise,
		strings.TrimSpace(reason),
	)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// EvaluateRefundThreshold evaluates whether a refund amount permits single-owner authorization
// or requires dual-owner authorization under maker-checker rules.
// Rules:
// 1. In production, an unconfigured / unresolved threshold prohibits single-owner authorization.
// 2. An explicitly configured threshold requires 2 distinct owner approvals for any refund exceeding it.
// 3. A refund within an explicitly configured threshold permits single-owner authorization.
func EvaluateRefundThreshold(amountPaise int64, policyLimit *int64, appEnv string) (requiredApprovals int, singleOwnerAllowed bool) {
	if strings.EqualFold(appEnv, "production") && policyLimit == nil {
		return 2, false
	}
	if policyLimit != nil && amountPaise > *policyLimit {
		return 2, false
	}
	return 1, true
}
