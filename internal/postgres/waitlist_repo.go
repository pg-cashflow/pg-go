package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrWaitlistDisabled   = errors.New("waitlist: disabled")
	ErrWaitlistRoomOpen   = errors.New("waitlist: room has capacity")
	ErrWaitlistDuplicate  = errors.New("waitlist: already queued")
	ErrWaitlistNotFound   = errors.New("waitlist: not found")
	ErrWaitlistNotWaiting = errors.New("waitlist: not waiting")
)

type WaitlistRepo struct{ db DBTX }

func NewWaitlistRepo(db DBTX) *WaitlistRepo { return &WaitlistRepo{db: db} }

func (r *WaitlistRepo) WithTx(tx pgx.Tx) *WaitlistRepo { return &WaitlistRepo{db: tx} }

const waitlistCols = `id, property_id, room_id, user_id, phone, status, offered_slot_invite_id, offer_expires_at, created_at, updated_at`

func scanWaitlist(row pgx.Row) (*domain.RoomWaitlistEntry, error) {
	var e domain.RoomWaitlistEntry
	var status string
	err := row.Scan(
		&e.ID, &e.PropertyID, &e.RoomID, &e.UserID, &e.Phone, &status,
		&e.OfferedSlotInviteID, &e.OfferExpiresAt, &e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	e.Status = domain.WaitlistStatus(status)
	return &e, nil
}

func (r *WaitlistRepo) Enqueue(ctx context.Context, propertyID, roomID, userID uuid.UUID, phone string) (*domain.RoomWaitlistEntry, error) {
	phone = normalizeDigitsPhone(phone)
	return scanWaitlist(r.db.QueryRow(ctx, `
		INSERT INTO room_waitlist_entries (property_id, room_id, user_id, phone, status)
		VALUES ($1,$2,$3,$4,'waiting')
		RETURNING `+waitlistCols, propertyID, roomID, userID, phone))
}

func (r *WaitlistRepo) ListByRoom(ctx context.Context, propertyID, roomID uuid.UUID) ([]domain.RoomWaitlistEntry, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+waitlistCols+` FROM room_waitlist_entries
		WHERE property_id=$1 AND room_id=$2 AND status IN ('waiting','offered')
		ORDER BY created_at ASC`, propertyID, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RoomWaitlistEntry
	for rows.Next() {
		e, err := scanWaitlist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (r *WaitlistRepo) NextWaitingForUpdate(ctx context.Context, roomID uuid.UUID) (*domain.RoomWaitlistEntry, error) {
	return scanWaitlist(r.db.QueryRow(ctx, `
		SELECT `+waitlistCols+` FROM room_waitlist_entries
		WHERE room_id=$1 AND status='waiting'
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`, roomID))
}

func (r *WaitlistRepo) MarkOffered(ctx context.Context, entryID, inviteID uuid.UUID, expiresAt time.Time) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE room_waitlist_entries
		SET status='offered', offered_slot_invite_id=$2, offer_expires_at=$3, updated_at=NOW()
		WHERE id=$1 AND status='waiting'`, entryID, inviteID, expiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWaitlistNotWaiting
	}
	return nil
}

func (r *WaitlistRepo) RoomFloor(ctx context.Context, roomID uuid.UUID) (propertyID, floorID uuid.UUID, err error) {
	err = r.db.QueryRow(ctx, `SELECT property_id, floor_id FROM rooms WHERE id=$1`, roomID).Scan(&propertyID, &floorID)
	return propertyID, floorID, err
}

func (r *WaitlistRepo) ExpiredOffers(ctx context.Context, now time.Time) ([]domain.RoomWaitlistEntry, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+waitlistCols+` FROM room_waitlist_entries
		WHERE status='offered' AND offer_expires_at IS NOT NULL AND offer_expires_at < $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RoomWaitlistEntry
	for rows.Next() {
		e, err := scanWaitlist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (r *WaitlistRepo) ResetOffer(ctx context.Context, entryID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE room_waitlist_entries
		SET status='waiting', offered_slot_invite_id=NULL, offer_expires_at=NULL, updated_at=NOW()
		WHERE id=$1 AND status='offered'`, entryID)
	return err
}
