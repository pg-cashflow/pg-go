package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/roi"
)

// RoomLister lists rooms for a property.
type RoomLister interface {
	ListRooms(ctx context.Context, propertyID uuid.UUID) ([]domain.Room, error)
}

// ROIPersister calculates and stores KPI and ROI snapshots.
type ROIPersister interface {
	Report(ctx context.Context, propertyID uuid.UUID, period string, recon *payment.ReconciliationSummary, rooms []domain.Room, tenants []domain.Tenant, billedRent, fixedOpex, variableOpex int64) (*roi.Report, error)
	PersistSnapshots(ctx context.Context, propertyID uuid.UUID, period string, r *roi.Report) error
}

// KPISnapshotJob runs daily to record snapshots into kpi_snapshots and roi_snapshots.
type KPISnapshotJob struct {
	Properties PropertyLister
	Rooms      RoomLister
	Tenants    PropertyTenants
	Summaries  SummaryBuilder
	ROI        ROIPersister
	Log        *slog.Logger
	Now        func() time.Time
}

func (j *KPISnapshotJob) Run(ctx context.Context) error {
	log := j.Log
	if log == nil {
		log = slog.Default()
	}
	now := time.Now().UTC()
	if j.Now != nil {
		now = j.Now()
	}
	period := now.Format("2006-01")

	props, err := j.Properties.List(ctx)
	if err != nil {
		return fmt.Errorf("kpi-snapshot: list properties: %w", err)
	}

	var firstErr error
	for _, p := range props {
		recon, err := j.Summaries.BuildSummary(ctx, p.ID, period)
		if err != nil {
			log.Error("kpi-snapshot: build summary failed", "property_id", p.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		var rooms []domain.Room
		if j.Rooms != nil {
			rooms, _ = j.Rooms.ListRooms(ctx, p.ID)
		}

		var tenants []domain.Tenant
		if j.Tenants != nil {
			tenants, _ = j.Tenants.ListByProperty(ctx, p.ID)
		}

		rep, err := j.ROI.Report(ctx, p.ID, period, recon, rooms, tenants, 0, 0, 0)
		if err != nil {
			log.Error("kpi-snapshot: roi report failed", "property_id", p.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		if err := j.ROI.PersistSnapshots(ctx, p.ID, period, rep); err != nil {
			log.Error("kpi-snapshot: persist failed", "property_id", p.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		log.Info("kpi-snapshot: recorded", "property_id", p.ID, "period", period, "official", rep.Official)
	}

	return firstErr
}
