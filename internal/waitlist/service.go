package waitlist

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// ErrRoomNotFound is returned when room_id does not exist on the property scope.
var ErrRoomNotFound = errors.New("waitlist: room not found")

type Service struct {
	pool     *pgxpool.Pool
	waitlist *postgres.WaitlistRepo
	slots    *postgres.SlotInviteRepo
	settings *postgres.OnboardingSettingsRepo
	now      func() time.Time
}

func NewService(pool *pgxpool.Pool, waitlist *postgres.WaitlistRepo, slots *postgres.SlotInviteRepo, settings *postgres.OnboardingSettingsRepo) *Service {
	return &Service{
		pool:     pool,
		waitlist: waitlist,
		slots:    slots,
		settings: settings,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) Join(ctx context.Context, propertyID, roomID, userID uuid.UUID, phone string) (*domain.RoomWaitlistEntry, error) {
	settings, err := s.settings.EnsureDefaults(ctx, propertyID)
	if err != nil {
		return nil, err
	}
	if !settings.WaitlistEnabled {
		return nil, postgres.ErrWaitlistDisabled
	}
	roomProperty, _, err := s.waitlist.RoomFloor(ctx, roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoomNotFound
		}
		return nil, err
	}
	if roomProperty != propertyID {
		return nil, postgres.ErrSlotCrossProperty
	}
	avail, err := s.slots.AvailableNormalBeds(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if avail > 0 {
		return nil, postgres.ErrWaitlistRoomOpen
	}
	entry, err := s.waitlist.Enqueue(ctx, propertyID, roomID, userID, phone)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, postgres.ErrWaitlistDuplicate
		}
		return nil, err
	}
	return entry, nil
}

func (s *Service) List(ctx context.Context, propertyID, roomID uuid.UUID) ([]domain.RoomWaitlistEntry, error) {
	return s.waitlist.ListByRoom(ctx, propertyID, roomID)
}

type OfferResult struct {
	Entry  *domain.RoomWaitlistEntry
	Invite *domain.SlotInvite
}

func (s *Service) OfferNext(ctx context.Context, propertyID, roomID uuid.UUID, actorID uuid.UUID) (*OfferResult, error) {
	settings, err := s.settings.EnsureDefaults(ctx, propertyID)
	if err != nil {
		return nil, err
	}
	if !settings.WaitlistEnabled {
		return nil, postgres.ErrWaitlistDisabled
	}
	var result *OfferResult
	err = postgres.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
		wl := s.waitlist.WithTx(tx)
		sl := s.slots.WithTx(tx)
		entry, err := wl.NextWaitingForUpdate(ctx, roomID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return postgres.ErrWaitlistNotFound
			}
			return err
		}
		if entry.PropertyID != propertyID {
			return postgres.ErrSlotCrossProperty
		}
		avail, err := sl.AvailableNormalBeds(ctx, roomID)
		if err != nil {
			return err
		}
		if avail <= 0 {
			return postgres.ErrSlotRoomFull
		}
		_, floorID, err := wl.RoomFloor(ctx, roomID)
		if err != nil {
			return err
		}
		rent := settings.DefaultRentPaise
		if rent <= 0 {
			return fmt.Errorf("default rent not configured")
		}
		dueDay := int16(5)
		if settings.DefaultDueDay != nil {
			dueDay = *settings.DefaultDueDay
		}
		code, err := joinsvc.GenerateInviteCode()
		if err != nil {
			return err
		}
		ttlH := settings.WaitlistOfferTTLHours
		if ttlH <= 0 {
			ttlH = 48
		}
		expires := s.now().Add(time.Duration(ttlH) * time.Hour)
		invite, err := sl.CreateOpen(ctx, postgres.CreateSlotInviteInput{
			PropertyID:           propertyID,
			FloorID:              floorID,
			RoomID:               roomID,
			Code:                 code,
			RentPaise:            rent,
			DepositPaise:         settings.DefaultDepositPaise,
			DueDay:               dueDay,
			ProfileSchemaVersion: settings.ProfileSchemaVersion,
			ExpiresAt:            expires,
			CreatedByUserID:      actorID,
			ReserveBed:           settings.ReserveBedsForOpenInvites,
			MaxExtraBeds:         settings.MaxExtraBedsPerRoom,
			BoundPhone:           entry.Phone,
			WaitlistEntryID:      &entry.ID,
		})
		if err != nil {
			return err
		}
		if err := wl.MarkOffered(ctx, entry.ID, invite.ID, expires); err != nil {
			return err
		}
		entry.Status = domain.WaitlistOffered
		entry.OfferedSlotInviteID = &invite.ID
		entry.OfferExpiresAt = &expires
		result = &OfferResult{Entry: entry, Invite: invite}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) AcceptOffer(ctx context.Context, userID uuid.UUID, phone, inviteCode string, updateJoin func(ctx context.Context, slotInviteID uuid.UUID) error) (*domain.SlotInvite, error) {
	code := joinsvc.NormalizeInvite(inviteCode)
	if code == "" {
		return nil, joinsvc.ErrInvalidInvite
	}
	invite, err := s.slots.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, joinsvc.ErrInvalidInvite
		}
		return nil, err
	}
	if invite.Status != domain.SlotInviteOpen || !invite.ExpiresAt.After(s.now()) {
		return nil, joinsvc.ErrInvalidInvite
	}
	if auth.NormalizePhone(invite.BoundPhone) == "" || auth.NormalizePhone(phone) != auth.NormalizePhone(invite.BoundPhone) {
		return nil, postgres.ErrSlotBoundPhone
	}
	if err := updateJoin(ctx, invite.ID); err != nil {
		return nil, err
	}
	return invite, nil
}

func (s *Service) TryOfferAfterVacate(ctx context.Context, propertyID, roomID uuid.UUID) error {
	settings, err := s.settings.Get(ctx, propertyID)
	if err != nil || settings == nil || !settings.WaitlistEnabled {
		return nil
	}
	_, err = s.OfferNext(ctx, propertyID, roomID, uuid.Nil)
	if errors.Is(err, postgres.ErrWaitlistNotFound) || errors.Is(err, postgres.ErrSlotRoomFull) {
		return nil
	}
	return err
}

func (s *Service) ExpireOffers(ctx context.Context) (int, error) {
	now := s.now()
	expired, err := s.waitlist.ExpiredOffers(ctx, now)
	if err != nil {
		return 0, err
	}
	var n int
	for _, e := range expired {
		if e.OfferedSlotInviteID != nil {
			_, _ = s.slots.Revoke(ctx, e.PropertyID, *e.OfferedSlotInviteID, uuid.Nil)
		}
		if err := s.waitlist.ResetOffer(ctx, e.ID); err != nil {
			continue
		}
		n++
		_, _ = s.OfferNext(ctx, e.PropertyID, e.RoomID, uuid.Nil)
	}
	return n, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
