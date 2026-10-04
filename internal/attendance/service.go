package attendance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type AttendanceRepository interface {
	GetLeavePolicy(ctx context.Context, propertyID uuid.UUID) (*domain.LeavePolicy, error)
	UpsertLeavePolicy(ctx context.Context, policy domain.LeavePolicy) (*domain.LeavePolicy, error)
	CreateStaffProfile(ctx context.Context, staff domain.StaffProfile) (*domain.StaffProfile, error)
	GetStaffProfile(ctx context.Context, propertyID, staffID uuid.UUID) (*domain.StaffProfile, error)
	ListStaffProfiles(ctx context.Context, propertyID uuid.UUID, onlyActive bool) ([]domain.StaffProfile, error)
	UpdateStaffProfileStatus(ctx context.Context, propertyID, staffID uuid.UUID, status domain.StaffProfileStatus, effectiveTo *time.Time) error
	UpsertDailyAttendanceBatchTx(ctx context.Context, tx pgx.Tx, propertyID, recordedBy uuid.UUID, workDate time.Time, entries []domain.DailyAttendanceEntry) error
	ListAttendanceForMonth(ctx context.Context, propertyID uuid.UUID, cycleMonth string) ([]domain.AttendanceRecord, error)
	ListAttendanceForStaffMonth(ctx context.Context, propertyID, staffID uuid.UUID, cycleMonth string) ([]domain.AttendanceRecord, error)
	SaveWageCalculationTx(ctx context.Context, tx pgx.Tx, calc domain.WageCalculation) (*domain.WageCalculation, error)
	GetWageCalculation(ctx context.Context, propertyID, staffID uuid.UUID, cycleMonth string) (*domain.WageCalculation, error)
	ListWageCalculationsForMonth(ctx context.Context, propertyID uuid.UUID, cycleMonth string) ([]domain.WageCalculation, error)
}

type PayoutItemRepository interface {
	CreatePayoutItemTx(ctx context.Context, tx pgx.Tx, it *domain.PayoutItem) error
}

type Service struct {
	pool           *pgxpool.Pool
	attendanceRepo AttendanceRepository
	payoutRepo     PayoutItemRepository
}

func NewService(pool *pgxpool.Pool, attendanceRepo AttendanceRepository, payoutRepo PayoutItemRepository) *Service {
	return &Service{
		pool:           pool,
		attendanceRepo: attendanceRepo,
		payoutRepo:     payoutRepo,
	}
}

// MarkDailyAttendance saves or updates attendance for multiple staff members on a given date.
func (s *Service) MarkDailyAttendance(
	ctx context.Context,
	propertyID, recordedBy uuid.UUID,
	req domain.MarkDailyAttendanceRequest,
) error {
	workDate, err := time.Parse("2006-01-02", req.WorkDate)
	if err != nil {
		return fmt.Errorf("invalid work_date (expected YYYY-MM-DD): %w", err)
	}

	if len(req.Entries) == 0 {
		return errors.New("no attendance entries provided")
	}

	staffList, err := s.attendanceRepo.ListStaffProfiles(ctx, propertyID, false)
	if err != nil {
		return fmt.Errorf("list staff: %w", err)
	}
	validStaff := make(map[uuid.UUID]bool, len(staffList))
	for _, st := range staffList {
		validStaff[st.ID] = true
	}

	seenStaff := make(map[uuid.UUID]bool, len(req.Entries))
	for _, e := range req.Entries {
		if !validStaff[e.StaffID] {
			return fmt.Errorf("staff %s not found for property", e.StaffID)
		}
		if seenStaff[e.StaffID] {
			return fmt.Errorf("duplicate entry for staff %s in same request", e.StaffID)
		}
		seenStaff[e.StaffID] = true

		if !domain.IsValidAttendanceStatus(e.Status) {
			return fmt.Errorf("invalid attendance status '%s' for staff %s", e.Status, e.StaffID)
		}
	}

	return s.attendanceRepo.UpsertDailyAttendanceBatchTx(ctx, nil, propertyID, recordedBy, workDate, req.Entries)
}

