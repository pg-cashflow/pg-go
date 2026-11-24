package finance

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestDerivePayoutTransferID(t *testing.T) {
	itemID := uuid.MustParse("12345678-1234-1234-1234-123456789abc")
	cleanID := strings.ReplaceAll(itemID.String(), "-", "")

	// 1. Initial attempt
	item1 := domain.PayoutItem{
		ID: itemID,
	}
	expected1 := "pgo_" + cleanID
	if got := DerivePayoutTransferID(item1); got != expected1 {
		t.Fatalf("expected initial transfer ID %q, got %q", expected1, got)
	}

	// 2. Retry attempt (RetryOf != nil)
	retryOfID := uuid.New()
	item2 := domain.PayoutItem{
		ID:      itemID,
		RetryOf: &retryOfID,
	}
	expected2 := "pgo_" + cleanID + "_a2"
	if got := DerivePayoutTransferID(item2); got != expected2 {
		t.Fatalf("expected retry transfer ID %q, got %q", expected2, got)
	}
}

func TestPayoutDispatcher_ReconcileSingleTransfer_ExactIDEnforcement(t *testing.T) {
	// A nil repo dispatcher will panic if repo methods are invoked.
	// Since mismatched transfer IDs must be rejected before any status switch or repo call,
	// calling reconcileSingleTransfer with a mismatched transfer ID must return safely without panic.
	d := &PayoutDispatcher{}
	ctx := context.Background()
	batch := &domain.PayoutBatch{ID: uuid.New(), PropertyID: uuid.New()}

	retryOf := uuid.New()
	canonicalItem := domain.PayoutItem{
		ID:          uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		AmountPaise: 50000,
		RetryOf:     &retryOf, // Canonical transfer ID is pgo_aaaaaaaabbbbccccddddeeeeeeeeeeee_a2
	}
	canonicalID := DerivePayoutTransferID(canonicalItem)

	mismatchedCases := []struct {
		name        string
		incomingID  string
		status      string
		statusCode  string
	}{
		{
			name:        "wrong attempt suffix on SUCCESS",
			incomingID:  "pgo_aaaaaaaabbbbccccddddeeeeeeeeeeee_a1",
			status:      "SUCCESS",
			statusCode:  "COMPLETED",
		},
		{
			name:        "missing attempt suffix on SUCCESS",
			incomingID:  "pgo_aaaaaaaabbbbccccddddeeeeeeeeeeee",
			status:      "SUCCESS",
			statusCode:  "COMPLETED",
		},
		{
			name:        "missing prefix on SUCCESS",
			incomingID:  "aaaaaaaabbbbccccddddeeeeeeeeeeee_a2",
			status:      "SUCCESS",
			statusCode:  "COMPLETED",
		},
		{
			name:        "wrong attempt suffix on FAILED",
			incomingID:  "pgo_aaaaaaaabbbbccccddddeeeeeeeeeeee_a1",
			status:      "FAILED",
			statusCode:  "BANK_ERROR",
		},
		{
			name:        "wrong attempt suffix on APPROVAL_PENDING",
			incomingID:  "pgo_aaaaaaaabbbbccccddddeeeeeeeeeeee_a1",
			status:      "APPROVAL_PENDING",
			statusCode:  "APPROVAL_PENDING",
		},
		{
			name:        "wrong attempt suffix on REVERSED",
			incomingID:  "pgo_aaaaaaaabbbbccccddddeeeeeeeeeeee_a1",
			status:      "REVERSED",
			statusCode:  "REVERSED",
		},
		{
			name:        "wrong attempt suffix on REJECTED",
			incomingID:  "pgo_aaaaaaaabbbbccccddddeeeeeeeeeeee_a1",
			status:      "REJECTED",
			statusCode:  "REJECTED",
		},
		{
			name:        "whitespace padded transfer ID",
			incomingID:  " " + canonicalID + " ",
			status:      "FAILED",
			statusCode:  "FAILED",
		},
		{
			name:        "completely different UUID",
			incomingID:  "pgo_11111111222233334444555555555555_a2",
			status:      "FAILED",
			statusCode:  "FAILED",
		},
	}

	for _, tc := range mismatchedCases {
		t.Run(tc.name, func(t *testing.T) {
			st := &cashfree.TransferStatusItem{
				TransferID:   tc.incomingID,
				Status:       tc.status,
				StatusCode:   tc.statusCode,
				CFTransferID: "cf_mock_123",
			}
			// Must safely refuse mutation and return nil without panicking on nil repo
			err := d.reconcileSingleTransfer(ctx, batch, canonicalItem, st)
			if err != nil {
				t.Fatalf("expected nil error from rejected transfer ID, got: %v", err)
			}
		})
	}
}
