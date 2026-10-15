package referral

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

func setupLivePool(t *testing.T, timeout time.Duration) *pgxpool.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)
	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "postgres connect: %v", err)
		return nil
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func TestLiveReferralFirstRent_DiscountEarnAndReverse(t *testing.T) {
	pool := setupLivePool(t, 90*time.Second)
	if pool == nil {
		return
	}
	ctx := context.Background()
	propID := uuid.New()
	floorID := uuid.New()
	roomID := uuid.New()
	referrerUser := uuid.New()
	referrerTenant := uuid.New()
	joinerUser := uuid.New()
	joinerTenant := uuid.New()
	dueID := uuid.New()
	paymentID := uuid.New()
	phoneBase := time.Now().UnixNano() % 1e9
	referrerPhone := "+91" + strconv.FormatInt(phoneBase, 10)
	joinerPhone := "+91" + strconv.FormatInt(phoneBase+1, 10)

	err := postgres.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SET LOCAL app.ledger_maintenance = 'on'`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
			VALUES ($1, 'Ref Journey', '1 Test St', $2, 't@upi', 'Owner', 'o@test.com', $3)`,
			propID, referrerPhone, "INV"+uuid.NewString()[:6])
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO floors (id, property_id, floor_number, name) VALUES ($1,$2,1,'F1')`, floorID, propID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO rooms (id, property_id, floor_id, room_number, capacity) VALUES ($1,$2,$3,'101',4)`,
			roomID, propID, floorID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO property_onboarding_settings (property_id, referral_enabled, joiner_discount_bps, referrer_reward_bps)
			VALUES ($1, TRUE, 1000, 1000)`, propID)
		if err != nil {
			return err
		}
		for _, spec := range []struct {
			uid, tid uuid.UUID
			phone    string
		}{
			{referrerUser, referrerTenant, referrerPhone},
			{joinerUser, joinerTenant, joinerPhone},
		} {
			if _, err = tx.Exec(ctx, `
				INSERT INTO tenants (id, property_id, room_id, name, phone, status, rent_amount, due_day, credit_balance_paise)
				VALUES ($1,$2,$3,'T',$4,'active',100000,5,0)`,
				spec.tid, propID, roomID, spec.phone); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO users (id, phone, role, property_id, tenant_id) VALUES ($1,$2,'tenant',$3,$4)`,
				spec.uid, spec.phone, propID, spec.tid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM referral_credit_entries WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM due_referral_adjustments WHERE due_id=$1`, dueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM referrals WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM payment_allocations WHERE payment_id=$1`, paymentID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM payments WHERE due_id=$1`, dueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM dues WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM property_onboarding_settings WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM rooms WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM floors WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id=$1`, propID)
	})

	svc := NewService(postgres.NewReferralRepo(pool))
	ref, err := svc.CreateForTenant(ctx, propID, referrerTenant, joinerPhone, "Joiner", domain.ReferralDestReferrerCredit)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BindAtJoin(ctx, propID, joinerTenant, joinerPhone, ref.ReferralCode); err != nil {
		t.Fatal(err)
	}

	const rentBase = int64(100_000)
	due := &domain.Due{Kind: domain.DueKindRent, OriginalAmount: rentBase, Amount: rentBase}
	joiner := &domain.Tenant{ID: joinerTenant}
	if err := svc.ApplyFirstRentDiscount(ctx, joiner, due); err != nil {
		t.Fatal(err)
	}
	if due.Amount != 90_000 {
		t.Fatalf("discounted amount %d want 90000", due.Amount)
	}

	now := time.Now().UTC()
	dueCode := "D" + uuid.NewString()[:7]
	_, err = pool.Exec(ctx, `
		INSERT INTO dues (id, property_id, tenant_id, due_code, kind, status, amount, original_amount, period_start, period_end, due_date)
		VALUES ($1,$2,$3,$7,'rent','open',$4,$5,$6,$6,$6)`,
		dueID, propID, joinerTenant, due.Amount, rentBase, now, dueCode)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO payments (id, property_id, due_id, tenant_id, amount, matched_by, provider)
		VALUES ($1, $2, $3, $4, $5, 'cash', 'offline')`,
		paymentID, propID, dueID, joinerTenant, due.Amount)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordFirstRentAdjustment(ctx, joinerTenant, dueID, rentBase); err != nil {
		t.Fatal(err)
	}
	if err := svc.SettleFirstRentPayment(ctx, dueID); err != nil {
		t.Fatal(err)
	}

	var credit int64
	if err := pool.QueryRow(ctx, `SELECT credit_balance_paise FROM tenants WHERE id=$1`, referrerTenant).Scan(&credit); err != nil {
		t.Fatal(err)
	}
	if credit != 10_000 {
		t.Fatalf("referrer credit %d want 10000", credit)
	}

	if err := svc.ReverseFirstRentPayment(ctx, dueID, paymentID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT credit_balance_paise FROM tenants WHERE id=$1`, referrerTenant).Scan(&credit); err != nil {
		t.Fatal(err)
	}
	if credit != 0 {
		t.Fatalf("credit after reversal %d want 0", credit)
	}
}
