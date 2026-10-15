package postgres

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Karpathy baseline: API helper sums room_occupancy + tenant beds (capacity 2, one tenant, one reserved invite → 0 available).
func TestLivePropertyAdmissionSnapshot_AvailableNormalBeds(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 60*time.Second)
	if pool == nil {
		return
	}
	ctx := context.Background()
	propID := uuid.New()
	floorID := uuid.New()
	roomID := uuid.New()
	tenantID := uuid.New()
	phone := "+91" + strconv.FormatInt(time.Now().UnixNano()%1e10, 10)

	err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SET LOCAL app.ledger_maintenance = 'on'`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code, occupancy_revision)
			VALUES ($1, 'Occ Read', '1 Test St', $2, 't@upi', 'Owner', 'o@test.com', $3, 3)`,
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
		_, err = tx.Exec(ctx, `
			INSERT INTO tenants (id, property_id, room_id, name, phone, status, rent_amount, due_day, credit_balance_paise)
			VALUES ($1,$2,$3,'T1',$4,'active',500000,5,0)`, tenantID, propID, roomID, phone)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO room_occupancy (property_id, room_id, open_invite_reserved, over_capacity_allocated)
			VALUES ($1,$2,1,0)`, propID, roomID)
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM room_occupancy WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM rooms WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM floors WHERE property_id=$1`, propID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id=$1`, propID)
	})

	snap, err := PropertyAdmissionSnapshot(ctx, pool, propID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ReservedBeds != 1 {
		t.Fatalf("reserved beds %d want 1", snap.ReservedBeds)
	}
	if snap.AvailableNormalBeds != 0 {
		t.Fatalf("available_normal_beds %d want 0 (cap 2 - 1 tenant - 1 reserved)", snap.AvailableNormalBeds)
	}
	if snap.OccupancyRevision != 3 {
		t.Fatalf("occupancy_revision %d want 3", snap.OccupancyRevision)
	}
}
