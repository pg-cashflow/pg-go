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

// Resolver maps a normalized invite code to a property and optional slot invite.
type Resolver struct {
	props *postgres.PropertyRepo
	slots *postgres.SlotInviteRepo
}

func NewResolver(props *postgres.PropertyRepo, slots *postgres.SlotInviteRepo) *Resolver {
	return &Resolver{props: props, slots: slots}
}

func normalizeInviteCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func (r *Resolver) Resolve(ctx context.Context, code string) (*domain.InviteResolution, error) {
	code = normalizeInviteCode(code)
	if code == "" {
		return nil, ErrInvalidInvite
	}
	if r.slots != nil {
		slot, err := r.slots.GetByCode(ctx, code)
		if err == nil {
			now := time.Now().UTC()
			if slot.Status == domain.SlotInviteOpen && slot.ExpiresAt.After(now) {
				return &domain.InviteResolution{
					PropertyID:   slot.PropertyID,
					SlotInviteID: &slot.ID,
					SlotInvite:   slot,
				}, nil
			}
			return nil, ErrInvalidInvite
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	p, err := r.props.GetByInviteCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidInvite
		}
		return nil, err
	}
	return &domain.InviteResolution{PropertyID: p.ID}, nil
}
