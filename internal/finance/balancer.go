package finance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type SettlementBalancerStore interface {
	UpsertDailyBalance(ctx context.Context, bal *domain.DailySettlementBalance) error
	GetDailyBalance(ctx context.Context, propertyID uuid.UUID, reconDate time.Time) (*domain.DailySettlementBalance, error)
	ListDailyBalances(ctx context.Context, propertyID uuid.UUID, limit, offset int) ([]*domain.DailySettlementBalance, error)
	ComputeDayAggregates(ctx context.Context, propertyID uuid.UUID, reconDate time.Time) (*domain.DailySettlementBalance, error)
}

type SettlementBalancer struct {
	Store SettlementBalancerStore
	Now   func() time.Time
}

func NewSettlementBalancer(store SettlementBalancerStore) *SettlementBalancer {
	return &SettlementBalancer{
		Store: store,
		Now:   func() time.Time { return time.Now().UTC() },
	}
}

// RunDailyBalance executes the multi-way balancer for a given property and date, persisting the result snapshot.
func (b *SettlementBalancer) RunDailyBalance(ctx context.Context, propertyID uuid.UUID, reconDate time.Time) (*domain.DailySettlementBalance, error) {
	if b.Store == nil {
		return nil, errors.New("settlement balancer: store not configured")
	}
	bal, err := b.Store.ComputeDayAggregates(ctx, propertyID, reconDate)
	if err != nil {
		return nil, fmt.Errorf("compute day aggregates: %w", err)
	}
	if err := b.Store.UpsertDailyBalance(ctx, bal); err != nil {
		return nil, fmt.Errorf("upsert daily balance: %w", err)
	}
	return bal, nil
}

// GetDailyBalance retrieves a previously computed snapshot by property and date.
func (b *SettlementBalancer) GetDailyBalance(ctx context.Context, propertyID uuid.UUID, reconDate time.Time) (*domain.DailySettlementBalance, error) {
	if b.Store == nil {
		return nil, errors.New("settlement balancer: store not configured")
	}
	return b.Store.GetDailyBalance(ctx, propertyID, reconDate)
}

// ListDailyBalances lists historical balancer snapshots for a property.
func (b *SettlementBalancer) ListDailyBalances(ctx context.Context, propertyID uuid.UUID, limit, offset int) ([]*domain.DailySettlementBalance, error) {
	if b.Store == nil {
		return nil, errors.New("settlement balancer: store not configured")
	}
	return b.Store.ListDailyBalances(ctx, propertyID, limit, offset)
}
