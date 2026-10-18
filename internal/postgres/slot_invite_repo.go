package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type SlotInviteRepo struct{ db DBTX }

func NewSlotInviteRepo(db DBTX) *SlotInviteRepo { return &SlotInviteRepo{db: db} }

func (r *SlotInviteRepo) WithTx(tx pgx.Tx) *SlotInviteRepo { return &SlotInviteRepo{db: tx} }

const slotInviteCols = `id, property_id, floor_id, room_id, code, rent_paise, deposit_paise, due_day,
	status, terms_frozen_at, profile_schema_version, allow_over_capacity, bound_phone, waitlist_entry_id, expires_at,
	consumed_at, consumed_by_user_id, consumed_tenant_id, created_by_user_id, created_at`

func scanSlotInvite(row pgx.Row) (*domain.SlotInvite, error) {
	var s domain.SlotInvite
	var status string
	var boundPhone *string
	err := row.Scan(
		&s.ID, &s.PropertyID, &s.FloorID, &s.RoomID, &s.Code, &s.RentPaise, &s.DepositPaise, &s.DueDay,
		&status, &s.TermsFrozenAt, &s.ProfileSchemaVersion, &s.AllowOverCapacity, &boundPhone, &s.WaitlistEntryID, &s.ExpiresAt,
		&s.ConsumedAt, &s.ConsumedByUserID, &s.ConsumedTenantID, &s.CreatedByUserID, &s.CreatedAt,
	)
	if boundPhone != nil {
		s.BoundPhone = *boundPhone
	}
	if err != nil {
		return nil, err
	}
	s.Status = domain.SlotInviteStatus(status)
	return &s, nil
}

func (r *SlotInviteRepo) GetByCode(ctx context.Context, code string) (*domain.SlotInvite, error) {
	return scanSlotInvite(r.db.QueryRow(ctx, `SELECT `+slotInviteCols+` FROM slot_invites WHERE code=$1`, code))
}

func (r *SlotInviteRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.SlotInvite, error) {
	return scanSlotInvite(r.db.QueryRow(ctx, `SELECT `+slotInviteCols+` FROM slot_invites WHERE id=$1`, id))
}

func (r *SlotInviteRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID, status *domain.SlotInviteStatus) ([]domain.SlotInvite, error) {
	q := `SELECT ` + slotInviteCols + ` FROM slot_invites WHERE property_id=$1`
	args := []any{propertyID}
	if status != nil {
		q += ` AND status=$2`
		args = append(args, string(*status))
	}
	q += ` ORDER BY created_at DESC`
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SlotInvite
	for rows.Next() {
		s, err := scanSlotInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func (r *SlotInviteRepo) RoomNumber(ctx context.Context, roomID uuid.UUID) (string, error) {
	var n string
	err := r.db.QueryRow(ctx, `SELECT room_number FROM rooms WHERE id=$1`, roomID).Scan(&n)
	return n, err
}

func (r *SlotInviteRepo) CountRooms(ctx context.Context, propertyID uuid.UUID) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM rooms WHERE property_id=$1`, propertyID).Scan(&n)
	return n, err
}

func (r *SlotInviteRepo) bumpOccupancyRevision(ctx context.Context, propertyID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE properties SET occupancy_revision = occupancy_revision + 1 WHERE id=$1`, propertyID)
	return err
}

type RoomBedSnapshot struct {
	Capacity              int
	TenantBeds            int
	OpenInviteReserved    int
	OverCapacityAllocated int
}

// AvailableNormalBeds returns physical capacity minus tenants and open-invite reservations.
func (r *SlotInviteRepo) AvailableNormalBeds(ctx context.Context, roomID uuid.UUID) (int, error) {
	snap, _, err := r.roomSnapshot(ctx, roomID)
	if err != nil {
		return 0, err
	}
	return snap.Capacity - snap.TenantBeds - snap.OpenInviteReserved, nil
}

func (r *SlotInviteRepo) roomSnapshot(ctx context.Context, roomID uuid.UUID) (RoomBedSnapshot, uuid.UUID, error) {
	var cap int
	var propertyID uuid.UUID
	// FOR UPDATE: serializes concurrent capacity checks for this room. Any second
	// CreateOpen/OfferNext call for the same room blocks here until the first
	// transaction commits or rolls back, so both always see fresh occupancy —
	// this is what actually prevents overbooking, not the Go-level comparison below.
	// Caller MUST run this inside a transaction (see CreateOpen/AvailableNormalBeds
	// callers) or the lock is released immediately and provides no protection.
	err := r.db.QueryRow(ctx, `SELECT capacity, property_id FROM rooms WHERE id=$1 FOR UPDATE`, roomID).Scan(&cap, &propertyID)
	if err != nil {
		return RoomBedSnapshot{}, uuid.Nil, err
	}
	var tenantBeds int
	err = r.db.QueryRow(ctx, `
		SELECT COUNT(*)::int FROM tenants
		WHERE room_id=$1 AND status IN ('active','pending_allocation')`, roomID).Scan(&tenantBeds)
	if err != nil {
		return RoomBedSnapshot{}, uuid.Nil, err
	}
	var reserved, overCap int
	err = r.db.QueryRow(ctx, `
		INSERT INTO room_occupancy (room_id, property_id, open_invite_reserved, over_capacity_allocated, updated_at)
		VALUES ($1, $2, 0, 0, NOW())
		ON CONFLICT (room_id) DO UPDATE SET updated_at = room_occupancy.updated_at
		RETURNING open_invite_reserved, over_capacity_allocated`, roomID, propertyID,
	).Scan(&reserved, &overCap)
	if err != nil {
		return RoomBedSnapshot{}, uuid.Nil, err
	}
	return RoomBedSnapshot{
		Capacity:              cap,
		TenantBeds:            tenantBeds,
		OpenInviteReserved:    reserved,
		OverCapacityAllocated: overCap,
	}, propertyID, nil
}

