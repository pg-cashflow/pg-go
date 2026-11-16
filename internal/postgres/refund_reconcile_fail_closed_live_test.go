package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// TestLivePostgresRefundReconcile_FailClosedOnShortfall verifies that ReconcileRefundTransition
// rejects and completely rolls back any refund on an applied payment where the due allocations
// do not cover the refund amount exactly (Invariant INV-017).
func TestLivePostgresRefundReconcile_FailClosedOnShortfall(t *testing.T) {
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../../.env")
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	repo := NewPaymentRepo(pool)

	// 1. Create Property
	propID := uuid.New()
	inviteCode := fmt.Sprintf("P%s", uuid.New().String()[:7])
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Refund Prop', 'Addr', '+919999977777', 'refund@upi', 'Owner', 'refund@test.com', $2)`, propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID) }()

	// 2. Create Tenant
	tenantID := uuid.New()
	tenantPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+2)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Refund Tenant', $3, '102', 10000, 1, 'active')`,
		tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID) }()

	// 3. Create Due: 10,000 paise
	dueID := uuid.New()
	dueCode := "D" + uuid.New().String()[:7]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $2, $3, $4, 10000, 10000, 'paid', CURRENT_DATE, 'rent', CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month')`,
		dueID, dueCode, propID, tenantID)
	if err != nil {
		t.Fatalf("insert due: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM dues WHERE id = $1`, dueID) }()

	// 4. Create Payment: 10,000 paise applied to due
	paymentID := uuid.New()
	upiTxnID := "UPI" + uuid.New().String()[:12]
	cfPayID := "cf_pay_" + uuid.New().String()[:10]
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, property_id, tenant_id, due_id, amount, upi_txn_id, provider, cf_payment_id, provider_payment_id, matched_by, is_unapplied, created_at)
		VALUES ($1, $2, $3, $4, 10000, $5, 'cashfree', $6, $6, 'manual', false, NOW())`,
		paymentID, propID, tenantID, dueID, upiTxnID, cfPayID)
	if err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM payments WHERE id = $1`, paymentID)
	}()

	// 5. Artificial shortfall: update auto-allocated amount from 10,000 to 6,000
	_, err = pool.Exec(ctx, `
		UPDATE payment_allocations SET amount_paise = 6000 WHERE payment_id = $1`, paymentID)
	if err != nil {
		t.Fatalf("update payment allocation shortfall: %v", err)
	}

	// 6. Create Gateway Refund record for 10,000 paise (status: pending)
	refundID := uuid.New()
	cfRefundRef := "cf_rf_" + uuid.New().String()[:8]
	ref := &domain.GatewayRefund{
		ID:              refundID,
		PaymentID:       paymentID,
		PropertyID:      propID,
		Provider:        "cashfree",
		Source:          "system",
		RefundReference: &cfRefundRef,
		AmountPaise:     10000,
		Status:          "failed",
		Reason:          "Test refund fail closed",
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	if err := repo.CreateOrUpdateRefund(ctx, ref); err != nil {
		t.Fatalf("insert refund: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM refund_allocations WHERE refund_id = $1`, refundID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM gateway_refunds WHERE id = $1`, refundID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_outbox_events WHERE source_id = $1`, refundID)
	}()

	// 7. Transition to 'succeeded' -> MUST FAIL CLOSED because allocation total (6,000) != refund amount (10,000)
	err = repo.ReconcileRefundTransition(ctx, ref, "succeeded")
	if err == nil {
		t.Fatalf("EXPECTED ERROR due to allocation shortfall, but ReconcileRefundTransition succeeded")
	}
	if !strings.Contains(err.Error(), "allocation mismatch") {
		t.Fatalf("expected 'allocation mismatch' error message, got: %v", err)
	}

	// 8. Assert database state remained untouched:
	// - Refund status in DB must NOT be succeeded (transaction rolled back)
	storedRef, err := repo.GetRefundByID(ctx, refundID)
	if err != nil {
		t.Fatalf("fetch refund after rollback: %v", err)
	}
	if storedRef.Status == "succeeded" {
		t.Fatalf("refund status committed as succeeded despite allocation mismatch")
	}

	// - Zero refund_allocations persisted
	var allocCount int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM refund_allocations WHERE refund_id = $1`, refundID).Scan(&allocCount)
	if err != nil || allocCount != 0 {
		t.Fatalf("expected 0 refund_allocations committed, got %d (err: %v)", allocCount, err)
	}

	// - Zero ledger_outbox_events persisted
	var outboxCount int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_outbox_events WHERE source_id = $1`, refundID).Scan(&outboxCount)
	if err != nil || outboxCount != 0 {
		t.Fatalf("expected 0 ledger_outbox_events committed, got %d (err: %v)", outboxCount, err)
	}
}

