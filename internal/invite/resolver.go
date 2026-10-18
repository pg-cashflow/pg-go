package invite

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// ErrInvalidInvite is returned when a code does not resolve to an active invite.
var ErrInvalidInvite = errors.New("invalid invite code")

// Resolver maps a normalized invite code to a property + room/floor via the
// slot-invite system. The legacy bare property-level invite code (no
// room/floor binding) is retired: a code that is not a valid, open slot
// invite is simply invalid. See docs/adr/ legacy-join-retirement.
type Resolver struct {
	slots *postgres.SlotInviteRepo
}

func NewResolver(slots *postgres.SlotInviteRepo) *Resolver {
	return &Resolver{slots: slots}
}

func normalizeInviteCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func (r *Resolver) Resolve(ctx context.Context, code string) (*domain.InviteResolution, error) {
	code = normalizeInviteCode(code)
	if code == "" {
		return nil, ErrInvalidInvite
	}
	if r.slots == nil {
		return nil, ErrInvalidInvite
	}
	slot, err := r.slots.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidInvite
		}
		return nil, err
	}
	now := time.Now().UTC()
	if slot.Status != domain.SlotInviteOpen || !slot.ExpiresAt.After(now) {
		return nil, ErrInvalidInvite
	}
	return &domain.InviteResolution{
		PropertyID:   slot.PropertyID,
		SlotInviteID: &slot.ID,
		SlotInvite:   slot,
	}, nil
}