var (
	ErrSlotRoomFull          = errors.New("slot: room full")
	ErrSlotRoomMasterMissing = errors.New("slot: room master missing")
	ErrSlotInviteNotOpen     = errors.New("slot: invite not open")
	ErrSlotInviteExpired     = errors.New("slot: invite expired")
	ErrSlotCrossProperty     = errors.New("slot: invite property mismatch")
	ErrSlotBoundPhone        = errors.New("slot: bound phone mismatch")
)

type CreateSlotInviteInput struct {
	PropertyID           uuid.UUID
	FloorID              uuid.UUID
	RoomID               uuid.UUID
	Code                 string
	RentPaise            int64
	DepositPaise         int64
	DueDay               int16
	ProfileSchemaVersion int
	AllowOverCapacity    bool
	ExpiresAt            time.Time
	CreatedByUserID      uuid.UUID
	ReserveBed           bool
	MaxExtraBeds         int
	BoundPhone           string
	WaitlistEntryID      *uuid.UUID
}

func (r *SlotInviteRepo) CreateOpen(ctx context.Context, in CreateSlotInviteInput) (*domain.SlotInvite, error) {
	n, err := r.CountRooms(ctx, in.PropertyID)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrSlotRoomMasterMissing
	}
	var roomProperty uuid.UUID
	err = r.db.QueryRow(ctx, `SELECT property_id FROM rooms WHERE id=$1 AND floor_id=$2`, in.RoomID, in.FloorID).Scan(&roomProperty)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("room not found")
		}
		return nil, err
	}
	if roomProperty != in.PropertyID {
		return nil, ErrSlotCrossProperty
	}

	snap, _, err := r.roomSnapshot(ctx, in.RoomID)
	if err != nil {
		return nil, err
	}
	used := snap.TenantBeds + snap.OpenInviteReserved
	available := snap.Capacity - used
	if available <= 0 && !in.AllowOverCapacity {
		return nil, ErrSlotRoomFull
	}
	if in.AllowOverCapacity && snap.OverCapacityAllocated >= in.MaxExtraBeds {
		return nil, ErrSlotRoomFull
	}

	now := time.Now().UTC()
	var boundPhone *string
	if strings.TrimSpace(in.BoundPhone) != "" {
		boundPhone = &in.BoundPhone
	}
	s, err := scanSlotInvite(r.db.QueryRow(ctx, `
		INSERT INTO slot_invites (
			property_id, floor_id, room_id, code, rent_paise, deposit_paise, due_day,
			status, terms_frozen_at, profile_schema_version, allow_over_capacity, bound_phone, waitlist_entry_id,
			expires_at, created_by_user_id, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,'open',$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING `+slotInviteCols,
		in.PropertyID, in.FloorID, in.RoomID, in.Code, in.RentPaise, in.DepositPaise, in.DueDay,
		now, in.ProfileSchemaVersion, in.AllowOverCapacity, boundPhone, in.WaitlistEntryID, in.ExpiresAt, in.CreatedByUserID, now,
	))
	if err != nil {
		return nil, err
	}
	if in.ReserveBed {
		_, err = r.db.Exec(ctx, `
			UPDATE room_occupancy SET open_invite_reserved = open_invite_reserved + 1, updated_at=NOW()
			WHERE room_id=$1`, in.RoomID)
		if err != nil {
			return nil, err
		}
	}
	if in.AllowOverCapacity {
		_, err = r.db.Exec(ctx, `
			UPDATE room_occupancy SET over_capacity_allocated = over_capacity_allocated + 1, updated_at=NOW()
			WHERE room_id=$1`, in.RoomID)
		if err != nil {
			return nil, err
		}
	}
	if err := r.bumpOccupancyRevision(ctx, in.PropertyID); err != nil {
		return nil, err
	}
	if in.AllowOverCapacity {
		_, _ = r.db.Exec(ctx, `
			INSERT INTO slot_invite_audit (property_id, slot_invite_id, actor_user_id, action, reason)
			VALUES ($1,$2,$3,'over_capacity_create','')`, in.PropertyID, s.ID, in.CreatedByUserID)
	}
	return s, nil
}