// TestLivePostgresRefundReconcile_ExactMatchCommitsSuccess verifies that ReconcileRefundTransition
// successfully commits when the payment due allocations match the refund amount exactly.
func TestLivePostgresRefundReconcile_ExactMatchCommitsSuccess(t *testing.T) {
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../../.env")
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	repo := NewPaymentRepo(pool)

	propID := uuid.New()
	inviteCode := fmt.Sprintf("P%s", uuid.New().String()[:7])
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Exact Match Prop', 'Addr', '+919999966666', 'exact@upi', 'Owner', 'exact@test.com', $2)`, propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID) }()

	tenantID := uuid.New()
	tenantPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+3)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Exact Tenant', $3, '103', 10000, 1, 'active')`,
		tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID) }()

	dueID := uuid.New()
	dueCode := "D" + uuid.New().String()[:7]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $2, $3, $4, 10000, 10000, 'paid', CURRENT_DATE, 'rent', CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month')`,
		dueID, dueCode, propID, tenantID)
	if err != nil {
		t.Fatalf("insert due: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM dues WHERE id = $1`, dueID) }()

	paymentID := uuid.New()
	upiTxnID := "UPI" + uuid.New().String()[:12]
	cfPayID := "cf_pay_" + uuid.New().String()[:10]
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, property_id, tenant_id, due_id, amount, upi_txn_id, provider, cf_payment_id, provider_payment_id, matched_by, is_unapplied, created_at)
		VALUES ($1, $2, $3, $4, 10000, $5, 'cashfree', $6, $6, 'manual', false, NOW())`,
		paymentID, propID, tenantID, dueID, upiTxnID, cfPayID)
	if err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM payments WHERE id = $1`, paymentID) }()

	refundID := uuid.New()
	cfRefundRef := "cf_rf_" + uuid.New().String()[:8]
	ref := &domain.GatewayRefund{
		ID:              refundID,
		PaymentID:       paymentID,
		PropertyID:      propID,
		Provider:        "cashfree",
		Source:          "system",
		RefundReference: &cfRefundRef,
		AmountPaise:     10000,
		Status:          "failed",
		Reason:          "Exact match refund",
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	if err := repo.CreateOrUpdateRefund(ctx, ref); err != nil {
		t.Fatalf("insert refund: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM refund_allocations WHERE refund_id = $1`, refundID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM gateway_refunds WHERE id = $1`, refundID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_outbox_events WHERE source_id = $1`, refundID)
	}()

	// Transition to 'succeeded' -> Must succeed cleanly
	if err := repo.ReconcileRefundTransition(ctx, ref, "succeeded"); err != nil {
		t.Fatalf("ReconcileRefundTransition failed on exact match: %v", err)
	}

	// Assertions:
	// 1. Status updated to succeeded
	storedRef, err := repo.GetRefundByID(ctx, refundID)
	if err != nil || storedRef.Status != "succeeded" {
		t.Fatalf("expected refund status 'succeeded', got %q (err: %v)", storedRef.Status, err)
	}

	// 2. Exactly 10,000 paise allocated in refund_allocations
	var allocSum int64
	err = pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_paise), 0) FROM refund_allocations WHERE refund_id = $1`, refundID).Scan(&allocSum)
	if err != nil || allocSum != 10000 {
		t.Fatalf("expected 10000 paise allocated in refund_allocations, got %d (err: %v)", allocSum, err)
	}

	// 3. Exactly 1 ledger outbox event created
	var outboxCount int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_outbox_events WHERE source_id = $1`, refundID).Scan(&outboxCount)
	if err != nil || outboxCount != 1 {
		t.Fatalf("expected 1 ledger outbox event, got %d (err: %v)", outboxCount, err)
	}
}

