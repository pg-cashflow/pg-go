package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// A payment with no due that is not unapplied must not write a ledger mirror at
// create time: it would carry a debit with no credit (outbox "journal lines do not
// balance"). The insert must be refused, and no payment or outbox row may remain.
func TestLivePaymentCreate_RejectsEagerMirrorWithoutDue(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	propID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Mirror Guard', 'x', $2, 'g@upi', 'G', 'g@guard.test', $3)`,
		propID, "+919"+uuid.New().String()[:9], uuid.New().String()[:8]); err != nil {
		t.Fatalf("seed property: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id=$1`, propID) })

	tenantID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Guard Tenant', $3, '1', 100000, 5, 'active')`,
		tenantID, propID, "+918"+uuid.New().String()[:9]); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	repo := NewPaymentRepo(pool)
	cfID := "cf_guard_" + uuid.New().String()[:8]
	p := &domain.Payment{
		ID:          uuid.New(),
		DueID:       uuid.Nil,
		TenantID:    tenantID,
		PropertyID:  &propID,
		Amount:      1100000,
		MatchedBy:   domain.MatchedByCashfree,
		CFPaymentID: &cfID,
		CreatedAt:   time.Now(),
	}
	err := repo.Create(ctx, p)
	if err == nil {
		t.Fatal("create without due, unapplied, or skip must be refused")
	}
	if !strings.Contains(err.Error(), "SkipOutboxEnqueue") {
		t.Fatalf("refused for the wrong reason (want the guard, got): %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM ledger_outbox_events WHERE property_id=$1`, propID).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("refused create left %d outbox rows", n)
	}
}
