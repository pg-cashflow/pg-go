package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLivePostgresMigration020AndRepository(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	// 1. Verify / Apply migrations 020, 021, and 022
	for _, mFile := range []string{
		"020_gateway_unmatched_and_refunds.sql",
		"021_tenants_minor_guardian_and_compliance.sql",
		"022_payout_batches_and_payees.sql",
	} {
		mPath := filepath.Join("..", "..", "migrations", mFile)
		mBytes, err := os.ReadFile(mPath)
		if err != nil {
			t.Fatalf("failed to read migration %s: %v", mFile, err)
		}
		if _, err := pool.Exec(ctx, string(mBytes)); err != nil {
			t.Fatalf("failed to apply migration %s SQL: %v", mFile, err)
		}
	}

	payRepo := NewPaymentRepo(pool)

	// 2. Test In-Transaction Deduplication (processed_webhook_events)
	provider := "cashfree"
	eventType := "PAYMENT_SETTLED"
	providerRefID := "live_test_" + uuid.New().String()
	eventStatus := ""

	firstSeen, err := payRepo.RecordProcessedEvent(ctx, provider, eventType, providerRefID, eventStatus)
	if err != nil {
		t.Fatalf("first RecordProcessedEvent failed: %v", err)
	}
	if !firstSeen {
		t.Fatalf("expected first RecordProcessedEvent to return true (first seen)")
	}

	// Repeat call with identical key -> must return false (deduplicated)
	secondSeen, err := payRepo.RecordProcessedEvent(ctx, provider, eventType, providerRefID, eventStatus)
	if err != nil {
		t.Fatalf("second RecordProcessedEvent failed: %v", err)
	}
	if secondSeen {
		t.Fatalf("expected duplicate RecordProcessedEvent to return false (deduplicated)")
	}

	// 3. Test Actionable Domain Unmatched Receipts (Provider-Neutral)
	orderID := "order_" + uuid.New().String()
	paymentID := "pay_" + uuid.New().String()
	amountPaise := int64(550000)
	rawPayload := []byte(`{"event":"PAYMENT_SUCCESS_WEBHOOK"}`)

	// Valid failure_reason CHECK: 'unknown_order'
	err = payRepo.RecordUnmatchedReceipt(ctx, orderID, paymentID, nil, amountPaise, "unknown_order", rawPayload)
	if err != nil {
		t.Fatalf("RecordUnmatchedReceipt with 'unknown_order' failed: %v", err)
	}

	// Duplicate unmatched insert -> ON CONFLICT DO NOTHING -> should not error
	err = payRepo.RecordUnmatchedReceipt(ctx, orderID, paymentID, nil, amountPaise, "unknown_order", rawPayload)
	if err != nil {
		t.Fatalf("duplicate RecordUnmatchedReceipt failed: %v", err)
	}

	// 4. Test Raw Webhook Ingestion Table (webhook_events)
	wbEvent := &domain.WebhookEvent{
		Provider:         "cashfree",
		EventType:        "PAYMENT_SUCCESS_WEBHOOK",
		RawPayload:       rawPayload,
		ProcessingStatus: "received",
	}
	err = payRepo.CreateWebhookEvent(ctx, wbEvent)
	if err != nil {
		t.Fatalf("CreateWebhookEvent failed: %v", err)
	}
	if wbEvent.ID == uuid.Nil {
		t.Fatalf("expected non-nil UUID for created webhook event")
	}

	// Update status
	errMsg := "test error"
	err = payRepo.UpdateWebhookEventStatus(ctx, wbEvent.ID, "dead_letter", &errMsg)
	if err != nil {
		t.Fatalf("UpdateWebhookEventStatus failed: %v", err)
	}

	// 5. Test Active Intent Double-Active Guard (uq_active_intent_due)
	propID := uuid.New()
	tenantID := uuid.New()
	dueID := uuid.New()
	inviteCode := fmt.Sprintf("P%s", uuid.New().String()[:7])

	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Live Test Prop', 'Address', '+919999988888', 'prop@upi', 'Owner', 'o@test.com', $2)
	`, propID, inviteCode)
	if err != nil {
		t.Fatalf("insert test property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	}()

	tenantPhone := fmt.Sprintf("+9199%08d", time.Now().UnixNano()%100000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Test Tenant', $3, '101', 550000, 5, 'active')
	`, tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("insert test tenant: %v", err)
	}

	dueCode := uuid.New().String()[:8]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $2, $3, $4, 550000, 550000, 'pending', CURRENT_DATE, 'rent', CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month')
	`, dueID, dueCode, propID, tenantID)
	if err != nil {
		t.Fatalf("insert test due: %v", err)
	}

	// Insert first intent with status 'initiating'
	intent1ID := uuid.New()
	cfOrderID1 := "order_init_" + uuid.New().String()[:8]
	_, err = pool.Exec(ctx, `
		INSERT INTO payment_intents (id, due_id, provider, provider_order_id, amount_paise, status, expires_at)
		VALUES ($1, $2, 'cashfree', $3, 550000, 'initiating', NOW() + INTERVAL '30 minutes')
	`, intent1ID, dueID, cfOrderID1)
	if err != nil {
		t.Fatalf("insert first active intent ('initiating'): %v", err)
	}

	// Insert second active intent on SAME due with status 'created' -> MUST FAIL with unique violation
	intent2ID := uuid.New()
	cfOrderID2 := "order_init_" + uuid.New().String()[:8]
	_, err = pool.Exec(ctx, `
		INSERT INTO payment_intents (id, due_id, provider, provider_order_id, amount_paise, status, expires_at)
		VALUES ($1, $2, 'cashfree', $3, 550000, 'created', NOW() + INTERVAL '30 minutes')
	`, intent2ID, dueID, cfOrderID2)
	if err == nil {
		t.Fatalf("expected unique violation on uq_active_intent_due for second concurrent active intent, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected SQLSTATE 23505 (unique_violation), got: %v", err)
	}

	// Supersede first intent
	_, err = pool.Exec(ctx, `UPDATE payment_intents SET status = 'superseded' WHERE id = $1`, intent1ID)
	if err != nil {
		t.Fatalf("supersede first intent: %v", err)
	}

	// Now inserting second intent with 'created' MUST SUCCEED
	_, err = pool.Exec(ctx, `
		INSERT INTO payment_intents (id, due_id, provider, provider_order_id, amount_paise, status, expires_at)
		VALUES ($1, $2, 'cashfree', $3, 550000, 'created', NOW() + INTERVAL '30 minutes')
	`, intent2ID, dueID, cfOrderID2)
	if err != nil {
		t.Fatalf("insert second intent after superseding first intent: %v", err)
	}

	// 6. Test Multi-Due Active Intent Guard (payment_intent_dues with due_id IS NULL on parent)
	dueMulti1 := uuid.New()
	dueMulti2 := uuid.New()
	dueMultiCode1 := uuid.New().String()[:8]
	dueMultiCode2 := uuid.New().String()[:8]

	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES 
			($1, $2, $3, $4, 550000, 550000, 'pending', CURRENT_DATE + INTERVAL '2 month', 'rent', CURRENT_DATE + INTERVAL '2 month', CURRENT_DATE + INTERVAL '3 month'),
			($5, $6, $3, $4, 550000, 550000, 'pending', CURRENT_DATE + INTERVAL '3 month', 'rent', CURRENT_DATE + INTERVAL '3 month', CURRENT_DATE + INTERVAL '4 month')
	`, dueMulti1, dueMultiCode1, propID, tenantID, dueMulti2, dueMultiCode2)
	if err != nil {
		t.Fatalf("insert test multi dues: %v", err)
	}

	intentRepo := NewPaymentIntentRepo(pool)

	// First multi-due intent covering dueMulti1 and dueMulti2 (due_id IS NULL on parent)
	mIntent1 := &domain.PaymentIntent{
		ID:              uuid.New(),
		Provider:        "cashfree",
		ProviderOrderID: "order_m1_" + uuid.New().String()[:8],
		AmountPaise:     1100000,
		Status:          domain.IntentInitiating,
	}
	err = intentRepo.CreateWithDues(ctx, mIntent1, []uuid.UUID{dueMulti1, dueMulti2}, []int64{550000, 550000})
	if err != nil {
		t.Fatalf("CreateWithDues for first multi-due intent: %v", err)
	}

	// Second concurrent multi-due intent trying to cover dueMulti2 (overlapping due)
	mIntent2 := &domain.PaymentIntent{
		ID:              uuid.New(),
		Provider:        "cashfree",
		ProviderOrderID: "order_m2_" + uuid.New().String()[:8],
		AmountPaise:     550000,
		Status:          domain.IntentCreated,
	}
	err = intentRepo.CreateWithDues(ctx, mIntent2, []uuid.UUID{dueMulti2}, []int64{550000})
	if err == nil {
		t.Fatalf("expected unique violation on uq_active_intent_dues_item for overlapping multi-due intent, got nil")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected SQLSTATE 23505 on overlapping multi-due intent due_id, got: %v", err)
	}

	// Verify atomicity rollback: parent payment_intent row must NOT exist
	var mIntent2Count int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM payment_intents WHERE id = $1`, mIntent2.ID).Scan(&mIntent2Count)
	if err != nil {
		t.Fatalf("query mIntent2 count: %v", err)
	}
	if mIntent2Count != 0 {
		t.Fatalf("atomicity violation: failed CreateWithDues left orphaned parent payment_intent %v in database", mIntent2.ID)
	}

	// Supersede parent intent 1 -> Trigger sync_payment_intent_dues_status automatically updates child rows
	_, err = pool.Exec(ctx, `UPDATE payment_intents SET status = 'superseded' WHERE id = $1`, mIntent1.ID)
	if err != nil {
		t.Fatalf("supersede multi-due intent 1: %v", err)
	}

	// Verify trigger synchronized status on payment_intent_dues
	var childStatus string
	err = pool.QueryRow(ctx, `SELECT status FROM payment_intent_dues WHERE payment_intent_id = $1 LIMIT 1`, mIntent1.ID).Scan(&childStatus)
	if err != nil || childStatus != "superseded" {
		t.Fatalf("expected payment_intent_dues status to be synced to 'superseded', got: %q (err: %v)", childStatus, err)
	}

	// Now creating multi-due intent 3 covering dueMulti2 MUST SUCCEED (fresh intent and order ID)
	mIntent3 := &domain.PaymentIntent{
		ID:              uuid.New(),
		Provider:        "cashfree",
		ProviderOrderID: "order_m3_" + uuid.New().String()[:8],
		AmountPaise:     550000,
		Status:          domain.IntentCreated,
	}
	err = intentRepo.CreateWithDues(ctx, mIntent3, []uuid.UUID{dueMulti2}, []int64{550000})
	if err != nil {
		t.Fatalf("insert multi-due intent after superseding first multi-due intent failed: %v", err)
	}

	// 7. Verify BEFORE UPDATE Guard refuses direct application mutations on payment_intent_dues.status
	_, err = pool.Exec(ctx, `UPDATE payment_intent_dues SET status = 'paid' WHERE payment_intent_id = $1`, mIntent3.ID)
	if err == nil {
		t.Fatalf("expected direct UPDATE on payment_intent_dues.status to be refused by trg_prevent_direct_payment_intent_dues_status_update, got nil")
	}

	// 8. Verify Defense-in-Depth: uq_refunds_reference partial unique index on gateway_refunds
	basePaymentID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, tenant_id, amount, matched_by, provider, provider_payment_id, cf_payment_id, created_at)
		VALUES ($1, $2, 550000, 'cashfree', 'cashfree', $3, $3, NOW())`,
		basePaymentID, tenantID, "cf_pay_"+uuid.New().String()[:8],
	)
	if err != nil {
		t.Fatalf("insert base payment: %v", err)
	}

	refString := "ref_system_" + uuid.New().String()[:8]
	_, err = pool.Exec(ctx, `
		INSERT INTO gateway_refunds (id, payment_id, property_id, provider, provider_refund_id, refund_reference, amount_paise, status, cf_refund_id, reason, source)
		VALUES ($1, $2, $3, 'cashfree', $4, $5, 100000, 'initiated', $4, 'test refund 1', 'system')`,
		uuid.New(), basePaymentID, propID, "cf_ref_1_"+uuid.New().String()[:8], refString,
	)
	if err != nil {
		t.Fatalf("insert first refund with reference: %v", err)
	}

	// Inserting duplicate refund_reference must fail with SQLSTATE 23505
	_, err = pool.Exec(ctx, `
		INSERT INTO gateway_refunds (id, payment_id, property_id, provider, provider_refund_id, refund_reference, amount_paise, status, cf_refund_id, reason, source)
		VALUES ($1, $2, $3, 'cashfree', $4, $5, 100000, 'initiated', $4, 'test refund 2', 'system')`,
		uuid.New(), basePaymentID, propID, "cf_ref_2_"+uuid.New().String()[:8], refString,
	)
	if err == nil {
		t.Fatalf("expected unique violation on uq_refunds_reference for duplicate refund_reference, got nil")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected SQLSTATE 23505 on uq_refunds_reference, got: %v", err)
	}

	// Two refunds with NULL refund_reference must both succeed (partial index semantics)
	_, err = pool.Exec(ctx, `
		INSERT INTO gateway_refunds (id, payment_id, property_id, provider, provider_refund_id, refund_reference, amount_paise, status, cf_refund_id, reason, source)
		VALUES ($1, $2, $3, 'cashfree', $4, NULL, 50000, 'initiated', $4, 'auto refund 1', 'cashfree_auto')`,
		uuid.New(), basePaymentID, propID, "cf_auto_1_"+uuid.New().String()[:8],
	)
	if err != nil {
		t.Fatalf("insert first auto-refund with NULL reference: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO gateway_refunds (id, payment_id, property_id, provider, provider_refund_id, refund_reference, amount_paise, status, cf_refund_id, reason, source)
		VALUES ($1, $2, $3, 'cashfree', $4, NULL, 50000, 'initiated', $4, 'auto refund 2', 'cashfree_auto')`,
		uuid.New(), basePaymentID, propID, "cf_auto_2_"+uuid.New().String()[:8],
	)
	if err != nil {
		t.Fatalf("insert second auto-refund with NULL reference must succeed: %v", err)
	}

	// 9. Verify GetByDueID finds multi-due payment where payments.due_id IS NULL via payment_allocations
	cfPayID := "cf_multi_" + uuid.New().String()[:8]
	multiPayment := &domain.Payment{
		ID:                uuid.New(),
		DueID:             uuid.Nil, // multi-due payment has NULL due_id
		TenantID:          tenantID,
		Amount:            1100000,
		MatchedBy:         domain.MatchedByCashfree,
		CFPaymentID:       &cfPayID,
		ProviderPaymentID: &cfPayID,
		CreatedAt:         time.Now(),
	}
	err = payRepo.Create(ctx, multiPayment)
	if err != nil {
		t.Fatalf("create multi-due payment: %v", err)
	}
	err = payRepo.CreateAllocation(ctx, multiPayment.ID, dueMulti1, 550000)
	if err != nil {
		t.Fatalf("create allocation 1 for multi-due payment: %v", err)
	}
	foundPayment, err := payRepo.GetByDueID(ctx, dueMulti1)
	if err != nil || foundPayment == nil || foundPayment.ID != multiPayment.ID {
		t.Fatalf("expected GetByDueID to find payment via payment_allocations, got payment=%v, err=%v", foundPayment, err)
	}

	// 10. Verify Single-Due payment auto-creates payment_allocations row (guaranteeing 100% SSoT)
	singleDuePayment := &domain.Payment{
		ID:        uuid.New(),
		DueID:     dueMulti2,
		TenantID:  tenantID,
		Amount:    550000,
		MatchedBy: domain.MatchedByCash,
		Provider:  "cash",
		CreatedAt: time.Now(),
	}
	err = payRepo.Create(ctx, singleDuePayment)
	if err != nil {
		t.Fatalf("create single-due cash payment: %v", err)
	}

	allocs, err := payRepo.ListAllocationsByPayment(ctx, singleDuePayment.ID)
	if err != nil {
		t.Fatalf("ListAllocationsByPayment failed: %v", err)
	}
	if len(allocs) != 1 || allocs[0].DueID != dueMulti2 || allocs[0].AmountPaise != 550000 {
		t.Fatalf("expected 1 allocation automatically created for single-due payment, got: %+v", allocs)
	}

	// 11. Step 1c: Non-blocking Constraint Validation Sweep
	// Validates that historical data and newly inserted rows strictly satisfy all NOT VALID constraints.
	constraintValidations := []string{
		`ALTER TABLE unmatched_gateway_receipts VALIDATE CONSTRAINT chk_unmatched_provider_id_sync;`,
		`ALTER TABLE payments VALIDATE CONSTRAINT chk_payments_matched_by;`,
		`ALTER TABLE payments VALIDATE CONSTRAINT chk_payments_provider_id_sync;`,
		`ALTER TABLE gateway_refunds VALIDATE CONSTRAINT chk_refunds_provider_id_sync;`,
	}
	for _, sqlStmt := range constraintValidations {
		if _, err := pool.Exec(ctx, sqlStmt); err != nil {
			t.Fatalf("Step 1c constraint validation failed on [%s]: %v", sqlStmt, err)
		}
	}
}