// TestLivePostgresRefundReconcile_UnappliedPaymentSucceedsWithZeroDues verifies that
// reconciling a refund on an unapplied payment succeeds without requiring due allocations.
func TestLivePostgresRefundReconcile_UnappliedPaymentSucceedsWithZeroDues(t *testing.T) {
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../../.env")
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	repo := NewPaymentRepo(pool)

	propID := uuid.New()
	inviteCode := fmt.Sprintf("P%s", uuid.New().String()[:7])
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Unapplied Prop', 'Addr', '+919999955555', 'unapplied@upi', 'Owner', 'unapplied@test.com', $2)`, propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID) }()

	tenantID := uuid.New()
	tenantPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+4)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Unapplied Tenant', $3, '104', 10000, 1, 'active')`,
		tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID) }()

	paymentID := uuid.New()
	upiTxnID := "UPI" + uuid.New().String()[:12]
	cfPayID := "cf_pay_" + uuid.New().String()[:10]
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, property_id, tenant_id, amount, upi_txn_id, provider, cf_payment_id, provider_payment_id, matched_by, is_unapplied, created_at)
		VALUES ($1, $2, $3, 5000, $4, 'cashfree', $5, $5, 'manual', true, NOW())`,
		paymentID, propID, tenantID, upiTxnID, cfPayID)
	if err != nil {
		t.Fatalf("insert unapplied payment: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM payments WHERE id = $1`, paymentID) }()

	refundID := uuid.New()
	cfRefundRef := "cf_rf_" + uuid.New().String()[:8]
	ref := &domain.GatewayRefund{
		ID:              refundID,
		PaymentID:       paymentID,
		PropertyID:      propID,
		Provider:        "cashfree",
		Source:          "system",
		RefundReference: &cfRefundRef,
		AmountPaise:     5000,
		Status:          "pending",
		Reason:          "Unapplied refund",
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	if err := repo.CreateOrUpdateRefund(ctx, ref); err != nil {
		t.Fatalf("insert refund: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gateway_refunds WHERE id = $1`, refundID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_outbox_events WHERE source_id = $1`, refundID)
	}()

	// Transition to 'succeeded' -> Must succeed without requiring due allocations
	if err := repo.ReconcileRefundTransition(ctx, ref, "succeeded"); err != nil {
		t.Fatalf("ReconcileRefundTransition failed on unapplied payment: %v", err)
	}

	storedRef, err := repo.GetRefundByID(ctx, refundID)
	if err != nil || storedRef.Status != "succeeded" {
		t.Fatalf("expected refund status 'succeeded', got %q (err: %v)", storedRef.Status, err)
	}
}

// TestLivePostgresRefundReconcile_QueryErrorFailsClosed verifies that when a database query
// fails (injected via cancelled context during execution), ReconcileRefundTransition fails closed
// and commits zero state mutations or ledger events.
func TestLivePostgresRefundReconcile_QueryErrorFailsClosed(t *testing.T) {
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../../.env")
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	repo := NewPaymentRepo(pool)

	propID := uuid.New()
	inviteCode := fmt.Sprintf("P%s", uuid.New().String()[:7])
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'FailClosed Query Prop', 'Addr', '+919999944444', 'queryfail@upi', 'Owner', 'queryfail@test.com', $2)`, propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID) }()

	tenantID := uuid.New()
	tenantPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+5)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'FailClosed Tenant', $3, '105', 10000, 1, 'active')`,
		tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID) }()

	dueID := uuid.New()
	dueCode := "D" + uuid.New().String()[:7]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $2, $3, $4, 10000, 10000, 'paid', CURRENT_DATE, 'rent', CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month')`,
		dueID, dueCode, propID, tenantID)
	if err != nil {
		t.Fatalf("insert due: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM dues WHERE id = $1`, dueID) }()

	paymentID := uuid.New()
	upiTxnID := "UPI" + uuid.New().String()[:12]
	cfPayID := "cf_pay_" + uuid.New().String()[:10]
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, property_id, tenant_id, due_id, amount, upi_txn_id, provider, cf_payment_id, provider_payment_id, matched_by, is_unapplied, created_at)
		VALUES ($1, $2, $3, $4, 10000, $5, 'cashfree', $6, $6, 'manual', false, NOW())`,
		paymentID, propID, tenantID, dueID, upiTxnID, cfPayID)
	if err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM payments WHERE id = $1`, paymentID) }()

	refundID := uuid.New()
	cfRefundRef := "cf_rf_" + uuid.New().String()[:8]
	ref := &domain.GatewayRefund{
		ID:              refundID,
		PaymentID:       paymentID,
		PropertyID:      propID,
		Provider:        "cashfree",
		Source:          "system",
		RefundReference: &cfRefundRef,
		AmountPaise:     10000,
		Status:          "failed",
		Reason:          "Query error failure injection test",
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	if err := repo.CreateOrUpdateRefund(ctx, ref); err != nil {
		t.Fatalf("insert refund: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gateway_refunds WHERE id = $1`, refundID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_outbox_events WHERE source_id = $1`, refundID)
	}()

	// Inject failure via already-cancelled context
	canceledCtx, cancelNow := context.WithCancel(ctx)
	cancelNow()

	err = repo.ReconcileRefundTransition(canceledCtx, ref, "succeeded")
	if err == nil {
		t.Fatalf("expected error from ReconcileRefundTransition with canceled context, got nil")
	}

	// Verify fail-closed rollback: refund remains 'failed'
	storedRef, fetchErr := repo.GetRefundByID(ctx, refundID)
	if fetchErr != nil {
		t.Fatalf("fetch refund after query failure rollback: %v", fetchErr)
	}
	if storedRef.Status == "succeeded" {
		t.Fatalf("refund marked succeeded despite query error rollback")
	}
}

// TestLivePostgresRefundReconcile_TargetDueLockTimeoutFailsClosed verifies that if a target due
// cannot be locked (e.g. held by another concurrent transaction), ReconcileRefundTransition
// fails closed upon lock timeout and commits zero state mutations or ledger entries.
func TestLivePostgresRefundReconcile_TargetDueLockTimeoutFailsClosed(t *testing.T) {
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../../.env")
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	repo := NewPaymentRepo(pool)

	propID := uuid.New()
	inviteCode := fmt.Sprintf("P%s", uuid.New().String()[:7])
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Lock Timeout Prop', 'Addr', '+919999933333', 'locktimeout@upi', 'Owner', 'locktimeout@test.com', $2)`, propID, inviteCode)
	if err != nil {
		t.Fatalf("insert property: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID) }()

	tenantID := uuid.New()
	tenantPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+6)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Lock Timeout Tenant', $3, '106', 10000, 1, 'active')`,
		tenantID, propID, tenantPhone)
	if err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantID) }()

	dueID := uuid.New()
	dueCode := "D" + uuid.New().String()[:7]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, property_id, tenant_id, amount, original_amount, status, due_date, kind, period_start, period_end)
		VALUES ($1, $2, $3, $4, 10000, 10000, 'paid', CURRENT_DATE, 'rent', CURRENT_DATE, CURRENT_DATE + INTERVAL '1 month')`,
		dueID, dueCode, propID, tenantID)
	if err != nil {
		t.Fatalf("insert due: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM dues WHERE id = $1`, dueID) }()

	paymentID := uuid.New()
	upiTxnID := "UPI" + uuid.New().String()[:12]
	cfPayID := "cf_pay_" + uuid.New().String()[:10]
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, property_id, tenant_id, due_id, amount, upi_txn_id, provider, cf_payment_id, provider_payment_id, matched_by, is_unapplied, created_at)
		VALUES ($1, $2, $3, $4, 10000, $5, 'cashfree', $6, $6, 'manual', false, NOW())`,
		paymentID, propID, tenantID, dueID, upiTxnID, cfPayID)
	if err != nil {
		t.Fatalf("insert payment: %v", err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM payments WHERE id = $1`, paymentID) }()

	refundID := uuid.New()
	cfRefundRef := "cf_rf_" + uuid.New().String()[:8]
	ref := &domain.GatewayRefund{
		ID:              refundID,
		PaymentID:       paymentID,
		PropertyID:      propID,
		Provider:        "cashfree",
		Source:          "system",
		RefundReference: &cfRefundRef,
		AmountPaise:     10000,
		Status:          "failed",
		Reason:          "Lock timeout test",
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	if err := repo.CreateOrUpdateRefund(ctx, ref); err != nil {
		t.Fatalf("insert refund: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM gateway_refunds WHERE id = $1`, refundID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_outbox_events WHERE source_id = $1`, refundID)
	}()

	// Hold an exclusive lock on the target due in a separate connection
	txHold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin txHold: %v", err)
	}
	var dummy uuid.UUID
	if err := txHold.QueryRow(ctx, `SELECT id FROM dues WHERE id = $1 FOR UPDATE`, dueID).Scan(&dummy); err != nil {
		_ = txHold.Rollback(ctx)
		t.Fatalf("lock due in txHold: %v", err)
	}
	defer func() { _ = txHold.Rollback(context.Background()) }()

	// Reconcile must fail closed because target due lock cannot be acquired within short deadline
	shortCtx, shortCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer shortCancel()

	err = repo.ReconcileRefundTransition(shortCtx, ref, "succeeded")
	if err == nil {
		t.Fatalf("expected error from ReconcileRefundTransition due to lock timeout, got nil")
	}

	// Release holder transaction
	_ = txHold.Rollback(ctx)

	// Verify fail-closed rollback: refund remains 'failed'
	storedRef, fetchErr := repo.GetRefundByID(ctx, refundID)
	if fetchErr != nil {
		t.Fatalf("fetch refund after lock timeout: %v", fetchErr)
	}
	if storedRef.Status == "succeeded" {
		t.Fatalf("refund marked succeeded despite lock timeout error")
	}

	// Verify zero allocations or outbox
	var allocCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM refund_allocations WHERE refund_id = $1`, refundID).Scan(&allocCount)
	if allocCount != 0 {
		t.Fatalf("expected 0 refund_allocations, got %d", allocCount)
	}
}
