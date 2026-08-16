package events

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// PostgresPublisher writes events via EventRepo.Insert.
type PostgresPublisher struct {
	repo *postgres.EventRepo
}

func NewPostgresPublisher(repo *postgres.EventRepo) *PostgresPublisher {
	return &PostgresPublisher{repo: repo}
}

func (p *PostgresPublisher) Publish(ctx context.Context, e domain.Event) error {
	return p.repo.Insert(ctx, &e)
}

// Ensure interface compliance.
var _ Publisher = (*PostgresPublisher)(nil)
