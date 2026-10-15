package postgres

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLiveSlotInvite_ConsumeOpenConcurrency(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	if pool == nil {
		return
	}
	ctx := context.Background()
	propID := uuid.New()
	floorID := uuid.New()
	roomID := uuid.New()
	inviteID := uuid.New()
	userA := uuid.New()
	userB := uuid.New()
	tenantA := uuid.New()
	tenantB := uuid.New()
	code := "CC" + strconv.FormatInt(time.Now().UnixNano()%1e8, 10)
	phone := "+91" + strconv.FormatInt(time.Now().UnixNano()%1e10, 10)

	err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SET LOCAL app.ledger_maintenance = 'on'`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
			VALUES ($1, 'Slot Concurrency', '1 Test St', $2, 't@upi', 'Owner', 'o@test.com', $3)`,
			propID, phone, "INV"+uuid.NewString()[:6])
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO floors (id, property_id, floor_number, name) VALUES ($1,$2,1,'F1')`, floorID, propID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO rooms (id, property_id, floor_id, room_number, capacity) VALUES ($1,$2,$3,'101',2)`, roomID, propID, floorID)
		if err != nil {
			return err
		}
		expires := time.Now().UTC().Add(24 * time.Hour)
		_, err = tx.Exec(ctx, `
			INSERT INTO slot_invites (
				id, property_id, floor_id, room_id, code, rent_paise, deposit_paise, due_day,
				status, terms_frozen_at, profile_schema_version, allow_over_capacity, expires_at, created_at
			) VALUES ($1,$2,$3,$4,$5,500000,0,5,'open',NOW(),1,false,$6,NOW())`,
			inviteID, propID, floorID, roomID, code, expires)
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM slot_invites WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM rooms WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM floors WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id=$1`, propID)
	})

	repo := NewSlotInviteRepo(pool)
	var wg sync.WaitGroup
	wg.Add(2)
	results := make([]error, 2)
	go func() {
		defer wg.Done()
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := repo.WithTx(tx).ConsumeOpenForJoin(ctx, inviteID, propID, userA, tenantA, phone)
			return err
		})
		results[0] = err
	}()
	go func() {
		defer wg.Done()
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := repo.WithTx(tx).ConsumeOpenForJoin(ctx, inviteID, propID, userB, tenantB, phone)
			return err
		})
		results[1] = err
	}()
	wg.Wait()

	var wins, loses int
	for _, err := range results {
		if err == nil {
			wins++
		} else if err == ErrSlotInviteNotOpen {
			loses++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || loses != 1 {
		t.Fatalf("expected 1 success and 1 not open, got wins=%d loses=%d errs=%v", wins, loses, results)
	}
	inv, err := repo.GetByID(ctx, inviteID)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status != domain.SlotInviteConsumed {
		t.Fatalf("status %s want consumed", inv.Status)
	}
}
