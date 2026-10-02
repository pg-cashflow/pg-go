package postgres

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLivePostgresSettlementAntiRegressionAndReplay(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

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

		// Operator manually reconciles the discrepancy
		notes := "verified with bank statement manually"
		now := time.Now().UTC()
		_, err := pool.Exec(ctx, `
			UPDATE gateway_settlements
			SET reconciliation_status = 'manually_reconciled',
			    discrepancy_reason = NULL,
			    resolution_notes = $1,
			    resolved_at = $2
			WHERE cf_settlement_id = $3 AND order_id = $4`,
			notes, now, cfSettlementID, orderID,
		)
		if err != nil {
			t.Fatalf("operator manual update failed: %v", err)
		}

		// Webhook replay delivers the original discrepancy status again
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
			RawPayload:           json.RawMessage(`{}`),
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

		// 3. Legitimate REVERSED status must win over SUCCESS
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

		// 4. Out-of-order SUCCESS replay must NOT overwrite REVERSED
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
	})
}
