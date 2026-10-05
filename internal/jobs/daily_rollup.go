package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// DailyRollupStore persists and retrieves daily property financial rollups.
type DailyRollupStore interface {
	UpsertDailyFinancialRollup(ctx context.Context, rollup *domain.DailyFinancialRollup) error
	ListDailyFinancialRollups(ctx context.Context, propertyID uuid.UUID, from, to time.Time) ([]domain.DailyFinancialRollup, error)
}

// DailyRollupCollector extracts daily metrics across payments, dues, expenses, and occupancy.
type DailyRollupCollector interface {
	CollectDailyMetrics(ctx context.Context, propertyID uuid.UUID, day time.Time) (*domain.DailyFinancialRollup, error)
}

// DailyRollupJob calculates and stores daily financial summaries in integer paise.
type DailyRollupJob struct {
	Properties PropertyLister
	Collector  DailyRollupCollector
	Store      DailyRollupStore
	Log        *slog.Logger
}

// NewDailyRollupJob creates a new daily rollup job.
func NewDailyRollupJob(props PropertyLister, coll DailyRollupCollector, store DailyRollupStore, log *slog.Logger) *DailyRollupJob {
	if log == nil {
		log = slog.Default()
	}
	return &DailyRollupJob{
		Properties: props,
		Collector:  coll,
		Store:      store,
		Log:        log,
	}
}

// Run executes the rollup for all properties for targetDate (interpreted in Asia/Kolkata).
func (j *DailyRollupJob) Run(ctx context.Context, targetDate time.Time) error {
	log := j.Log
	if log == nil {
		log = slog.Default()
	}

	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}

	dayIST := targetDate.In(loc)
	rollupDay := time.Date(dayIST.Year(), dayIST.Month(), dayIST.Day(), 0, 0, 0, 0, time.UTC)

	props, err := j.Properties.List(ctx)
	if err != nil {
		return fmt.Errorf("daily-rollup: list properties: %w", err)
	}

	var firstErr error
	for _, p := range props {
		if j.Collector != nil && j.Store != nil {
			rollup, err := j.Collector.CollectDailyMetrics(ctx, p.ID, rollupDay)
			if err != nil {
				log.Error("daily-rollup: failed to collect metrics", "property_id", p.ID, "err", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if err := j.Store.UpsertDailyFinancialRollup(ctx, rollup); err != nil {
				log.Error("daily-rollup: failed to persist rollup", "property_id", p.ID, "err", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}
