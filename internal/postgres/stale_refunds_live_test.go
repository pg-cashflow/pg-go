package postgres

import (
	"context"
	"testing"
	"time"
)

// The Cashfree poll job runs ListStaleNonTerminalRefunds on a schedule. Postgres
// checks column names when the statement is prepared, so running the real query
// against the migrated schema catches a wrong column even when no rows match.
func TestLiveListStaleNonTerminalRefunds_QueryMatchesSchema(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 30*time.Second)
	if pool == nil {
		return
	}
	repo := NewPaymentRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := repo.ListStaleNonTerminalRefunds(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("stale refund query does not match gateway_refunds schema: %v", err)
	}
}
