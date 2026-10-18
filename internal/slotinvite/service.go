package slotinvite

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/join"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

var (
	ErrRoomFull          = postgres.ErrSlotRoomFull
	ErrRoomMasterMissing = postgres.ErrSlotRoomMasterMissing
	ErrNotOpen           = postgres.ErrSlotInviteNotOpen
	ErrCrossProperty     = postgres.ErrSlotCrossProperty
)

type Service struct {
	pool     *pgxpool.Pool
	invites  *postgres.SlotInviteRepo
	settings *postgres.OnboardingSettingsRepo
	now      func() time.Time
}

func NewService(pool *pgxpool.Pool, invites *postgres.SlotInviteRepo, settings *postgres.OnboardingSettingsRepo) *Service {
	return &Service{
		pool:     pool,
		invites:  invites,
		settings: settings,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

type CreateInput struct {
	PropertyID        uuid.UUID
	FloorID           uuid.UUID
	RoomID            uuid.UUID
	RentPaise         int64
	DepositPaise      int64
	DueDay            int16
	AllowOverCapacity bool
	OverCapacityReason string
	CreatedBy         uuid.UUID
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*domain.SlotInvite, error) {
	if in.RentPaise <= 0 || in.DueDay < 1 || in.DueDay > 28 {
		return nil, fmt.Errorf("rent and due_day required")
	}
	settings, err := s.settings.EnsureDefaults(ctx, in.PropertyID)
	if err != nil {
		return nil, err
	}
	code, err := join.GenerateInviteCode()
	if err != nil {
		return nil, err
	}
	ttlDays := settings.SlotInviteDefaultTTLDays
	if ttlDays <= 0 {
		ttlDays = 14
	}
	expires := s.now().Add(time.Duration(ttlDays) * 24 * time.Hour)

	// Dual-mode invariant (see postgres/dbtx.go): CreateOpen performs multiple
	// writes (rooms row lock, slot_invites insert, room_occupancy update) and
	// MUST run inside a transaction. Wrapping here — rather than letting
	// CreateOpen run on the bare pool — is what makes the FOR UPDATE lock in
	// roomSnapshot actually serialize concurrent invite creation for the same
	// room instead of releasing immediately after the read.
	var invite *domain.SlotInvite
	err = postgres.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
		sl := s.invites.WithTx(tx)
		created, err := sl.CreateOpen(ctx, postgres.CreateSlotInviteInput{
			PropertyID:           in.PropertyID,
			FloorID:              in.FloorID,
			RoomID:               in.RoomID,
			Code:                 code,
			RentPaise:            in.RentPaise,
			DepositPaise:         in.DepositPaise,
			DueDay:               in.DueDay,
			ProfileSchemaVersion: settings.ProfileSchemaVersion,
			AllowOverCapacity:    in.AllowOverCapacity,
			ExpiresAt:            expires,
			CreatedByUserID:      in.CreatedBy,
			ReserveBed:           settings.ReserveBedsForOpenInvites,
			MaxExtraBeds:         settings.MaxExtraBedsPerRoom,
		})
		if err != nil {
			return err
		}
		invite = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return invite, nil
}

func (s *Service) List(ctx context.Context, propertyID uuid.UUID, status *domain.SlotInviteStatus) ([]domain.SlotInvite, error) {
	return s.invites.ListByProperty(ctx, propertyID, status)
}

func (s *Service) Revoke(ctx context.Context, propertyID, inviteID, actor uuid.UUID) (*domain.SlotInvite, error) {
	return s.invites.Revoke(ctx, propertyID, inviteID, actor)
}

func (s *Service) ExpireDue(ctx context.Context) (int64, error) {
	return s.invites.ExpireDue(ctx, s.now())
}

func MapCreateError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, postgres.ErrSlotRoomFull):
		return ErrRoomFull
	case errors.Is(err, postgres.ErrSlotRoomMasterMissing):
		return ErrRoomMasterMissing
	default:
		return err
	}
}
