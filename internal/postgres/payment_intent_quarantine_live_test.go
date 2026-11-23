package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLivePaymentIntentQuarantineRequiresProviderConfirmation verifies the
// database guard added by migration 123. A quarantine cannot resolve before a
// first terminal observation is stored. MarkResolved represents the worker's
// second matching provider read and completes the confirmation counter.
func TestLivePaymentIntentQuarantineRequiresProviderConfirmation(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	propertyID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()
	secondDueID := uuid.New()
	intentID := uuid.New()
	phone := fmt.Sprintf("+9199%08d", time.Now().UnixNano()%100000000)
	inviteCode := "Q" + uuid.New().String()[:8]
	orderID := "quarantine-live-" + uuid.New().String()
	upiVPA := "quarantine-" + uuid.New().String()[:12] + "@upi"

	if _, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Quarantine Live Test', 'Quarantine Test Address', $2, $3, 'Test Owner', $4, $5)`,
		propertyID, phone, upiVPA, "quarantine-"+uuid.New().String()+"@test.invalid", inviteCode,
	); err != nil {
		t.Fatalf("seed property: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		// Migration 123 deliberately uses ON DELETE RESTRICT so quarantine
		// history cannot disappear through ordinary intent cleanup.
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM unmatched_gateway_receipts WHERE provider_payment_id = $1`, "cf-quarantine-"+intentID.String())
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM payment_intent_quarantine WHERE payment_intent_id = $1`, intentID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM payment_intents WHERE id = $1`, intentID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM dues WHERE id = ANY($1::uuid[])`, []uuid.UUID{dueID, secondDueID})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM tenants WHERE id = $1`, tenantID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM properties WHERE id = $1`, propertyID)
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Quarantine Test Tenant', $3, 'Q-1', 12500, 5, 'active')`,
		tenantID, propertyID, fmt.Sprintf("+9198%08d", (time.Now().UnixNano()+1)%100000000),
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount,
		                  period_start, period_end, due_date, status)
		VALUES ($1, $3, $5, $6, 'rent', 6250, 6250, DATE '2026-11-01', DATE '2026-11-30', DATE '2026-11-05', 'pending'),
		       ($2, $4, $5, $6, 'rent', 6250, 6250, DATE '2026-12-01', DATE '2026-12-31', DATE '2026-12-05', 'pending')`,
		dueID, secondDueID, "Q"+uuid.New().String()[:7], "Q"+uuid.New().String()[:7], tenantID, propertyID,
	); err != nil {
		t.Fatalf("seed multi-due set: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_intents (id, due_id, provider, provider_order_id, amount_paise, status, expires_at)
		VALUES ($1, NULL, 'cashfree', $2, 12500, 'created', NOW() + INTERVAL '10 minutes')`,
		intentID, orderID,
	); err != nil {
		t.Fatalf("seed multi-due payment intent: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_intent_dues (payment_intent_id, due_id, amount_paise, allocation_order, status)
		VALUES ($1, $2, 6250, 1, 'created'), ($1, $3, 6250, 2, 'created')`,
		intentID, dueID, secondDueID,
	); err != nil {
		t.Fatalf("seed payment intent dues snapshot: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE payment_intents SET status = 'superseded' WHERE id = $1`, intentID); err != nil {
		t.Fatalf("supersede payment intent and enqueue quarantine: %v", err)
	}

	var quarantineID int64
	var dueIDs []uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id, due_ids FROM payment_intent_quarantine WHERE payment_intent_id = $1`, intentID).Scan(&quarantineID, &dueIDs); err != nil {
		t.Fatalf("read trigger-created quarantine row (migration 123 must be installed): %v", err)
	}
	if len(dueIDs) != 2 {
		t.Fatalf("quarantine due snapshot has %d IDs; want both dues: %v", len(dueIDs), dueIDs)
	}

	repo := NewPaymentIntentQuarantineRepo(pool)
	if err := repo.MarkResolved(ctx, quarantineID, "TERMINATED"); err == nil || !strings.Contains(err.Error(), "second matching") {
		t.Fatalf("quarantine resolved without a stored terminal observation: %v", err)
	}
	if err := repo.RecordAttempt(ctx, quarantineID, "needs_review", "TERMINATED", nil, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("record first terminal observation: %v", err)
	}

	paymentID := "cf-quarantine-" + intentID.String()
	if err := NewPaymentRepo(pool).RecordUnmatchedReceipt(ctx, orderID, paymentID, &intentID, 12500, "session_superseded", []byte(`{"event":"late_capture"}`)); err != nil {
		t.Fatalf("record late multi-due capture: %v", err)
	}
	var receiptDueID, receiptTenantID, receiptPropertyID uuid.UUID
	var receiptIntentID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT due_id, tenant_id, property_id, payment_intent_id
		FROM unmatched_gateway_receipts WHERE provider_payment_id = $1`, paymentID,
	).Scan(&receiptDueID, &receiptTenantID, &receiptPropertyID, &receiptIntentID); err != nil {
		t.Fatalf("read multi-due unmatched receipt: %v", err)
	}
	if receiptDueID != dueID || receiptTenantID != tenantID || receiptPropertyID != propertyID || receiptIntentID != intentID {
		t.Fatalf("multi-due receipt linkage mismatch: due=%s tenant=%s property=%s intent=%s", receiptDueID, receiptTenantID, receiptPropertyID, receiptIntentID)
	}

	if err := repo.RecordAttempt(ctx, quarantineID, "needs_review", "TERMINATED", nil, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("record second terminal observation: %v", err)
	}
	if err := repo.MarkResolved(ctx, quarantineID, "TERMINATED"); err == nil || !strings.Contains(err.Error(), "unresolved unmatched receipt") {
		t.Fatalf("quarantine resolved while a late-capture receipt was still open: %v", err)
	}

	var status string
	var confirmations int16
	if err := pool.QueryRow(ctx, `
		SELECT status, terminal_confirmation_count
		FROM payment_intent_quarantine WHERE id = $1`, quarantineID,
	).Scan(&status, &confirmations); err != nil {
		t.Fatalf("read quarantined order: %v", err)
	}
	if status == "resolved" || confirmations != 2 {
		t.Fatalf("quarantine state = %q with %d confirmations; want held after two provider observations while owner receipt is open", status, confirmations)
	}
}
