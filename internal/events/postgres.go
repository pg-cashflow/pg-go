package events

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// EventInserter inserts an event into persistent storage.
type EventInserter interface {
	Insert(ctx context.Context, e *domain.Event) error
}

// PostgresPublisher writes events via EventInserter.
type PostgresPublisher struct {
	repo EventInserter
}

func NewPostgresPublisher(repo EventInserter) *PostgresPublisher {
	return &PostgresPublisher{repo: repo}
}

func (p *PostgresPublisher) Publish(ctx context.Context, e domain.Event) error {
	return p.repo.Insert(ctx, &e)
}

// Ensure interface compliance.
var _ Publisher = (*PostgresPublisher)(nil)
