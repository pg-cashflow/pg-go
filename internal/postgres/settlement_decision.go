package postgres

import (
	"strings"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// AmountsDiffer reports whether any of the financial amounts between two settlement records differ.
func AmountsDiffer(a, b *domain.GatewaySettlement) bool {
	if a == nil || b == nil {
		return false
	}
	return a.GrossAmountPaise != b.GrossAmountPaise ||
		a.NetAmountPaise != b.NetAmountPaise ||
		a.ServiceChargePaise != b.ServiceChargePaise ||
		a.ServiceTaxPaise != b.ServiceTaxPaise ||
		a.AdjustmentPaise != b.AdjustmentPaise
}

// DecideSettlementUpdate evaluates business and idempotency rules when an incoming
// gateway settlement delivery arrives for an existing database record.
//
// Rules enforced:
// 1. Terminal states stay terminal:
//    - Existing "REVERSED" can never be overwritten by any state.
//    - Existing "SUCCESS" cannot regress to "PENDING" or "FAILED".
//    - Existing "FAILED" cannot regress to "PENDING".
// 2. Legitimate initial reversal:
//    - Incoming "REVERSED" on a non-REVERSED row transitions to "REVERSED",
//      re-opens discrepancy with reason "post_reconciliation_reversal", updates reversal figures,
//      and resets resolution fields.
// 3. Discrepancy is sticky:
//    - Once in "discrepancy", only manual operator action can resolve it. Incoming webhooks
//      (whether identical, replayed, or conflicting) CANNOT overwrite reconciliation_status,
//      discrepancy_reason, or stored amounts.
// 4. Amount freeze:
//    - Amounts are frozen once terminal ("SUCCESS" or "REVERSED") or manually reconciled ("manually_reconciled").
//    - While "PENDING", amounts may be updated (allowing provisional-to-final changes).
// 5. Tamper / conflicting replay detection:
//    - If incoming amounts differ on a terminal row ("SUCCESS" / "REVERSED") or reconciled row ("matched"):
//      flips to "discrepancy" with reason "amount_changed_after_reconciliation", preserves the
//      stored figures, appends an alert to resolution_notes, and clears resolved_by / resolved_at.
// 6. Manual resolution preservation:
//    - If row is "manually_reconciled", normal webhook replays cannot overwrite operator figures
//      or clear the manual reconciliation status.
// 7. Banking metadata:
//    - Official banking metadata (UTR, settled_on, transfer_time) is updated when incoming provides values.
func DecideSettlementUpdate(existing, incoming *domain.GatewaySettlement) domain.GatewaySettlement {
	updated := *existing

	// 1. Banking metadata updates (safe tie-outs for bank references)
	if incoming.UTR != "" {
		updated.UTR = incoming.UTR
	}
	if incoming.SettledOn != nil {
		updated.SettledOn = incoming.SettledOn
	}
	if incoming.SettlementInitiatedOn != nil {
		updated.SettlementInitiatedOn = incoming.SettlementInitiatedOn
	}
	if incoming.TransferTime != nil {
		updated.TransferTime = incoming.TransferTime
	}
	if incoming.PropertyID != nil && updated.PropertyID == nil {
		updated.PropertyID = incoming.PropertyID
	}
	if incoming.PaymentIntentID != nil && updated.PaymentIntentID == nil {
		updated.PaymentIntentID = incoming.PaymentIntentID
	}
	if incoming.PaymentID != nil && updated.PaymentID == nil {
		updated.PaymentID = incoming.PaymentID
	}
	if incoming.JournalEntryID != nil && updated.JournalEntryID == nil {
		updated.JournalEntryID = incoming.JournalEntryID
	}

	isInitialReversal := incoming.SettlementStatus == "REVERSED" && existing.SettlementStatus != "REVERSED"
	diffAmounts := AmountsDiffer(existing, incoming)

	// 2. Settlement status state transition
	if existing.SettlementStatus == "REVERSED" {
		updated.SettlementStatus = "REVERSED"
	} else if incoming.SettlementStatus == "REVERSED" {
		updated.SettlementStatus = "REVERSED"
	} else if existing.SettlementStatus == "SUCCESS" && (incoming.SettlementStatus == "PENDING" || incoming.SettlementStatus == "FAILED") {
		updated.SettlementStatus = "SUCCESS"
	} else if existing.SettlementStatus == "FAILED" && incoming.SettlementStatus == "PENDING" {
		updated.SettlementStatus = "FAILED"
	} else if incoming.SettlementStatus != "" {
		updated.SettlementStatus = incoming.SettlementStatus
	}

	// 3. Reconciliation status, discrepancy reason, and amounts
	switch {
	case isInitialReversal:
		// Legitimate first transition into REVERSED
		updated.ReconciliationStatus = domain.ReconDiscrepancy
		reason := "post_reconciliation_reversal"
		updated.DiscrepancyReason = &reason
		updated.GrossAmountPaise = incoming.GrossAmountPaise
		updated.NetAmountPaise = incoming.NetAmountPaise
		updated.ServiceChargePaise = incoming.ServiceChargePaise
		updated.ServiceTaxPaise = incoming.ServiceTaxPaise
		updated.AdjustmentPaise = incoming.AdjustmentPaise
		updated.IngestionSource = incoming.IngestionSource
		updated.RawPayload = incoming.RawPayload
		if existing.ReconciliationStatus == domain.ReconManuallyReconciled {
			updated.ResolutionNotes = appendResolutionAlert(existing.ResolutionNotes, "SYSTEM ALERT: Reversal received post-reconciliation")
		}
		updated.ResolvedBy = nil
		updated.ResolvedAt = nil

	case existing.ReconciliationStatus == domain.ReconManuallyReconciled:
		// Operator manual reconciliation is preserved against normal incoming webhooks.
		updated.ReconciliationStatus = domain.ReconManuallyReconciled
		updated.DiscrepancyReason = existing.DiscrepancyReason
		updated.GrossAmountPaise = existing.GrossAmountPaise
		updated.NetAmountPaise = existing.NetAmountPaise
		updated.ServiceChargePaise = existing.ServiceChargePaise
		updated.ServiceTaxPaise = existing.ServiceTaxPaise
		updated.AdjustmentPaise = existing.AdjustmentPaise
		updated.ResolutionNotes = existing.ResolutionNotes
		updated.ResolvedBy = existing.ResolvedBy
		updated.ResolvedAt = existing.ResolvedAt
		updated.RawPayload = existing.RawPayload
		updated.IngestionSource = existing.IngestionSource

	case (existing.SettlementStatus == "SUCCESS" || existing.SettlementStatus == "REVERSED") && diffAmounts:
		// Conflicting amounts arrive for a terminal settlement:
		// Flip/keep discrepancy, set reason to amount_changed_after_reconciliation, and freeze stored amounts.
		updated.ReconciliationStatus = domain.ReconDiscrepancy
		reason := "amount_changed_after_reconciliation"
		updated.DiscrepancyReason = &reason
		updated.ResolutionNotes = appendResolutionAlert(existing.ResolutionNotes, "SYSTEM ALERT: Conflicting amounts received post-reconciliation")
		updated.ResolvedBy = nil
		updated.ResolvedAt = nil
		// Stored figures are NOT overwritten
		updated.GrossAmountPaise = existing.GrossAmountPaise
		updated.NetAmountPaise = existing.NetAmountPaise
		updated.ServiceChargePaise = existing.ServiceChargePaise
		updated.ServiceTaxPaise = existing.ServiceTaxPaise
		updated.AdjustmentPaise = existing.AdjustmentPaise
		updated.RawPayload = existing.RawPayload
		updated.IngestionSource = existing.IngestionSource

	case existing.ReconciliationStatus == domain.ReconDiscrepancy:
		// Sticky discrepancy: once in discrepancy, only operator action can leave it.
		// Replays (conflicting or identical) preserve existing discrepancy, reason, and stored amounts.
		updated.ReconciliationStatus = domain.ReconDiscrepancy
		updated.DiscrepancyReason = existing.DiscrepancyReason
		updated.GrossAmountPaise = existing.GrossAmountPaise
		updated.NetAmountPaise = existing.NetAmountPaise
		updated.ServiceChargePaise = existing.ServiceChargePaise
		updated.ServiceTaxPaise = existing.ServiceTaxPaise
		updated.AdjustmentPaise = existing.AdjustmentPaise
		updated.ResolutionNotes = existing.ResolutionNotes
		updated.ResolvedBy = existing.ResolvedBy
		updated.ResolvedAt = existing.ResolvedAt
		updated.RawPayload = existing.RawPayload
		updated.IngestionSource = existing.IngestionSource

	case existing.SettlementStatus == "SUCCESS" || existing.SettlementStatus == "REVERSED":
		// Terminal row with matching amounts: preserve status and figures.
		updated.GrossAmountPaise = existing.GrossAmountPaise
		updated.NetAmountPaise = existing.NetAmountPaise
		updated.ServiceChargePaise = existing.ServiceChargePaise
		updated.ServiceTaxPaise = existing.ServiceTaxPaise
		updated.AdjustmentPaise = existing.AdjustmentPaise
		if len(incoming.RawPayload) > 0 && string(incoming.RawPayload) != "{}" {
			updated.RawPayload = incoming.RawPayload
		}

	default:
		// Non-terminal (e.g. PENDING): amounts can be freely updated (provisional -> final)
		// Even if tentative reconciliation_status was 'matched', provisional-to-final amount changes
		// are legitimate and must NOT be flagged as tampering/discrepancy.
		updated.GrossAmountPaise = incoming.GrossAmountPaise
		updated.NetAmountPaise = incoming.NetAmountPaise
		updated.ServiceChargePaise = incoming.ServiceChargePaise
		updated.ServiceTaxPaise = incoming.ServiceTaxPaise
		updated.AdjustmentPaise = incoming.AdjustmentPaise
		updated.IngestionSource = incoming.IngestionSource
		if len(incoming.RawPayload) > 0 && string(incoming.RawPayload) != "{}" {
			updated.RawPayload = incoming.RawPayload
		}
		if incoming.ReconciliationStatus != "" {
			updated.ReconciliationStatus = incoming.ReconciliationStatus
		}
		if incoming.DiscrepancyReason != nil {
			updated.DiscrepancyReason = incoming.DiscrepancyReason
		}
	}

	return updated
}

func appendResolutionAlert(existing *string, alert string) *string {
	if existing == nil || *existing == "" {
		res := alert
		return &res
	}
	if strings.Contains(*existing, alert) {
		return existing
	}
	res := *existing + " | [" + alert + "]"
	return &res
}
