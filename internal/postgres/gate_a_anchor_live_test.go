package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// TestLivePostgresGateA_AnchorBatches verifies C-08, C-09, C-10, C-05, and C-11 on live PostgreSQL.
func TestLivePostgresGateA_AnchorBatches(t *testing.T) {
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)
	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	// Seed test fixtures
	propID := uuid.New()
	invCode := "GA" + uuid.New().String()[:6]
	tenantID := uuid.New()
	phone := "+91" + strconv.FormatInt(time.Now().UnixNano()%10000000000, 10)

	err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
			VALUES ($1, 'Gate A Live Prop', '123 Anchor Way', '+919999900030', 'anchor@upi', 'Owner Anchor', 'anchor@example.com', $2)
			ON CONFLICT (id) DO NOTHING;
		`, propID, invCode)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status, credit_balance_paise)
			VALUES ($1, $2, 'Gate A Tenant', $3, 20000, 1, 'active', 0)
			ON CONFLICT (id) DO NOTHING;
		`, tenantID, propID, phone)
		return err
	})
	if err != nil {
		t.Fatalf("seeding properties and tenants failed: %v", err)
	}

	t.Cleanup(func() {
		_ = WithinTx(context.Background(), pool, func(tx pgx.Tx) error {
			_, _ = tx.Exec(context.Background(), "SET LOCAL app.ledger_maintenance = 'on'")
			_, _ = tx.Exec(context.Background(), "DELETE FROM ledger_outbox_events WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM deposit_settlements WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM financial_corrections WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM payments WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM dues WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM tenants WHERE property_id = $1", propID)
			_, _ = tx.Exec(context.Background(), "DELETE FROM properties WHERE id = $1", propID)
			return nil
		})
	})

	depositSettlementRepo := NewDepositSettlementRepo(pool)
	paymentRepo := NewPaymentRepo(pool)

	// -------------------------------------------------------------
	// C-08: Deposit Settlement State Machine & Outbox Invariants
	// -------------------------------------------------------------
	t.Run("C-08: Deposit Settlement Atomic State Machine & Outbox", func(t *testing.T) {
		depositDueID := uuid.New()
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'"); err != nil {
				return err
			}
			dueCode := "DEP" + uuid.New().String()[:4]
			_, err = tx.Exec(ctx, `
				INSERT INTO dues (id, property_id, tenant_id, kind, original_amount, amount, status, due_code, due_date, period_start, period_end, created_at)
				VALUES ($1, $2, $3, 'deposit', 50000, 0, 'paid', $4, NOW(), NOW(), NOW() + INTERVAL '30 days', NOW())`,
				depositDueID, propID, tenantID, dueCode,
			)
			return err
		})
		if err != nil {
			t.Fatalf("failed inserting deposit due: %v", err)
		}

		// 1. Successful atomic settlement
		idemKey := "idem-dep-" + uuid.New().String()
		params := SettleDepositParams{
			PropertyID:      propID,
			TenantID:        tenantID,
			DepositDueID:    depositDueID,
			RefundedPaise:   35000,
			DeductionsPaise: 15000,
			IdempotencyKey:  idemKey,
			Reason:          "departure move-out settlement",
			SettledAt:       time.Now().UTC(),
		}
		settlement, err := depositSettlementRepo.SettleDepositAtomic(ctx, params)
		if err != nil {
			t.Fatalf("SettleDepositAtomic failed: %v", err)
		}
		if settlement.Status != domain.DepositSettlementSettled {
			t.Errorf("expected settlement status 'settled', got '%s'", settlement.Status)
		}

		// Verify deposit_settlement_mirror outbox event was atomically enqueued
		var outboxID int64
		var outboxPayload []byte
		err = pool.QueryRow(ctx, `
			SELECT id, payload FROM ledger_outbox_events
			WHERE property_id = $1 AND event_type = $2 AND source_id = $3`,
			propID, domain.LedgerOutboxDepositSettlement, settlement.ID,
		).Scan(&outboxID, &outboxPayload)
		if err != nil {
			t.Fatalf("expected outbox event for deposit settlement, got: %v", err)
		}
		var p domain.DepositSettlementMirrorPayload
		if err := json.Unmarshal(outboxPayload, &p); err != nil {
			t.Fatalf("failed unmarshaling outbox payload: %v", err)
		}
		if p.RefundedPaise != 35000 || p.DeductionsPaise != 15000 {
			t.Errorf("unexpected outbox payload amounts: %+v", p)
		}

		// 2. Idempotent replay: calling again with same key returns identical settlement
		replayed, err := depositSettlementRepo.SettleDepositAtomic(ctx, params)
		if err != nil {
			t.Fatalf("replayed SettleDepositAtomic failed: %v", err)
		}
		if replayed.ID != settlement.ID {
			t.Errorf("expected replayed settlement ID %s, got %s", settlement.ID, replayed.ID)
		}

		// 3. Conflict: attempting to settle the already settled deposit due with a NEW key
		paramsConflict := params
		paramsConflict.IdempotencyKey = "idem-dep-conflict-" + uuid.New().String()
		_, err = depositSettlementRepo.SettleDepositAtomic(ctx, paramsConflict)
		if err == nil {
			t.Fatalf("expected conflict settling already-settled deposit due with different key, got nil")
		}

		// 4. Bounds Check: refunded + deductions > original (50,000) must fail
		unsettledDueID := uuid.New()
		err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'"); err != nil {
				return err
			}
			dueCode := "DEX" + uuid.New().String()[:4]
			_, err = tx.Exec(ctx, `
				INSERT INTO dues (id, property_id, tenant_id, kind, original_amount, amount, status, due_code, due_date, period_start, period_end, created_at)
				VALUES ($1, $2, $3, 'deposit', 50000, 0, 'paid', $4, NOW(), NOW(), NOW() + INTERVAL '30 days', NOW())`,
				unsettledDueID, propID, tenantID, dueCode,
			)
			return err
		})
		if err != nil {
			t.Fatalf("failed inserting second deposit due: %v", err)
		}
		paramsOver := SettleDepositParams{
			PropertyID:      propID,
			TenantID:        tenantID,
			DepositDueID:    unsettledDueID,
			RefundedPaise:   40000,
			DeductionsPaise: 20000, // 40k + 20k = 60k > 50k
			IdempotencyKey:  "idem-over-" + uuid.New().String(),
			Reason:          "over refund",
		}
		_, err = depositSettlementRepo.SettleDepositAtomic(ctx, paramsOver)
		if err == nil {
			t.Fatalf("expected bounds check failure for 60,000 > 50,000, got nil")
		}

		// 5. Ownership Boundary: foreign property ID must return ErrForbidden
		foreignPropID := uuid.New()
		paramsForeign := paramsOver
		paramsForeign.PropertyID = foreignPropID
		paramsForeign.RefundedPaise = 25000
		paramsForeign.DeductionsPaise = 25000
		_, err = depositSettlementRepo.SettleDepositAtomic(ctx, paramsForeign)
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("expected ErrForbidden for foreign property ID, got: %v", err)
		}
	})

	// -------------------------------------------------------------
	// C-09: Universal Payment Outbox Atomicity
	// -------------------------------------------------------------
	t.Run("C-09: Single-Due Payment Atomically Enqueues Outbox Event", func(t *testing.T) {
		rentDueID := uuid.New()
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'"); err != nil {
				return err
			}
			dueCode := "RNT" + uuid.New().String()[:4]
			_, err = tx.Exec(ctx, `
				INSERT INTO dues (id, property_id, tenant_id, kind, original_amount, amount, status, due_code, due_date, period_start, period_end, created_at)
				VALUES ($1, $2, $3, 'rent', 15000, 15000, 'pending', $4, NOW(), NOW(), NOW() + INTERVAL '30 days', NOW())`,
				rentDueID, propID, tenantID, dueCode,
			)
			return err
		})
		if err != nil {
			t.Fatalf("failed inserting rent due: %v", err)
		}

		pay := &domain.Payment{
			PropertyID: &propID,
			DueID:      rentDueID,
			TenantID:   tenantID,
			Amount:     15000,
			MatchedBy:  domain.MatchedByCash,
			Provider:   "cash",
			MatchedAt:  time.Now().UTC(),
		}
		if err := paymentRepo.Create(ctx, pay); err != nil {
			t.Fatalf("PaymentRepo.Create failed: %v", err)
		}

		// Verify outbox event exists
		var outboxID int64
		var outboxPayload []byte
		err = pool.QueryRow(ctx, `
			SELECT id, payload FROM ledger_outbox_events
			WHERE property_id = $1 AND event_type = $2 AND source_id = $3`,
			propID, domain.LedgerOutboxPayment, pay.ID,
		).Scan(&outboxID, &outboxPayload)
		if err != nil {
			t.Fatalf("expected outbox event for cash payment, got: %v", err)
		}
		var mp domain.PaymentMirrorPayload
		if err := json.Unmarshal(outboxPayload, &mp); err != nil {
			t.Fatalf("unmarshal payment outbox payload: %v", err)
		}
		if mp.AmountPaise != 15000 || len(mp.Allocations) != 1 || mp.Allocations[0].DueKind != "rent" {
			t.Errorf("unexpected payment mirror payload: %+v", mp)
		}
	})

	// -------------------------------------------------------------
	// C-10: Payment Correction Outbox Atomicity
	// -------------------------------------------------------------
	t.Run("C-10: Payment Correction Atomically Enqueues Outbox Event", func(t *testing.T) {
		origPaymentID := uuid.New()
		reversalID := uuid.New()
		correctedID := uuid.New()
		userID := uuid.New()

		userPhone := "+91" + strconv.FormatInt((time.Now().UnixNano()+7)%10000000000, 10)
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'"); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO users (id, phone, role) VALUES ($1, $2, 'owner')
				ON CONFLICT (id) DO NOTHING`, userID, userPhone,
			)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO payments (id, property_id, tenant_id, amount, matched_by, provider, created_at)
				VALUES ($1, $2, $3, 25000, 'manual', 'manual', NOW()),
				       ($4, $2, $3, -25000, 'manual', 'correction', NOW()),
				       ($5, $2, $3, 30000, 'manual', 'manual', NOW())`,
				origPaymentID, propID, tenantID, reversalID, correctedID,
			)
			return err
		})
		if err != nil {
			t.Fatalf("failed inserting test payments for correction: %v", err)
		}
		corr := &domain.FinancialCorrection{
			PropertyID:         propID,
			OriginalPaymentID:  origPaymentID,
			ReversalPaymentID:  &reversalID,
			CorrectedPaymentID: &correctedID,
			Reason:             "wrong amount entered by owner",
			CorrectedBy:        userID,
			OccurredAt:         time.Now().UTC(),
		}
		if err := paymentRepo.RecordCorrection(ctx, corr); err != nil {
			t.Fatalf("RecordCorrection failed: %v", err)
		}

		// Verify correction_mirror outbox event was created
		var outboxID int64
		var outboxPayload []byte
		err = pool.QueryRow(ctx, `
			SELECT id, payload FROM ledger_outbox_events
			WHERE property_id = $1 AND event_type = $2 AND source_id = $3`,
			propID, domain.LedgerOutboxCorrection, corr.ID,
		).Scan(&outboxID, &outboxPayload)
		if err != nil {
			t.Fatalf("expected outbox event for correction, got: %v", err)
		}
		var cp domain.CorrectionMirrorPayload
		if err := json.Unmarshal(outboxPayload, &cp); err != nil {
			t.Fatalf("unmarshal correction outbox payload: %v", err)
		}
		if cp.AmountPaise != 25000 || cp.OriginalPaymentID != origPaymentID {
			t.Errorf("unexpected correction payload: %+v", cp)
		}
	})

	// -------------------------------------------------------------
	// C-11: Gamification Tenant Credit Transactional Atomicity
	// -------------------------------------------------------------
	t.Run("C-11: Gamification Tenant Credit & Outbox Rollback Atomicity", func(t *testing.T) {
		gamificationRepo := NewGamificationRepo(pool)

		// 1. Success case: credit applied and outbox inserted inside transaction
		redemptionID := uuid.New()
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'"); err != nil {
				return err
			}
			if err := gamificationRepo.AddTenantCreditTx(ctx, tx, tenantID, 50000); err != nil {
				return err
			}
			outboxEvt := &domain.LedgerOutboxEvent{
				EventType:      domain.LedgerOutboxRewardRedeem,
				PropertyID:     propID,
				SourceID:       redemptionID,
				Payload:        []byte(`{"points_spent": 500, "amount_paise": 50000}`),
				IdempotencyKey: fmt.Sprintf("reward_redeem:%s", redemptionID),
			}
			return gamificationRepo.InsertOutboxEventTx(ctx, tx, outboxEvt)
		})
		if err != nil {
			t.Fatalf("successful transaction failed: %v", err)
		}

		var creditBalance int64
		err = pool.QueryRow(ctx, `SELECT credit_balance_paise FROM tenants WHERE id = $1`, tenantID).Scan(&creditBalance)
		if err != nil {
			t.Fatalf("query credit balance failed: %v", err)
		}
		if creditBalance != 50000 {
			t.Errorf("expected tenant credit 50,000, got %d", creditBalance)
		}

		// 2. Rollback case: if error injected before commit, no credit survives!
		injectedErr := errors.New("simulated error before commit")
		_ = WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL app.ledger_maintenance = 'on'"); err != nil {
				return err
			}
			if err := gamificationRepo.AddTenantCreditTx(ctx, tx, tenantID, 99999); err != nil {
				return err
			}
			return injectedErr
		})

		// Assert balance did NOT change
		err = pool.QueryRow(ctx, `SELECT credit_balance_paise FROM tenants WHERE id = $1`, tenantID).Scan(&creditBalance)
		if err != nil {
			t.Fatalf("query credit balance after rollback failed: %v", err)
		}
		if creditBalance != 50000 {
			t.Fatalf("TRANSACTION LEAK: rolled back credit mutated tenant balance to %d!", creditBalance)
		}
	})
}
