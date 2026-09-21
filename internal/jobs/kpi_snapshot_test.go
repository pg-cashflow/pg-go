package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/roi"
)

type mockPropertyLister struct {
	props []domain.Property
}

func (m *mockPropertyLister) List(ctx context.Context) ([]domain.Property, error) {
	return m.props, nil
}

type mockSummaryBuilder struct {
	recon *payment.ReconciliationSummary
}

func (m *mockSummaryBuilder) BuildSummary(ctx context.Context, propertyID uuid.UUID, period string) (*payment.ReconciliationSummary, error) {
	return m.recon, nil
}

type mockRoomLister struct {
	rooms []domain.Room
}

func (m *mockRoomLister) ListRooms(ctx context.Context, propertyID uuid.UUID) ([]domain.Room, error) {
	return m.rooms, nil
}

type mockTenants struct {
	tenants []domain.Tenant
}

func (m *mockTenants) ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.Tenant, error) {
	return m.tenants, nil
}

func TestKPISnapshotJobRun(t *testing.T) {
	ctx := context.Background()
	pid := uuid.New()
	st := finance.NewMemoryStore()
	finSvc := finance.NewService(st, nil)
	roiSvc := roi.NewService(finSvc)

	props := &mockPropertyLister{
		props: []domain.Property{
			{ID: pid, Name: "Test PG"},
		},
	}
	recon := &mockSummaryBuilder{
		recon: &payment.ReconciliationSummary{
			Period:        "2026-09",
			RentCollected: 10000000,
		},
	}
	rooms := &mockRoomLister{
		rooms: []domain.Room{
			{ID: uuid.New(), PropertyID: pid, Capacity: 2},
			{ID: uuid.New(), PropertyID: pid, Capacity: 2},
		},
	}
	tenants := &mockTenants{
		tenants: []domain.Tenant{
			{ID: uuid.New(), PropertyID: pid, Status: domain.TenantStatusActive},
		},
	}

	job := &KPISnapshotJob{
		Properties: props,
		Rooms:      rooms,
		Tenants:    tenants,
		Summaries:  recon,
		ROI:        roiSvc,
		Now:        func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) },
	}

	if err := job.Run(ctx); err != nil {
		t.Fatalf("kpi snapshot job failed: %v", err)
	}

	latestROI, err := st.LatestROI(ctx, pid)
	if err != nil {
		t.Fatalf("latest roi failed: %v", err)
	}
	if latestROI == nil || latestROI.PeriodMonth != "2026-09" {
		t.Fatalf("expected 2026-09 snapshot, got: %+v", latestROI)
	}
}
