package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestSettlementDecision_PropertyTests(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	now := time.Now().UTC()

	t.Run("terminal states stay terminal", func(t *testing.T) {
		revRow := &domain.GatewaySettlement{
			SettlementStatus:     "REVERSED",
			ReconciliationStatus: domain.ReconDiscrepancy,
			DiscrepancyReason:    strPtr("post_reconciliation_reversal"),
			GrossAmountPaise:     100000,
		}
		// Try overwriting REVERSED with SUCCESS, PENDING, FAILED
		for _, incomingStatus := range []string{"SUCCESS", "PENDING", "FAILED"} {
			incoming := &domain.GatewaySettlement{
				SettlementStatus:     incomingStatus,
				ReconciliationStatus: domain.ReconMatched,
				GrossAmountPaise:     100000,
			}
			decided := DecideSettlementUpdate(revRow, incoming)
			if decided.SettlementStatus != "REVERSED" {
				t.Fatalf("expected REVERSED to stay terminal against %s, got %s", incomingStatus, decided.SettlementStatus)
			}
		}

		succRow := &domain.GatewaySettlement{
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			GrossAmountPaise:     100000,
		}
		for _, incomingStatus := range []string{"PENDING", "FAILED"} {
			incoming := &domain.GatewaySettlement{
				SettlementStatus:     incomingStatus,
				ReconciliationStatus: domain.ReconMatched,
				GrossAmountPaise:     100000,
			}
			decided := DecideSettlementUpdate(succRow, incoming)
			if decided.SettlementStatus != "SUCCESS" {
				t.Fatalf("expected SUCCESS not to regress to %s, got %s", incomingStatus, decided.SettlementStatus)
			}
		}

		failedRow := &domain.GatewaySettlement{
			SettlementStatus: "FAILED",
			GrossAmountPaise: 100000,
		}
		pendingIncoming := &domain.GatewaySettlement{
			SettlementStatus: "PENDING",
			GrossAmountPaise: 100000,
		}
		if decided := DecideSettlementUpdate(failedRow, pendingIncoming); decided.SettlementStatus != "FAILED" {
			t.Fatalf("expected FAILED not to regress to PENDING, got %s", decided.SettlementStatus)
		}
	})

	t.Run("discrepancy leaves only by operator action (sticky discrepancy)", func(t *testing.T) {
		initialDiscrepancy := &domain.GatewaySettlement{
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconDiscrepancy,
			DiscrepancyReason:    strPtr("amount_changed_after_reconciliation"),
			GrossAmountPaise:     500000,
			NetAmountPaise:       500000,
		}

		// Replay claiming matched status with conflicting figures
		incoming1 := &domain.GatewaySettlement{
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			GrossAmountPaise:     600000,
			NetAmountPaise:       600000,
		}
		d1 := DecideSettlementUpdate(initialDiscrepancy, incoming1)
		if d1.ReconciliationStatus != domain.ReconDiscrepancy || *d1.DiscrepancyReason != "amount_changed_after_reconciliation" {
			t.Fatalf("expected discrepancy sticky on 1st replay: %+v", d1)
		}
		if d1.GrossAmountPaise != 500000 {
			t.Fatalf("amounts overwritten during discrepancy: %d", d1.GrossAmountPaise)
		}

		// 2nd identical delivery
		d2 := DecideSettlementUpdate(&d1, incoming1)
		if d2.ReconciliationStatus != domain.ReconDiscrepancy || *d2.DiscrepancyReason != "amount_changed_after_reconciliation" {
			t.Fatalf("expected discrepancy sticky on 2nd replay: %+v", d2)
		}
		if d2.GrossAmountPaise != 500000 {
			t.Fatalf("amounts overwritten on 2nd replay: %d", d2.GrossAmountPaise)
		}

		// 3rd delivery claiming different status
		incoming3 := &domain.GatewaySettlement{
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconUnmatched,
			GrossAmountPaise:     700000,
		}
		d3 := DecideSettlementUpdate(&d2, incoming3)
		if d3.ReconciliationStatus != domain.ReconDiscrepancy {
			t.Fatalf("expected discrepancy sticky on 3rd replay: %+v", d3)
		}
	})

	t.Run("while PENDING amounts can update, once terminal amounts are frozen", func(t *testing.T) {
		// 1. Pending row allows provisional-to-final amount updates
		pendingRow := &domain.GatewaySettlement{
			SettlementStatus: "PENDING",
			GrossAmountPaise: 100000,
			NetAmountPaise:   99000,
		}
		pendingUpdate := &domain.GatewaySettlement{
			SettlementStatus: "PENDING",
			GrossAmountPaise: 105000,
			NetAmountPaise:   103000,
		}
		decidedPending := DecideSettlementUpdate(pendingRow, pendingUpdate)
		if decidedPending.GrossAmountPaise != 105000 || decidedPending.NetAmountPaise != 103000 {
			t.Fatalf("expected pending amount update to succeed, got gross=%d, net=%d",
				decidedPending.GrossAmountPaise, decidedPending.NetAmountPaise)
		}

		// 2. Terminal SUCCESS row freezes amounts
		succRow := &domain.GatewaySettlement{
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			GrossAmountPaise:     105000,
			NetAmountPaise:       103000,
		}
		conflictingReplay := &domain.GatewaySettlement{
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			GrossAmountPaise:     120000,
			NetAmountPaise:       118000,
		}
		decidedSucc := DecideSettlementUpdate(succRow, conflictingReplay)
		if decidedSucc.ReconciliationStatus != domain.ReconDiscrepancy {
			t.Fatalf("expected differing replay on SUCCESS to flip to discrepancy, got %s", decidedSucc.ReconciliationStatus)
		}
		if *decidedSucc.DiscrepancyReason != "amount_changed_after_reconciliation" {
			t.Fatalf("expected reason amount_changed_after_reconciliation, got %v", decidedSucc.DiscrepancyReason)
		}
		if decidedSucc.GrossAmountPaise != 105000 || decidedSucc.NetAmountPaise != 103000 {
			t.Fatalf("expected SUCCESS amounts preserved, got gross=%d, net=%d",
				decidedSucc.GrossAmountPaise, decidedSucc.NetAmountPaise)
		}
	})

	t.Run("idempotency property f(f(x)) = f(x)", func(t *testing.T) {
		states := []*domain.GatewaySettlement{
			{
				SettlementStatus:     "SUCCESS",
				ReconciliationStatus: domain.ReconMatched,
				GrossAmountPaise:     200000,
				NetAmountPaise:       198000,
				UTR:                  "UTR123",
				SettledOn:            &now,
				RawPayload:           json.RawMessage(`{"first": true}`),
			},
			{
				SettlementStatus:     "PENDING",
				ReconciliationStatus: domain.ReconUnmatched,
				GrossAmountPaise:     200000,
				NetAmountPaise:       198000,
			},
			{
				SettlementStatus:     "REVERSED",
				ReconciliationStatus: domain.ReconDiscrepancy,
				DiscrepancyReason:    strPtr("post_reconciliation_reversal"),
				GrossAmountPaise:     200000,
				NetAmountPaise:       0,
				AdjustmentPaise:      -200000,
			},
			{
				SettlementStatus:     "SUCCESS",
				ReconciliationStatus: domain.ReconManuallyReconciled,
				DiscrepancyReason:    nil,
				ResolutionNotes:      strPtr("verified with statement"),
				ResolvedBy:           &uuid.Nil,
				ResolvedAt:           &now,
				GrossAmountPaise:     195000,
				NetAmountPaise:       193000,
			},
		}

		incomings := []*domain.GatewaySettlement{
			// Identical replay
			{
				SettlementStatus:     "SUCCESS",
				ReconciliationStatus: domain.ReconMatched,
				GrossAmountPaise:     200000,
				NetAmountPaise:       198000,
				UTR:                  "UTR123",
			},
			// Conflicting amounts replay
			{
				SettlementStatus:     "SUCCESS",
				ReconciliationStatus: domain.ReconMatched,
				GrossAmountPaise:     250000,
				NetAmountPaise:       245000,
			},
			// Legitimate reversal
			{
				SettlementStatus:     "REVERSED",
				ReconciliationStatus: domain.ReconMatched,
				GrossAmountPaise:     200000,
				NetAmountPaise:       0,
				AdjustmentPaise:      -200000,
			},
		}

		for _, existing := range states {
			for _, incoming := range incomings {
				f1 := DecideSettlementUpdate(existing, incoming)
				f2 := DecideSettlementUpdate(&f1, incoming)

				if f1.SettlementStatus != f2.SettlementStatus {
					t.Fatalf("idempotency broken on status: %s vs %s", f1.SettlementStatus, f2.SettlementStatus)
				}
				if f1.ReconciliationStatus != f2.ReconciliationStatus {
					t.Fatalf("idempotency broken on recon status: %s vs %s", f1.ReconciliationStatus, f2.ReconciliationStatus)
				}
				if (f1.DiscrepancyReason == nil) != (f2.DiscrepancyReason == nil) ||
					(f1.DiscrepancyReason != nil && *f1.DiscrepancyReason != *f2.DiscrepancyReason) {
					t.Fatalf("idempotency broken on discrepancy reason: %v vs %v", f1.DiscrepancyReason, f2.DiscrepancyReason)
				}
				if f1.GrossAmountPaise != f2.GrossAmountPaise || f1.NetAmountPaise != f2.NetAmountPaise {
					t.Fatalf("idempotency broken on amounts: gross %d vs %d", f1.GrossAmountPaise, f2.GrossAmountPaise)
				}
			}
		}
	})
}
