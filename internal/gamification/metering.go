package gamification

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

const (
	MaxRoomElectricityDeltaAllowed = 400.0   // 400 kWh/month maximum room safety ceiling
	MaxFloorWaterDeltaAllowed      = 50000.0 // 50,000 Litres/month maximum floor safety ceiling
)

type MeterReadingInput struct {
	PropertyID     uuid.UUID  `json:"property_id"`
	RoomID         *uuid.UUID `json:"room_id,omitempty"`
	FloorID        *uuid.UUID `json:"floor_id,omitempty"`
	Kind           string     `json:"kind"` // electricity, water
	ReadingValue   float64    `json:"reading_value"`
	Source         string     `json:"source"`
	MeterReplaced  bool       `json:"meter_replaced"`
	ConfirmAnomaly bool       `json:"confirm_anomaly"`
	RecordedBy     uuid.UUID  `json:"recorded_by"`
}

type MeterReadingResult struct {
	Reading       domain.MeterReading `json:"reading"`
	PreviousValue float64             `json:"previous_value"`
	DeltaUnits    float64             `json:"delta_units"`
	IncludedUnits int                 `json:"included_units"`
	ExcessUnits   float64             `json:"excess_units"`
	BillablePaise int64               `json:"billable_paise"`
	AnomalyNotice string              `json:"anomaly_notice,omitempty"`
}

// RecordMeterReading validates floor/ceiling bounds and saves the reading.
func (s *Service) RecordMeterReading(ctx context.Context, in MeterReadingInput) (*MeterReadingResult, error) {
	if in.ReadingValue < 0 {
		return nil, errors.New("meter reading cannot be negative")
	}

	settings, err := s.store.GetSettings(ctx, in.PropertyID)
	if err != nil {
		return nil, err
	}

	// Fetch previous reading
	prev, err := s.store.GetLatestMeterReading(ctx, in.PropertyID, in.RoomID, in.FloorID, in.Kind)
	if err != nil {
		return nil, err
	}

	var prevVal float64
	if prev != nil {
		prevVal = prev.ReadingValue
	}

	// P0 Fix 1: Floor check (non-decreasing)
	if prev != nil && in.ReadingValue < prevVal && !in.MeterReplaced {
		return nil, fmt.Errorf("%w: current %.2f is lower than previous %.2f (pass meter_replaced=true if physically replaced)",
			ErrMeterReadingDecreased, in.ReadingValue, prevVal)
	}

	delta := in.ReadingValue - prevVal
	if in.MeterReplaced {
		delta = in.ReadingValue
	}

	// P0 Fix 2: Ceiling check
	ceiling := MaxRoomElectricityDeltaAllowed
	if in.Kind == "water" {
		ceiling = MaxFloorWaterDeltaAllowed
	}
	if delta > ceiling {
		return nil, fmt.Errorf("%w: delta %.2f exceeds maximum ceiling of %.2f", ErrMeterReadingCeiling, delta, ceiling)
	}

	// Anomaly check: if delta > 2x previous delta
	var anomalyNotice string
	if prev != nil && prevVal > 0 && delta > 250 && !in.ConfirmAnomaly {
		anomalyNotice = fmt.Sprintf("Warning: Consumption of %.2f units is unusually high. Please verify reading.", delta)
	}

	mr := &domain.MeterReading{
		PropertyID:   in.PropertyID,
		RoomID:       in.RoomID,
		FloorID:      in.FloorID,
		Kind:         in.Kind,
		ReadingValue: in.ReadingValue,
		ReadingAt:    s.now().UTC(),
		Source:       in.Source,
		RecordedBy:   in.RecordedBy,
	}

	if err := s.store.CreateMeterReading(ctx, mr); err != nil {
		return nil, err
	}

	// Compute included units and excess for room electricity
	includedUnits := 0
	excessUnits := 0.0
	billablePaise := int64(0)

	if in.Kind == "electricity" && in.RoomID != nil {
		rm, err := s.store.GetRoomByID(ctx, *in.RoomID)
		if err == nil && rm != nil {
			includedUnits = rm.IncludedUnits
			if delta > float64(includedUnits) {
				excessUnits = delta - float64(includedUnits)
				// Pure integer arithmetic via milli-units (1 kWh = 1000 milli-units) to eliminate float precision loss and truncation artifacts
				excessMilliUnits := int64(math.Round(excessUnits * 1000.0))
				tariffPaise := settings.ElectricityTariffPaise
				// Half-up integer rounding: (milliUnits * tariff + 500) / 1000
				billablePaise = (excessMilliUnits*tariffPaise + 500) / 1000
			}
		}
	}

	_ = s.pub.Publish(ctx, domain.Event{
		PropertyID: in.PropertyID,
		EventType:  domain.EvtMeterReadingRecorded,
		OccurredAt: s.now().UTC(),
	})

	return &MeterReadingResult{
		Reading:       *mr,
		PreviousValue: prevVal,
		DeltaUnits:    delta,
		IncludedUnits: includedUnits,
		ExcessUnits:   excessUnits,
		BillablePaise: billablePaise,
		AnomalyNotice: anomalyNotice,
	}, nil
}

// GenerateElectricityDue creates an electricity due for a tenant in a room.
func (s *Service) GenerateElectricityDue(ctx context.Context, tenantID uuid.UUID, amountPaise int64, periodStart, periodEnd time.Time) (*domain.Due, error) {
	tenant, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return nil, ErrTenantNotFound
	}

	code, err := domain.GenerateDueCode()
	if err != nil {
		return nil, err
	}

	due := &domain.Due{
		DueCode:        code,
		TenantID:       tenantID,
		PropertyID:     tenant.PropertyID,
		Kind:           domain.DueKindElectricity,
		Amount:         amountPaise,
		OriginalAmount: amountPaise,
		PeriodStart:    periodStart,
		PeriodEnd:      periodEnd,
		DueDate:        periodEnd,
		Status:         domain.DueStatusPending,
		CreatedAt:      s.now().UTC(),
		UpdatedAt:      s.now().UTC(),
	}

	if err := s.dues.Create(ctx, due); err != nil {
		return nil, err
	}

	return due, nil
}

// CalculateWaterRUBS calculates per-tenant water allocation from floor bulk meter.
func (s *Service) CalculateWaterRUBS(ctx context.Context, floorID uuid.UUID, startDate, endDate time.Time) (float64, int, float64, error) {
	bulkLitres, err := s.store.GetFloorWaterConsumption(ctx, floorID, startDate, endDate)
	if err != nil {
		return 0, 0, 0, err
	}

	tenantCount, err := s.store.CountFloorActiveTenants(ctx, floorID)
	if err != nil || tenantCount <= 0 {
		return bulkLitres, 1, bulkLitres, nil
	}

	perTenantLitres := bulkLitres / float64(tenantCount)
	return bulkLitres, tenantCount, perTenantLitres, nil
}
