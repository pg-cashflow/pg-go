package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

func TestRefundApprovalScopeRequiresVerifiedProperty(t *testing.T) {
	if _, err := refundApprovalScope(context.Background(), uuid.Nil); !errors.Is(err, ErrRefundApprovalScopeRequired) {
		t.Fatalf("scope error = %v, want ErrRefundApprovalScopeRequired", err)
	}
}

func TestRefundApprovalScopeRejectsPropertyMismatch(t *testing.T) {
	verified := uuid.New()
	other := uuid.New()
	ctx := requestscope.WithPropertyID(context.Background(), verified)
	if _, err := refundApprovalScope(ctx, other); !errors.Is(err, ErrRefundApprovalScopeMismatch) {
		t.Fatalf("scope error = %v, want ErrRefundApprovalScopeMismatch", err)
	}
}

func TestRefundApprovalScopeAcceptsVerifiedProperty(t *testing.T) {
	verified := uuid.New()
	ctx := requestscope.WithPropertyID(context.Background(), verified)
	got, err := refundApprovalScope(ctx, verified)
	if err != nil {
		t.Fatalf("refundApprovalScope returned error: %v", err)
	}
	if got != verified {
		t.Fatalf("property ID = %s, want %s", got, verified)
	}
}