func (r *SlotInviteRepo) Revoke(ctx context.Context, propertyID, inviteID uuid.UUID, actor uuid.UUID) (*domain.SlotInvite, error) {
	s, err := r.GetByID(ctx, inviteID)
	if err != nil {
		return nil, err
	}
	if s.PropertyID != propertyID {
		return nil, ErrSlotCrossProperty
	}
	if s.Status != domain.SlotInviteOpen {
		return nil, ErrSlotInviteNotOpen
	}
	_, err = r.db.Exec(ctx, `UPDATE slot_invites SET status='revoked' WHERE id=$1 AND status='open'`, inviteID)
	if err != nil {
		return nil, err
	}
	if err := r.releaseReservation(ctx, s); err != nil {
		return nil, err
	}
	if err := r.bumpOccupancyRevision(ctx, propertyID); err != nil {
		return nil, err
	}
	s.Status = domain.SlotInviteRevoked
	_, _ = r.db.Exec(ctx, `
		INSERT INTO slot_invite_audit (property_id, slot_invite_id, actor_user_id, action)
		VALUES ($1,$2,$3,'revoke')`, propertyID, inviteID, actor)
	return s, nil
}

func (r *SlotInviteRepo) releaseReservation(ctx context.Context, s *domain.SlotInvite) error {
	settings, err := NewOnboardingSettingsRepo(r.db).Get(ctx, s.PropertyID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	reserve := settings == nil || settings.ReserveBedsForOpenInvites
	if reserve {
		_, err = r.db.Exec(ctx, `
			UPDATE room_occupancy SET open_invite_reserved = GREATEST(0, open_invite_reserved - 1), updated_at=NOW()
			WHERE room_id=$1`, s.RoomID)
		if err != nil {
			return err
		}
	}
	if s.AllowOverCapacity {
		_, err = r.db.Exec(ctx, `
			UPDATE room_occupancy SET over_capacity_allocated = GREATEST(0, over_capacity_allocated - 1), updated_at=NOW()
			WHERE room_id=$1`, s.RoomID)
		if err != nil {
			return err
		}
	}
	return nil
}

// ConsumeOpenForJoin marks an open invite consumed after onboarding. Caller must run inside a transaction.
func (r *SlotInviteRepo) ConsumeOpenForJoin(ctx context.Context, inviteID, propertyID, userID, tenantID uuid.UUID, joinerPhone string) (*domain.SlotInvite, error) {
	row := r.db.QueryRow(ctx, `
		SELECT `+slotInviteCols+` FROM slot_invites WHERE id=$1 FOR UPDATE`, inviteID)
	s, err := scanSlotInvite(row)
	if err != nil {
		return nil, err
	}
	if s.PropertyID != propertyID {
		return nil, ErrSlotCrossProperty
	}
	if s.Status != domain.SlotInviteOpen {
		return nil, ErrSlotInviteNotOpen
	}
	now := time.Now().UTC()
	if !s.ExpiresAt.After(now) {
		return nil, ErrSlotInviteExpired
	}
	if strings.TrimSpace(s.BoundPhone) != "" && !phonesMatch(joinerPhone, s.BoundPhone) {
		return nil, ErrSlotBoundPhone
	}
	_, err = r.db.Exec(ctx, `
		UPDATE slot_invites SET status='consumed', consumed_at=$2, consumed_by_user_id=$3, consumed_tenant_id=$4
		WHERE id=$1`, inviteID, now, userID, tenantID)
	if err != nil {
		return nil, err
	}
	if err := r.releaseReservation(ctx, s); err != nil {
		return nil, err
	}
	if err := r.bumpOccupancyRevision(ctx, propertyID); err != nil {
		return nil, err
	}
	s.Status = domain.SlotInviteConsumed
	s.ConsumedAt = &now
	s.ConsumedByUserID = &userID
	s.ConsumedTenantID = &tenantID
	if s.WaitlistEntryID != nil {
		_, _ = r.db.Exec(ctx, `
			UPDATE room_waitlist_entries SET status='accepted', updated_at=NOW()
			WHERE id=$1 AND status='offered'`, *s.WaitlistEntryID)
	}
	return s, nil
}

func (r *SlotInviteRepo) ExpireDue(ctx context.Context, now time.Time) (int64, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+slotInviteCols+` FROM slot_invites
		WHERE status='open' AND expires_at < $1`, now)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var expired int64
	for rows.Next() {
		s, err := scanSlotInvite(rows)
		if err != nil {
			return expired, err
		}
		tag, err := r.db.Exec(ctx, `UPDATE slot_invites SET status='expired' WHERE id=$1 AND status='open'`, s.ID)
		if err != nil {
			return expired, err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if err := r.releaseReservation(ctx, s); err != nil {
			return expired, err
		}
		if err := r.bumpOccupancyRevision(ctx, s.PropertyID); err != nil {
			return expired, err
		}
		expired++
	}
	return expired, rows.Err()
}
