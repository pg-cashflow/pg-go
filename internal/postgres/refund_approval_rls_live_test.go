package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// TestLiveRefundApprovalRequestsRLS verifies that migration 122 forces tenant
// isolation for pgapp_app while trusted maintenance access remains available.
func TestLiveRefundApprovalRequestsRLS(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 45*time.Second)
	if pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	propA, propB := uuid.New(), uuid.New()
	tenantA, tenantB := uuid.New(), uuid.New()
	dueA, dueB := uuid.New(), uuid.New()
	paymentA, paymentB := uuid.New(), uuid.New()
	userA, userB := uuid.New(), uuid.New()
	requestA, requestB := uuid.New(), uuid.New()
	phoneA := "+91" + uuid.New().String()[:10]
	phoneB := "+91" + uuid.New().String()[:10]

	// Seed the minimum valid relationship chain using the test database owner.
	if _, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES
		($1, 'Refund RLS Alpha', 'Alpha', $3, 'alpha@upi', 'Alpha Owner', 'alpha@rls.test', $5),
		($2, 'Refund RLS Beta', 'Beta', $4, 'beta@upi', 'Beta Owner', 'beta@rls.test', $6)`,
		propA, propB, phoneA, phoneB, "ra"+uuid.New().String()[:8], "rb"+uuid.New().String()[:8],
	); err != nil {
		t.Fatalf("seed properties: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM refund_approval_requests WHERE id = ANY($1::uuid[])`, []uuid.UUID{requestA, requestB})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM payments WHERE id = ANY($1::uuid[])`, []uuid.UUID{paymentA, paymentB})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM dues WHERE id = ANY($1::uuid[])`, []uuid.UUID{dueA, dueB})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = ANY($1::uuid[])`, []uuid.UUID{userA, userB})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM tenants WHERE id = ANY($1::uuid[])`, []uuid.UUID{tenantA, tenantB})
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM properties WHERE id = ANY($1::uuid[])`, []uuid.UUID{propA, propB})
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, room_number, rent_amount, due_day, status)
		VALUES ($1, $2, 'Tenant Alpha', $3, 'A1', 100000, 5, 'active'),
		       ($4, $5, 'Tenant Beta', $6, 'B1', 100000, 5, 'active')`,
		tenantA, propA, phoneA, tenantB, propB, phoneB,
	); err != nil {
		t.Fatalf("seed tenants: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
		VALUES ($1, $3, $5, $7, 'rent', 100000, 100000, DATE '2026-11-01', DATE '2026-11-30', DATE '2026-11-05', 'pending'),
		       ($2, $4, $6, $8, 'rent', 100000, 100000, DATE '2026-11-01', DATE '2026-11-30', DATE '2026-11-05', 'pending')`,
		dueA, dueB, "RA"+uuid.New().String()[:6], "RB"+uuid.New().String()[:6], tenantA, tenantB, propA, propB,
	); err != nil {
		t.Fatalf("seed dues: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, tenant_id, property_id)
		VALUES ($1, $3, 'owner', NULL, $5), ($2, $4, 'owner', NULL, $6)`,
		userA, userB, "+917"+uuid.New().String()[:9], "+918"+uuid.New().String()[:9], propA, propB,
	); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payments (id, due_id, tenant_id, property_id, amount, matched_by, provider)
		VALUES ($1, $3, $5, $7, 100000, 'manual', 'legacy_unverified'), ($2, $4, $6, $8, 100000, 'manual', 'legacy_unverified')`,
		paymentA, paymentB, dueA, dueB, tenantA, tenantB, propA, propB,
	); err != nil {
		t.Fatalf("seed payments: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO refund_approval_requests (
			id, property_id, payment_id, tenant_id, amount_paise, reason, idempotency_key,
			requested_by, requested_by_role, status, required_approvals, material_hash
		) VALUES
		($1, $3, $5, $7, 10000, 'test refund', $9, $11, 'owner', 'pending', 1, 'hash-a'),
		($2, $4, $6, $8, 10000, 'test refund', $10, $12, 'owner', 'pending', 1, 'hash-b')`,
		requestA, requestB, propA, propB, paymentA, paymentB, tenantA, tenantB,
		"idemp-a-"+uuid.NewString(), "idemp-b-"+uuid.NewString(), userA, userB,
	); err != nil {
		t.Fatalf("seed refund approval requests: %v", err)
	}

	var rlsEnabled, rlsForced bool
	if err := pool.QueryRow(ctx, `
		SELECT c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c WHERE c.oid = 'refund_approval_requests'::regclass`).Scan(&rlsEnabled, &rlsForced); err != nil {
		t.Fatalf("read RLS catalog state: %v", err)
	}
	if !rlsEnabled || !rlsForced {
		t.Fatalf("refund_approval_requests RLS must be ENABLED and FORCED (enabled=%v forced=%v)", rlsEnabled, rlsForced)
	}

	t.Run("app role is isolated to its property", func(t *testing.T) {
		ctxA := requestscope.WithPropertyID(ctx, propA)
		if err := WithinTx(ctxA, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctxA, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			var count int
			if err := tx.QueryRow(ctxA, `SELECT COUNT(*) FROM refund_approval_requests WHERE id = ANY($1::uuid[])`, []uuid.UUID{requestA, requestB}).Scan(&count); err != nil {
				return err
			}
			if count != 1 {
				t.Fatalf("property A app role sees %d rows, want exactly 1", count)
			}
			return nil
		}); err != nil {
			t.Fatalf("scoped app query: %v", err)
		}

		repo := NewRefundApprovalRepo(pool)
		if _, err := repo.GetRequestByID(ctxA, requestB); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("cross-property ID read = %v, want ErrNotFound", err)
		}
		if _, err := repo.ListRequestsByProperty(ctxA, propB, ""); !errors.Is(err, ErrRefundApprovalScopeMismatch) {
			t.Fatalf("cross-property list = %v, want scope mismatch", err)
		}
	})

	t.Run("unscoped app role sees no rows", func(t *testing.T) {
		if err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_app"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', '', true)"); err != nil {
				return err
			}
			var count int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM refund_approval_requests`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("unscoped app role sees %d rows, want 0", count)
			}
			return nil
		}); err != nil {
			t.Fatalf("unscoped app query: %v", err)
		}
	})

	t.Run("maintenance role can inspect both properties", func(t *testing.T) {
		if err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_maint"); err != nil {
				return err
			}
			var count int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM refund_approval_requests WHERE id = ANY($1::uuid[])`, []uuid.UUID{requestA, requestB}).Scan(&count); err != nil {
				return err
			}
			if count != 2 {
				t.Fatalf("maintenance role sees %d rows, want 2", count)
			}
			return nil
		}); err != nil {
			t.Fatalf("maintenance query: %v", err)
		}
	})

	t.Run("worker role can inspect both properties", func(t *testing.T) {
		if err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE pgapp_worker"); err != nil {
				return err
			}
			var count int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM refund_approval_requests WHERE id = ANY($1::uuid[])`, []uuid.UUID{requestA, requestB}).Scan(&count); err != nil {
				return err
			}
			if count != 2 {
				t.Fatalf("worker role sees %d rows, want 2", count)
			}
			return nil
		}); err != nil {
			t.Fatalf("worker query: %v", err)
		}
	})

	t.Run("repository requires an authenticated property scope", func(t *testing.T) {
		repo := NewRefundApprovalRepo(pool)
		if _, err := repo.GetRequestByID(ctx, requestA); !errors.Is(err, ErrRefundApprovalScopeRequired) {
			t.Fatalf("unscoped repository read = %v, want ErrRefundApprovalScopeRequired", err)
		}
	})
}
