package postgres

import (
	"context"

	"github.com/google/uuid"
)

// PropertyAdmissionOccupancy aggregates admission counters for owner dashboards.
type PropertyAdmissionOccupancy struct {
	ReservedBeds        int
	OverCapacityBeds    int
	AvailableNormalBeds int
	OccupancyRevision   int64
}

// PropertyAdmissionSnapshot reads room_occupancy sums and occupancy_revision for a property.
func PropertyAdmissionSnapshot(ctx context.Context, db DBTX, propertyID uuid.UUID) (PropertyAdmissionOccupancy, error) {
	var out PropertyAdmissionOccupancy
	err := db.QueryRow(ctx, `
		SELECT COALESCE(SUM(open_invite_reserved),0)::int, COALESCE(SUM(over_capacity_allocated),0)::int
		FROM room_occupancy WHERE property_id=$1`, propertyID,
	).Scan(&out.ReservedBeds, &out.OverCapacityBeds)
	if err != nil {
		return out, err
	}
	err = db.QueryRow(ctx, `SELECT occupancy_revision FROM properties WHERE id=$1`, propertyID).Scan(&out.OccupancyRevision)
	if err != nil {
		return out, err
	}
	rows, err := db.Query(ctx, `
		SELECT r.id, r.capacity,
			(SELECT COUNT(*)::int FROM tenants t WHERE t.room_id=r.id AND t.status IN ('active','pending_allocation')),
			COALESCE(ro.open_invite_reserved,0)
		FROM rooms r
		LEFT JOIN room_occupancy ro ON ro.room_id=r.id
		WHERE r.property_id=$1`, propertyID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var roomID uuid.UUID
		var cap, tenants, reserved int
		if err := rows.Scan(&roomID, &cap, &tenants, &reserved); err != nil {
			return out, err
		}
		avail := cap - tenants - reserved
		if avail > 0 {
			out.AvailableNormalBeds += avail
		}
	}
	return out, rows.Err()
}
