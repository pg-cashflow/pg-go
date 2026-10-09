package postgres

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLivePostgresSettlementAntiRegressionAndReplay(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Ensure migration 026 is applied
	m26Path := filepath.Join("..", "..", "migrations", "026_gateway_settlements_and_recon.sql")
	m26SQL, err := os.ReadFile(m26Path)
	if err != nil {
		t.Fatalf("failed reading migration 026: %v", err)
	}
	if _, err := pool.Exec(ctx, string(m26SQL)); err != nil {
		t.Fatalf("failed applying migration 026: %v", err)
	}

	repo := NewSettlementRepo(pool)

	t.Run("manual resolution is preserved against webhook replay", func(t *testing.T) {
		cfSettlementID := "CF_REPLAY_" + uuid.New().String()[:8]
		orderID := "order_" + uuid.New().String()[:8]
		cfPaymentID := "pay_" + uuid.New().String()[:8]

		reason := "arithmetic_imbalance"
		stlm := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     500000,
			ServiceChargePaise:   1000,
			ServiceTaxPaise:      180,
			AdjustmentPaise:      0,
			NetAmountPaise:       498820,
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconDiscrepancy,
			DiscrepancyReason:    &reason,
			RawPayload:           json.RawMessage(`{}`),
		}

		if err := repo.UpsertSettlement(ctx, stlm); err != nil {
			t.Fatalf("initial upsert failed: %v", err)
		}

		// Operator manually reconciles the discrepancy and corrects amounts
		notes := "verified with bank statement manually"
		now := time.Now().UTC()
		correctedGross := int64(495000)
		correctedNet := int64(493820)
		_, err := pool.Exec(ctx, `
			UPDATE gateway_settlements
			SET reconciliation_status = 'manually_reconciled',
			    discrepancy_reason = NULL,
			    resolution_notes = $1,
			    resolved_at = $2,
			    gross_amount_paise = $3,
			    net_amount_paise = $4
			WHERE cf_settlement_id = $5 AND order_id = $6`,
			notes, now, correctedGross, correctedNet, cfSettlementID, orderID,
		)
		if err != nil {
			t.Fatalf("operator manual update failed: %v", err)
		}

		// Webhook replay delivers the original discrepancy status again with old amounts
		replayedReason := "arithmetic_imbalance"
		replayStlm := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     500000,
			ServiceChargePaise:   1000,
			ServiceTaxPaise:      180,
			AdjustmentPaise:      0,
			NetAmountPaise:       498820,
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconDiscrepancy,
			DiscrepancyReason:    &replayedReason,
			RawPayload:           json.RawMessage(`{"replayed": true}`),
		}

		if err := repo.UpsertSettlement(ctx, replayStlm); err != nil {
			t.Fatalf("replay upsert failed: %v", err)
		}

		fetched, err := repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch settlement failed: %v", err)
		}

		if fetched.ReconciliationStatus != domain.ReconManuallyReconciled {
			t.Errorf("expected reconciliation_status 'manually_reconciled' to be preserved, got %q", fetched.ReconciliationStatus)
		}
		if fetched.DiscrepancyReason != nil {
			t.Errorf("expected discrepancy_reason to remain nil, got %v", *fetched.DiscrepancyReason)
		}
		if fetched.ResolutionNotes == nil || *fetched.ResolutionNotes != notes {
			t.Errorf("expected resolution_notes %q, got %v", notes, fetched.ResolutionNotes)
		}
		if fetched.GrossAmountPaise != correctedGross {
			t.Errorf("expected gross_amount_paise to remain operator-corrected %d, got %d", correctedGross, fetched.GrossAmountPaise)
		}
		if fetched.NetAmountPaise != correctedNet {
			t.Errorf("expected net_amount_paise to remain operator-corrected %d, got %d", correctedNet, fetched.NetAmountPaise)
		}

		// Subsequent legitimate REVERSED webhook arrives for this manually reconciled settlement:
		// Must re-open discrepancy, set reason 'post_reconciliation_reversal', and record reversal figures.
		revGross := int64(495000)
		revNet := int64(0)
		revAdjustment := int64(-495000)
		revStlm := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     revGross,
			ServiceChargePaise:   1000,
			ServiceTaxPaise:      180,
			AdjustmentPaise:      revAdjustment,
			NetAmountPaise:       revNet,
			SettlementStatus:     "REVERSED",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{"chargeback": true}`),
		}
		if err := repo.UpsertSettlement(ctx, revStlm); err != nil {
			t.Fatalf("upsert post-reconciliation reversal failed: %v", err)
		}

		revFetched, err := repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch post-reconciliation reversal failed: %v", err)
		}
		if revFetched.SettlementStatus != "REVERSED" {
			t.Errorf("expected settlement_status REVERSED, got %q", revFetched.SettlementStatus)
		}
		if revFetched.ReconciliationStatus != domain.ReconDiscrepancy {
			t.Errorf("expected reconciliation_status 'discrepancy' on post-recon reversal, got %q", revFetched.ReconciliationStatus)
		}
		if revFetched.DiscrepancyReason == nil || *revFetched.DiscrepancyReason != "post_reconciliation_reversal" {
			t.Errorf("expected discrepancy_reason 'post_reconciliation_reversal', got %v", revFetched.DiscrepancyReason)
		}
		if revFetched.AdjustmentPaise != revAdjustment {
			t.Errorf("expected adjustment_paise %d, got %d", revAdjustment, revFetched.AdjustmentPaise)
		}
		if revFetched.NetAmountPaise != revNet {
			t.Errorf("expected net_amount_paise %d, got %d", revNet, revFetched.NetAmountPaise)
		}
		if revFetched.ResolutionNotes == nil || !strings.Contains(*revFetched.ResolutionNotes, "Reversal received post-reconciliation") {
			t.Errorf("expected alert in resolution_notes, got %v", revFetched.ResolutionNotes)
		}
	})

	t.Run("settlement_status state ranking and out-of-order replay protection", func(t *testing.T) {
		cfSettlementID := "CF_STATUS_" + uuid.New().String()[:8]
		orderID := "order_" + uuid.New().String()[:8]
		cfPaymentID := "pay_" + uuid.New().String()[:8]

		// 1. Initial status: SUCCESS
		stlm := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     100000,
			NetAmountPaise:       100000,
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, stlm); err != nil {
			t.Fatalf("upsert initial SUCCESS failed: %v", err)
		}

		// 2. Delayed PENDING webhook must NOT regress SUCCESS
		pendingStlm := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     100000,
			NetAmountPaise:       100000,
			SettlementStatus:     "PENDING",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, pendingStlm); err != nil {
			t.Fatalf("upsert delayed PENDING failed: %v", err)
		}

		fetched, err := repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch settlement failed: %v", err)
		}
		if fetched.SettlementStatus != "SUCCESS" {
			t.Fatalf("expected settlement_status to remain SUCCESS against PENDING replay, got %q", fetched.SettlementStatus)
		}

		// 3. Delayed FAILED webhook must NOT regress SUCCESS
		failedStlm := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     100000,
			NetAmountPaise:       100000,
			SettlementStatus:     "FAILED",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, failedStlm); err != nil {
			t.Fatalf("upsert delayed FAILED failed: %v", err)
		}

		fetched, err = repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch settlement failed: %v", err)
		}
		if fetched.SettlementStatus != "SUCCESS" {
			t.Fatalf("expected settlement_status to remain SUCCESS against FAILED replay, got %q", fetched.SettlementStatus)
		}

		// 4. Legitimate REVERSED status must win over SUCCESS
		reversedStlm := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     100000,
			NetAmountPaise:       100000,
			SettlementStatus:     "REVERSED",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, reversedStlm); err != nil {
			t.Fatalf("upsert REVERSED failed: %v", err)
		}

		fetched, err = repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch settlement failed: %v", err)
		}
		if fetched.SettlementStatus != "REVERSED" {
			t.Fatalf("expected settlement_status to transition to REVERSED, got %q", fetched.SettlementStatus)
		}

		// 5. Out-of-order SUCCESS replay must NOT overwrite REVERSED
		if err := repo.UpsertSettlement(ctx, stlm); err != nil {
			t.Fatalf("upsert replayed SUCCESS failed: %v", err)
		}
		fetched, err = repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch settlement failed: %v", err)
		}
		if fetched.SettlementStatus != "REVERSED" {
			t.Fatalf("expected settlement_status to remain terminal REVERSED against SUCCESS replay, got %q", fetched.SettlementStatus)
		}

		// 6. Out-of-order FAILED replay must NOT overwrite REVERSED
		if err := repo.UpsertSettlement(ctx, failedStlm); err != nil {
			t.Fatalf("upsert replayed FAILED failed: %v", err)
		}
		fetched, err = repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch settlement failed: %v", err)
		}
		if fetched.SettlementStatus != "REVERSED" {
			t.Fatalf("expected settlement_status to remain terminal REVERSED against FAILED replay, got %q", fetched.SettlementStatus)
		}

		// 7. Out-of-order PENDING replay must NOT overwrite REVERSED
		if err := repo.UpsertSettlement(ctx, pendingStlm); err != nil {
			t.Fatalf("upsert replayed PENDING failed: %v", err)
		}
		fetched, err = repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch settlement failed: %v", err)
		}
		if fetched.SettlementStatus != "REVERSED" {
			t.Fatalf("expected settlement_status to remain terminal REVERSED against PENDING replay, got %q", fetched.SettlementStatus)
		}

		// 8. Test FAILED cannot regress to PENDING, but can advance to SUCCESS
		cfFailedID := "CF_FAILED_" + uuid.New().String()[:8]
		orderFailedID := "order_f_" + uuid.New().String()[:8]
		cfPayFailedID := "pay_f_" + uuid.New().String()[:8]

		initFailed := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfFailedID,
			OrderID:              &orderFailedID,
			CFPaymentID:          &cfPayFailedID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     50000,
			NetAmountPaise:       50000,
			SettlementStatus:     "FAILED",
			ReconciliationStatus: domain.ReconDiscrepancy,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, initFailed); err != nil {
			t.Fatalf("upsert initFailed failed: %v", err)
		}

		// Delayed PENDING arrives for failed settlement: must NOT regress to PENDING
		pendingForFailed := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfFailedID,
			OrderID:              &orderFailedID,
			CFPaymentID:          &cfPayFailedID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     50000,
			NetAmountPaise:       50000,
			SettlementStatus:     "PENDING",
			ReconciliationStatus: domain.ReconDiscrepancy,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, pendingForFailed); err != nil {
			t.Fatalf("upsert pendingForFailed failed: %v", err)
		}
		fetchedFailed, err := repo.GetSettlementByCFID(ctx, cfFailedID, orderFailedID, cfPayFailedID)
		if err != nil {
			t.Fatalf("fetch failed settlement failed: %v", err)
		}
		if fetchedFailed.SettlementStatus != "FAILED" {
			t.Fatalf("expected settlement_status to remain FAILED against PENDING replay, got %q", fetchedFailed.SettlementStatus)
		}

		// Late recovery / manual retry succeeds: FAILED -> SUCCESS is permitted
		recoveredSuccess := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfFailedID,
			OrderID:              &orderFailedID,
			CFPaymentID:          &cfPayFailedID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     50000,
			NetAmountPaise:       50000,
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, recoveredSuccess); err != nil {
			t.Fatalf("upsert recoveredSuccess failed: %v", err)
		}
		fetchedRecovered, err := repo.GetSettlementByCFID(ctx, cfFailedID, orderFailedID, cfPayFailedID)
		if err != nil {
			t.Fatalf("fetch recovered settlement failed: %v", err)
		}
		if fetchedRecovered.SettlementStatus != "SUCCESS" {
			t.Fatalf("expected settlement_status to transition from FAILED to SUCCESS, got %q", fetchedRecovered.SettlementStatus)
		}
	})

	t.Run("duplicate REVERSED replay preserves discrepancy and does not overwrite status or amounts", func(t *testing.T) {
		cfSettlementID := "CF_DUP_REV_" + uuid.New().String()[:8]
		orderID := "order_dr_" + uuid.New().String()[:8]
		cfPaymentID := "pay_dr_" + uuid.New().String()[:8]

		// 1. Initial settled state
		initialStlm := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     200000,
			NetAmountPaise:       200000,
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, initialStlm); err != nil {
			t.Fatalf("upsert initial settled state failed: %v", err)
		}

		// 2. First REVERSED delivery arrives
		rev1 := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     200000,
			NetAmountPaise:       0,
			AdjustmentPaise:      -200000,
			SettlementStatus:     "REVERSED",
			ReconciliationStatus: domain.ReconMatched, // incoming payload might default to matched
			RawPayload:           json.RawMessage(`{"reversal": 1}`),
		}
		if err := repo.UpsertSettlement(ctx, rev1); err != nil {
			t.Fatalf("first REVERSED delivery failed: %v", err)
		}

		fetched1, err := repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch after first REVERSED failed: %v", err)
		}
		if fetched1.SettlementStatus != "REVERSED" {
			t.Fatalf("expected settlement_status REVERSED, got %q", fetched1.SettlementStatus)
		}
		if fetched1.ReconciliationStatus != domain.ReconDiscrepancy {
			t.Fatalf("expected reconciliation_status 'discrepancy', got %q", fetched1.ReconciliationStatus)
		}
		if fetched1.DiscrepancyReason == nil || *fetched1.DiscrepancyReason != "post_reconciliation_reversal" {
			t.Fatalf("expected discrepancy_reason 'post_reconciliation_reversal', got %v", fetched1.DiscrepancyReason)
		}

		// 3. Second IDENTICAL REVERSED delivery arrives (duplicate webhook / retry)
		rev2 := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     200000,
			NetAmountPaise:       0,
			AdjustmentPaise:      -200000,
			SettlementStatus:     "REVERSED",
			ReconciliationStatus: domain.ReconMatched, // replayed payload still computes matched
			RawPayload:           json.RawMessage(`{"reversal": 1}`),
		}
		if err := repo.UpsertSettlement(ctx, rev2); err != nil {
			t.Fatalf("second identical REVERSED delivery failed: %v", err)
		}

		fetched2, err := repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch after second REVERSED failed: %v", err)
		}
		// Crucial verification: duplicate delivery MUST NOT overwrite 'discrepancy' or discrepancy_reason
		if fetched2.SettlementStatus != "REVERSED" {
			t.Fatalf("expected settlement_status to remain REVERSED, got %q", fetched2.SettlementStatus)
		}
		if fetched2.ReconciliationStatus != domain.ReconDiscrepancy {
			t.Fatalf("expected duplicate REVERSED to preserve 'discrepancy', got %q", fetched2.ReconciliationStatus)
		}
		if fetched2.DiscrepancyReason == nil || *fetched2.DiscrepancyReason != "post_reconciliation_reversal" {
			t.Fatalf("expected duplicate REVERSED to preserve 'post_reconciliation_reversal', got %v", fetched2.DiscrepancyReason)
		}
		if fetched2.GrossAmountPaise != 200000 || fetched2.NetAmountPaise != 0 || fetched2.AdjustmentPaise != -200000 {
			t.Fatalf("expected amounts preserved on identical replay, got gross=%d, net=%d, adj=%d",
				fetched2.GrossAmountPaise, fetched2.NetAmountPaise, fetched2.AdjustmentPaise)
		}
	})

	t.Run("differing amount replay on reconciled row flips to discrepancy and preserves amounts", func(t *testing.T) {
		cfSettlementID := "CF_DIFF_RECON_" + uuid.New().String()[:8]
		orderID := "order_df_" + uuid.New().String()[:8]
		cfPaymentID := "pay_df_" + uuid.New().String()[:8]

		// 1. Initial matched row
		stlm := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     300000,
			NetAmountPaise:       300000,
			ServiceChargePaise:   1000,
			ServiceTaxPaise:      180,
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, stlm); err != nil {
			t.Fatalf("upsert initial matched row failed: %v", err)
		}

		// 2. Conflicting replay arrives claiming different gross/net amounts
		conflictingReplay := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     350000, // differing gross amount!
			NetAmountPaise:       350000, // differing net amount!
			ServiceChargePaise:   1000,
			ServiceTaxPaise:      180,
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{"tampered": true}`),
		}
		if err := repo.UpsertSettlement(ctx, conflictingReplay); err != nil {
			t.Fatalf("upsert conflicting replay failed: %v", err)
		}

		fetched, err := repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch after conflicting replay failed: %v", err)
		}
		// Must flip to discrepancy with amount_changed_after_reconciliation
		if fetched.ReconciliationStatus != domain.ReconDiscrepancy {
			t.Fatalf("expected reconciliation_status 'discrepancy', got %q", fetched.ReconciliationStatus)
		}
		if fetched.DiscrepancyReason == nil || *fetched.DiscrepancyReason != "amount_changed_after_reconciliation" {
			t.Fatalf("expected discrepancy_reason 'amount_changed_after_reconciliation', got %v", fetched.DiscrepancyReason)
		}
		// Existing amounts must NOT be silently changed
		if fetched.GrossAmountPaise != 300000 {
			t.Fatalf("expected gross_amount_paise preserved at 300000, got %d", fetched.GrossAmountPaise)
		}
		if fetched.NetAmountPaise != 300000 {
			t.Fatalf("expected net_amount_paise preserved at 300000, got %d", fetched.NetAmountPaise)
		}
		if fetched.ResolutionNotes == nil || !strings.Contains(*fetched.ResolutionNotes, "Conflicting amounts received post-reconciliation") {
			t.Fatalf("expected resolution notes alert, got %v", fetched.ResolutionNotes)
		}

		// 3. Second IDENTICAL conflicting delivery (proves discrepancy is sticky and amounts remain frozen)
		if err := repo.UpsertSettlement(ctx, conflictingReplay); err != nil {
			t.Fatalf("upsert 2nd conflicting replay failed: %v", err)
		}
		fetched2, err := repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch after 2nd conflicting replay failed: %v", err)
		}
		if fetched2.ReconciliationStatus != domain.ReconDiscrepancy {
			t.Fatalf("expected reconciliation_status 'discrepancy' preserved on 2nd replay, got %q", fetched2.ReconciliationStatus)
		}
		if fetched2.DiscrepancyReason == nil || *fetched2.DiscrepancyReason != "amount_changed_after_reconciliation" {
			t.Fatalf("expected discrepancy_reason 'amount_changed_after_reconciliation' preserved on 2nd replay, got %v", fetched2.DiscrepancyReason)
		}
		if fetched2.GrossAmountPaise != 300000 || fetched2.NetAmountPaise != 300000 {
			t.Fatalf("expected amounts preserved on 2nd conflicting delivery, got gross=%d, net=%d", fetched2.GrossAmountPaise, fetched2.NetAmountPaise)
		}

		// 4. Third conflicting delivery claiming even higher amounts
		conflictingReplay3 := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     400000,
			NetAmountPaise:       400000,
			ServiceChargePaise:   1000,
			ServiceTaxPaise:      180,
			SettlementStatus:     "SUCCESS",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{"tampered": 3}`),
		}
		if err := repo.UpsertSettlement(ctx, conflictingReplay3); err != nil {
			t.Fatalf("upsert 3rd conflicting replay failed: %v", err)
		}
		fetched3, err := repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch after 3rd conflicting replay failed: %v", err)
		}
		if fetched3.ReconciliationStatus != domain.ReconDiscrepancy {
			t.Fatalf("expected reconciliation_status 'discrepancy' preserved on 3rd replay, got %q", fetched3.ReconciliationStatus)
		}
		if fetched3.DiscrepancyReason == nil || *fetched3.DiscrepancyReason != "amount_changed_after_reconciliation" {
			t.Fatalf("expected discrepancy_reason preserved on 3rd replay, got %v", fetched3.DiscrepancyReason)
		}
		if fetched3.GrossAmountPaise != 300000 || fetched3.NetAmountPaise != 300000 {
			t.Fatalf("expected amounts preserved on 3rd conflicting delivery, got gross=%d, net=%d", fetched3.GrossAmountPaise, fetched3.NetAmountPaise)
		}
	})

	t.Run("differing amount replay on REVERSED row flips reason to amount_changed_after_reconciliation and preserves amounts", func(t *testing.T) {
		cfSettlementID := "CF_DIFF_REV_" + uuid.New().String()[:8]
		orderID := "order_dfr_" + uuid.New().String()[:8]
		cfPaymentID := "pay_dfr_" + uuid.New().String()[:8]

		// 1. Initial reversed row
		revInitial := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     150000,
			NetAmountPaise:       0,
			AdjustmentPaise:      -150000,
			SettlementStatus:     "REVERSED",
			ReconciliationStatus: domain.ReconDiscrepancy,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, revInitial); err != nil {
			t.Fatalf("upsert initial REVERSED row failed: %v", err)
		}

		// 2. Differing replay arrives with different adjustment figure
		revDiffering := &domain.GatewaySettlement{
			ID:                   uuid.New(),
			CFSettlementID:       cfSettlementID,
			OrderID:              &orderID,
			CFPaymentID:          &cfPaymentID,
			IngestionSource:      domain.IngestionWebhook,
			GrossAmountPaise:     150000,
			NetAmountPaise:       0,
			AdjustmentPaise:      -100000, // different adjustment!
			SettlementStatus:     "REVERSED",
			ReconciliationStatus: domain.ReconMatched,
			RawPayload:           json.RawMessage(`{}`),
		}
		if err := repo.UpsertSettlement(ctx, revDiffering); err != nil {
			t.Fatalf("upsert differing REVERSED replay failed: %v", err)
		}

		fetched, err := repo.GetSettlementByCFID(ctx, cfSettlementID, orderID, cfPaymentID)
		if err != nil {
			t.Fatalf("fetch after differing REVERSED replay failed: %v", err)
		}
		if fetched.ReconciliationStatus != domain.ReconDiscrepancy {
			t.Fatalf("expected reconciliation_status 'discrepancy', got %q", fetched.ReconciliationStatus)
		}
		if fetched.DiscrepancyReason == nil || *fetched.DiscrepancyReason != "amount_changed_after_reconciliation" {
			t.Fatalf("expected discrepancy_reason 'amount_changed_after_reconciliation', got %v", fetched.DiscrepancyReason)
		}
		if fetched.AdjustmentPaise != -150000 {
			t.Fatalf("expected adjustment_paise preserved at -150000, got %d", fetched.AdjustmentPaise)
		}
	})
}