// PreviewCyclePayroll computes wage calculations for all active staff in the cycle month without persisting.
func (s *Service) PreviewCyclePayroll(
	ctx context.Context,
	propertyID uuid.UUID,
	cycleMonth string,
) ([]domain.WageCalculation, error) {
	staffList, err := s.attendanceRepo.ListStaffProfiles(ctx, propertyID, true)
	if err != nil {
		return nil, fmt.Errorf("list staff: %w", err)
	}

	policy, err := s.attendanceRepo.GetLeavePolicy(ctx, propertyID)
	if err != nil {
		return nil, fmt.Errorf("get leave policy: %w", err)
	}

	allRecords, err := s.attendanceRepo.ListAttendanceForMonth(ctx, propertyID, cycleMonth)
	if err != nil {
		return nil, fmt.Errorf("list attendance: %w", err)
	}

	recordsByStaff := make(map[uuid.UUID][]domain.AttendanceRecord)
	for _, rec := range allRecords {
		recordsByStaff[rec.StaffID] = append(recordsByStaff[rec.StaffID], rec)
	}

	var previews []domain.WageCalculation
	for _, staff := range staffList {
		calc, err := CalculateMonthlyWage(CalculationParams{
			Staff:      staff,
			Policy:     *policy,
			CycleMonth: cycleMonth,
			Records:    recordsByStaff[staff.ID],
		})
		if err != nil {
			return nil, fmt.Errorf("calculate wage for staff %s: %w", staff.ID, err)
		}
		previews = append(previews, *calc)
	}

	return previews, nil
}

// FinalizePayrollCycleTx atomically calculates wages, persists wage_calculations snapshots,
// and inserts unbatched payout_items (PayeeTypeStaff) into the payout pipeline.
func (s *Service) FinalizePayrollCycleTx(
	ctx context.Context,
	propertyID, finalizedBy uuid.UUID,
	cycleMonth string,
) ([]domain.WageCalculation, error) {
	var finalized []domain.WageCalculation

	err := postgres.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
		staffList, err := s.attendanceRepo.ListStaffProfiles(ctx, propertyID, true)
		if err != nil {
			return fmt.Errorf("list staff: %w", err)
		}

		policy, err := s.attendanceRepo.GetLeavePolicy(ctx, propertyID)
		if err != nil {
			return fmt.Errorf("get leave policy: %w", err)
		}

		allRecords, err := s.attendanceRepo.ListAttendanceForMonth(ctx, propertyID, cycleMonth)
		if err != nil {
			return fmt.Errorf("list attendance: %w", err)
		}

		recordsByStaff := make(map[uuid.UUID][]domain.AttendanceRecord)
		for _, rec := range allRecords {
			recordsByStaff[rec.StaffID] = append(recordsByStaff[rec.StaffID], rec)
		}

		for _, staff := range staffList {
			calc, err := CalculateMonthlyWage(CalculationParams{
				Staff:      staff,
				Policy:     *policy,
				CycleMonth: cycleMonth,
				Records:    recordsByStaff[staff.ID],
			})
			if err != nil {
				return fmt.Errorf("calculate wage for staff %s: %w", staff.ID, err)
			}

			calc.FinalizedBy = finalizedBy

			// Bridge: If net wage > 0, generate an unbatched payout_item
			if calc.NetWagePaise > 0 {
				refNum := fmt.Sprintf("SAL-%s-%s", strings.ReplaceAll(cycleMonth, "-", ""), staff.ID.String()[:8])
				purpose := fmt.Sprintf("Salary %s - %s", cycleMonth, staff.Name)

				item := domain.PayoutItem{
					PayeeID:         staff.PayeeID,
					AmountPaise:     calc.NetWagePaise,
					Purpose:         purpose,
					PeriodLabel:     cycleMonth,
					ReferenceNumber: refNum,
					Status:          domain.PayoutPending,
				}

				if err := s.payoutRepo.CreatePayoutItemTx(ctx, tx, &item); err != nil {
					return fmt.Errorf("create payout item for staff %s: %w", staff.ID, err)
				}

				calc.PayoutItemID = &item.ID
				calc.Status = domain.WageStatusBatched
			} else {
				calc.Status = domain.WageStatusCalculated
			}

			saved, err := s.attendanceRepo.SaveWageCalculationTx(ctx, tx, *calc)
			if err != nil {
				return fmt.Errorf("save wage calculation for staff %s: %w", staff.ID, err)
			}

			finalized = append(finalized, *saved)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return finalized, nil
}
