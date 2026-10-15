package waitlist

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
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

func TestLiveWaitlistJoin_RoomOpenRejected(t *testing.T) {
	pool, propID, roomID, userID, phone, cleanup := seedWaitlistRoom(t, 2, 0, true)
	if pool == nil {
		return
	}
	defer cleanup()
	svc := NewService(pool, postgres.NewWaitlistRepo(pool), postgres.NewSlotInviteRepo(pool), postgres.NewOnboardingSettingsRepo(pool))
	_, err := svc.Join(context.Background(), propID, roomID, userID, phone)
	if !errors.Is(err, postgres.ErrWaitlistRoomOpen) {
		t.Fatalf("want room open, got %v", err)
	}
}

func TestLiveWaitlistJoin_EnqueueWhenFull(t *testing.T) {
	pool, propID, roomID, userID, phone, cleanup := seedWaitlistRoom(t, 1, 1, true)
	if pool == nil {
		return
	}
	defer cleanup()
	svc := NewService(pool, postgres.NewWaitlistRepo(pool), postgres.NewSlotInviteRepo(pool), postgres.NewOnboardingSettingsRepo(pool))
	entry, err := svc.Join(context.Background(), propID, roomID, userID, phone)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || entry.RoomID != roomID {
		t.Fatalf("unexpected entry %#v", entry)
	}
	_, err = svc.Join(context.Background(), propID, roomID, userID, phone)
	if !errors.Is(err, postgres.ErrWaitlistDuplicate) {
		t.Fatalf("want duplicate, got %v", err)
	}
}

func seedWaitlistRoom(t *testing.T, capacity int, activeTenants int, waitlistOn bool) (*pgxpool.Pool, uuid.UUID, uuid.UUID, uuid.UUID, string, func()) {
	pool := setupLivePool(t, 60*time.Second)
	if pool == nil {
		return nil, uuid.Nil, uuid.Nil, uuid.Nil, "", func() {}
	}
	ctx := context.Background()
	propID := uuid.New()
	floorID := uuid.New()
	roomID := uuid.New()
	userID := uuid.New()
	phone := "+91" + strconv.FormatInt(time.Now().UnixNano()%1e10, 10)

	err := postgres.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SET LOCAL app.ledger_maintenance = 'on'`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
			VALUES ($1, 'WL', '1 Test St', $2, 't@upi', 'Owner', 'o@test.com', $3)`,
			propID, phone, "INV"+uuid.NewString()[:6])
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO floors (id, property_id, floor_number, name) VALUES ($1,$2,1,'F1')`, floorID, propID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO rooms (id, property_id, floor_id, room_number, capacity) VALUES ($1,$2,$3,'101',$4)`,
			roomID, propID, floorID, capacity)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO property_onboarding_settings (property_id, waitlist_enabled)
			VALUES ($1,$2)`, propID, waitlistOn)
		if err != nil {
			return err
		}
		for i := 0; i < activeTenants; i++ {
			uid := uuid.New()
			tid := uuid.New()
			p := "+91" + strconv.FormatInt(time.Now().UnixNano()%1e9+int64(i), 10)
			if _, err = tx.Exec(ctx, `
				INSERT INTO tenants (id, property_id, room_id, name, phone, status, rent_amount, due_day, credit_balance_paise)
				VALUES ($1,$2,$3,'T',$4,'active',500000,5,0)`, tid, propID, roomID, p); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO users (id, phone, role, property_id) VALUES ($1,$2,'tenant',$3)`, userID, phone, propID)
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM room_waitlist_entries WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM property_onboarding_settings WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM rooms WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM floors WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id=$1`, propID)
	}
	return pool, propID, roomID, userID, phone, cleanup
}
