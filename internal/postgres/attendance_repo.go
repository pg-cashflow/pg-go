package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrStaffNotFound        = errors.New("staff profile not found")
	ErrLeavePolicyNotFound  = errors.New("leave policy not found")
	ErrWageSnapshotNotFound = errors.New("wage calculation not found")
)

type AttendanceRepo struct {
	pool *pgxpool.Pool
}

func NewAttendanceRepo(pool *pgxpool.Pool) *AttendanceRepo {
	return &AttendanceRepo{pool: pool}
}

// ----------------------------------------------------------------------------
// Staff Profiles
// ----------------------------------------------------------------------------

func (r *AttendanceRepo) CreateStaffProfile(ctx context.Context, staff domain.StaffProfile) (*domain.StaffProfile, error) {
	query := `
		INSERT INTO staff_profiles (
			id, property_id, payee_id, name, role, phone,
			base_monthly_wage_paise, effective_from, effective_to, status,
			created_at, updated_at
		) VALUES (
			COALESCE(NULLIF($1, '00000000-0000-0000-0000-000000000000'::uuid), gen_random_uuid()),
			$2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW()
		)
		RETURNING id, property_id, payee_id, name, role, phone,
		          base_monthly_wage_paise, effective_from, effective_to, status,
		          created_at, updated_at;
	`

	var out domain.StaffProfile
	err := r.pool.QueryRow(ctx, query,
		staff.ID,
		staff.PropertyID,
		staff.PayeeID,
		staff.Name,
		staff.Role,
		staff.Phone,
		staff.BaseMonthlyWagePaise,
		staff.EffectiveFrom,
		staff.EffectiveTo,
		staff.Status,
	).Scan(
		&out.ID,
		&out.PropertyID,
		&out.PayeeID,
		&out.Name,
		&out.Role,
		&out.Phone,
		&out.BaseMonthlyWagePaise,
		&out.EffectiveFrom,
		&out.EffectiveTo,
		&out.Status,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create staff profile: %w", err)
	}

	return &out, nil
}

func (r *AttendanceRepo) GetStaffProfile(ctx context.Context, propertyID, staffID uuid.UUID) (*domain.StaffProfile, error) {
	query := `
		SELECT id, property_id, payee_id, name, role, phone,
		       base_monthly_wage_paise, effective_from, effective_to, status,
		       created_at, updated_at
		FROM staff_profiles
		WHERE property_id = $1 AND id = $2;
	`

	var out domain.StaffProfile
	err := r.pool.QueryRow(ctx, query, propertyID, staffID).Scan(
		&out.ID,
		&out.PropertyID,
		&out.PayeeID,
		&out.Name,
		&out.Role,
		&out.Phone,
		&out.BaseMonthlyWagePaise,
		&out.EffectiveFrom,
		&out.EffectiveTo,
		&out.Status,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStaffNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get staff profile: %w", err)
	}

	return &out, nil
}

func (r *AttendanceRepo) ListStaffProfiles(ctx context.Context, propertyID uuid.UUID, onlyActive bool) ([]domain.StaffProfile, error) {
	query := `
		SELECT id, property_id, payee_id, name, role, phone,
		       base_monthly_wage_paise, effective_from, effective_to, status,
		       created_at, updated_at
		FROM staff_profiles
		WHERE property_id = $1
		  AND ($2 = FALSE OR status = 'active')
		ORDER BY name ASC;
	`

	rows, err := r.pool.Query(ctx, query, propertyID, onlyActive)
	if err != nil {
		return nil, fmt.Errorf("list staff profiles: %w", err)
	}
	defer rows.Close()

	var list []domain.StaffProfile
	for rows.Next() {
		var s domain.StaffProfile
		if err := rows.Scan(
			&s.ID,
			&s.PropertyID,
			&s.PayeeID,
			&s.Name,
			&s.Role,
			&s.Phone,
			&s.BaseMonthlyWagePaise,
			&s.EffectiveFrom,
			&s.EffectiveTo,
			&s.Status,
			&s.CreatedAt,
			&s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan staff profile: %w", err)
		}
		list = append(list, s)
	}

	return list, nil
}

func (r *AttendanceRepo) UpdateStaffProfileStatus(ctx context.Context, propertyID, staffID uuid.UUID, status domain.StaffProfileStatus, effectiveTo *time.Time) error {
	query := `
		UPDATE staff_profiles
		SET status = $1, effective_to = $2, updated_at = NOW()
		WHERE property_id = $3 AND id = $4;
	`
	cmd, err := r.pool.Exec(ctx, query, status, effectiveTo, propertyID, staffID)
	if err != nil {
		return fmt.Errorf("update staff profile status: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		return ErrStaffNotFound
	}
	return nil
}

// ----------------------------------------------------------------------------
// Leave Policies
// ----------------------------------------------------------------------------

func (r *AttendanceRepo) GetLeavePolicy(ctx context.Context, propertyID uuid.UUID) (*domain.LeavePolicy, error) {
	query := `
		SELECT id, property_id, monthly_free_leave_days, paid_holidays, working_days_basis,
		       created_at, updated_at
		FROM leave_policies
		WHERE property_id = $1;
	`

	var p domain.LeavePolicy
	var rawHolidays []byte
	err := r.pool.QueryRow(ctx, query, propertyID).Scan(
		&p.ID,
		&p.PropertyID,
		&p.MonthlyFreeLeaveDays,
		&rawHolidays,
		&p.WorkingDaysBasis,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Return default unpersisted policy for property
		return &domain.LeavePolicy{
			PropertyID:           propertyID,
			MonthlyFreeLeaveDays: 2,
			PaidHolidays:         []string{},
			WorkingDaysBasis:     domain.WorkingDaysBasisCalendarDays,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get leave policy: %w", err)
	}

	if len(rawHolidays) > 0 {
		_ = json.Unmarshal(rawHolidays, &p.PaidHolidays)
	}

	return &p, nil
}

func (r *AttendanceRepo) UpsertLeavePolicy(ctx context.Context, policy domain.LeavePolicy) (*domain.LeavePolicy, error) {
	holidaysJSON, err := json.Marshal(policy.PaidHolidays)
	if err != nil {
		return nil, fmt.Errorf("marshal holidays: %w", err)
	}

	query := `
		INSERT INTO leave_policies (
			property_id, monthly_free_leave_days, paid_holidays, working_days_basis,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, NOW(), NOW()
		)
		ON CONFLICT (property_id) DO UPDATE SET
			monthly_free_leave_days = EXCLUDED.monthly_free_leave_days,
			paid_holidays = EXCLUDED.paid_holidays,
			working_days_basis = EXCLUDED.working_days_basis,
			updated_at = NOW()
		RETURNING id, property_id, monthly_free_leave_days, paid_holidays, working_days_basis,
		          created_at, updated_at;
	`

	var out domain.LeavePolicy
	var rawHolidays []byte
	err = r.pool.QueryRow(ctx, query,
		policy.PropertyID,
		policy.MonthlyFreeLeaveDays,
		holidaysJSON,
		policy.WorkingDaysBasis,
	).Scan(
		&out.ID,
		&out.PropertyID,
		&out.MonthlyFreeLeaveDays,
		&rawHolidays,
		&out.WorkingDaysBasis,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert leave policy: %w", err)
	}

	if len(rawHolidays) > 0 {
		_ = json.Unmarshal(rawHolidays, &out.PaidHolidays)
	}

	return &out, nil
}

// ----------------------------------------------------------------------------
// Daily Attendance Records
// ----------------------------------------------------------------------------

func (r *AttendanceRepo) UpsertDailyAttendanceBatchTx(
	ctx context.Context,
	tx pgx.Tx,
	propertyID, recordedBy uuid.UUID,
	workDate time.Time,
	entries []domain.DailyAttendanceEntry,
) error {
	var runner DBTX = r.pool
	if tx != nil {
		runner = tx
	}

	query := `
		INSERT INTO attendance_records (
			property_id, staff_id, work_date, status, notes, recorded_by,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, NOW(), NOW()
		)
		ON CONFLICT (property_id, staff_id, work_date) DO UPDATE SET
			status = EXCLUDED.status,
			notes = EXCLUDED.notes,
			recorded_by = EXCLUDED.recorded_by,
			updated_at = NOW();
	`

	dateOnly := time.Date(workDate.Year(), workDate.Month(), workDate.Day(), 0, 0, 0, 0, time.UTC)
	for _, entry := range entries {
		_, err := runner.Exec(ctx, query,
			propertyID,
			entry.StaffID,
			dateOnly,
			entry.Status,
			entry.Notes,
			recordedBy,
		)
		if err != nil {
			return fmt.Errorf("upsert attendance record (staff %s): %w", entry.StaffID, err)
		}
	}

	return nil
}

func (r *AttendanceRepo) ListAttendanceForMonth(ctx context.Context, propertyID uuid.UUID, cycleMonth string) ([]domain.AttendanceRecord, error) {
	cycleTime, err := time.Parse("2006-01", cycleMonth)
	if err != nil {
		return nil, fmt.Errorf("invalid cycleMonth: %w", err)
	}
	start := time.Date(cycleTime.Year(), cycleTime.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)

	query := `
		SELECT id, property_id, staff_id, work_date, status, notes, recorded_by,
		       created_at, updated_at
		FROM attendance_records
		WHERE property_id = $1
		  AND work_date >= $2 AND work_date < $3
		ORDER BY work_date ASC, staff_id ASC;
	`

	rows, err := r.pool.Query(ctx, query, propertyID, start, end)
	if err != nil {
		return nil, fmt.Errorf("list attendance for month: %w", err)
	}
	defer rows.Close()

	var list []domain.AttendanceRecord
	for rows.Next() {
		var rec domain.AttendanceRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.PropertyID,
			&rec.StaffID,
			&rec.WorkDate,
			&rec.Status,
			&rec.Notes,
			&rec.RecordedBy,
			&rec.CreatedAt,
			&rec.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan attendance record: %w", err)
		}
		list = append(list, rec)
	}

	return list, nil
}

func (r *AttendanceRepo) ListAttendanceForStaffMonth(ctx context.Context, propertyID, staffID uuid.UUID, cycleMonth string) ([]domain.AttendanceRecord, error) {
	cycleTime, err := time.Parse("2006-01", cycleMonth)
	if err != nil {
		return nil, fmt.Errorf("invalid cycleMonth: %w", err)
	}
	start := time.Date(cycleTime.Year(), cycleTime.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)

	query := `
		SELECT id, property_id, staff_id, work_date, status, notes, recorded_by,
		       created_at, updated_at
		FROM attendance_records
		WHERE property_id = $1 AND staff_id = $2
		  AND work_date >= $3 AND work_date < $4
		ORDER BY work_date ASC;
	`

	rows, err := r.pool.Query(ctx, query, propertyID, staffID, start, end)
	if err != nil {
		return nil, fmt.Errorf("list staff attendance: %w", err)
	}
	defer rows.Close()

	var list []domain.AttendanceRecord
	for rows.Next() {
		var rec domain.AttendanceRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.PropertyID,
			&rec.StaffID,
			&rec.WorkDate,
			&rec.Status,
			&rec.Notes,
			&rec.RecordedBy,
			&rec.CreatedAt,
			&rec.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan staff attendance record: %w", err)
		}
		list = append(list, rec)
	}

	return list, nil
}

// ----------------------------------------------------------------------------
// Wage Calculations (Cycle Snapshots)
// ----------------------------------------------------------------------------

func (r *AttendanceRepo) SaveWageCalculationTx(ctx context.Context, tx pgx.Tx, calc domain.WageCalculation) (*domain.WageCalculation, error) {
	var runner DBTX = r.pool
	if tx != nil {
		runner = tx
	}

	query := `
		INSERT INTO wage_calculations (
			property_id, staff_id, cycle_month,
			base_monthly_wage_paise, prorated_base_wage_paise,
			total_basis_days, employed_basis_days,
			days_present, days_paid_leave, days_holiday, days_absent,
			free_leave_days_allowed, excess_absent_days,
			per_day_rate_paise, total_deduction_paise, net_wage_paise,
			payout_item_id, status, calculated_at, finalized_by
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, NOW(), $19
		)
		ON CONFLICT (property_id, staff_id, cycle_month) DO UPDATE SET
			prorated_base_wage_paise = EXCLUDED.prorated_base_wage_paise,
			total_basis_days = EXCLUDED.total_basis_days,
			employed_basis_days = EXCLUDED.employed_basis_days,
			days_present = EXCLUDED.days_present,
			days_paid_leave = EXCLUDED.days_paid_leave,
			days_holiday = EXCLUDED.days_holiday,
			days_absent = EXCLUDED.days_absent,
			free_leave_days_allowed = EXCLUDED.free_leave_days_allowed,
			excess_absent_days = EXCLUDED.excess_absent_days,
			per_day_rate_paise = EXCLUDED.per_day_rate_paise,
			total_deduction_paise = EXCLUDED.total_deduction_paise,
			net_wage_paise = EXCLUDED.net_wage_paise,
			payout_item_id = EXCLUDED.payout_item_id,
			status = EXCLUDED.status,
			calculated_at = NOW(),
			finalized_by = EXCLUDED.finalized_by
		RETURNING id, property_id, staff_id, cycle_month,
		          base_monthly_wage_paise, prorated_base_wage_paise,
		          total_basis_days, employed_basis_days,
		          days_present, days_paid_leave, days_holiday, days_absent,
		          free_leave_days_allowed, excess_absent_days,
		          per_day_rate_paise, total_deduction_paise, net_wage_paise,
		          payout_item_id, status, calculated_at, finalized_by;
	`

	var out domain.WageCalculation
	err := runner.QueryRow(ctx, query,
		calc.PropertyID,
		calc.StaffID,
		calc.CycleMonth,
		calc.BaseMonthlyWagePaise,
		calc.ProratedBaseWagePaise,
		calc.TotalBasisDays,
		calc.EmployedBasisDays,
		calc.DaysPresent,
		calc.DaysPaidLeave,
		calc.DaysHoliday,
		calc.DaysAbsent,
		calc.FreeLeaveDaysAllowed,
		calc.ExcessAbsentDays,
		calc.PerDayRatePaise,
		calc.TotalDeductionPaise,
		calc.NetWagePaise,
		calc.PayoutItemID,
		calc.Status,
		calc.FinalizedBy,
	).Scan(
		&out.ID,
		&out.PropertyID,
		&out.StaffID,
		&out.CycleMonth,
		&out.BaseMonthlyWagePaise,
		&out.ProratedBaseWagePaise,
		&out.TotalBasisDays,
		&out.EmployedBasisDays,
		&out.DaysPresent,
		&out.DaysPaidLeave,
		&out.DaysHoliday,
		&out.DaysAbsent,
		&out.FreeLeaveDaysAllowed,
		&out.ExcessAbsentDays,
		&out.PerDayRatePaise,
		&out.TotalDeductionPaise,
		&out.NetWagePaise,
		&out.PayoutItemID,
		&out.Status,
		&out.CalculatedAt,
		&out.FinalizedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("save wage calculation: %w", err)
	}

	return &out, nil
}

func (r *AttendanceRepo) GetWageCalculation(ctx context.Context, propertyID, staffID uuid.UUID, cycleMonth string) (*domain.WageCalculation, error) {
	query := `
		SELECT id, property_id, staff_id, cycle_month,
		       base_monthly_wage_paise, prorated_base_wage_paise,
		       total_basis_days, employed_basis_days,
		       days_present, days_paid_leave, days_holiday, days_absent,
		       free_leave_days_allowed, excess_absent_days,
		       per_day_rate_paise, total_deduction_paise, net_wage_paise,
		       payout_item_id, status, calculated_at, finalized_by
		FROM wage_calculations
		WHERE property_id = $1 AND staff_id = $2 AND cycle_month = $3;
	`

	var out domain.WageCalculation
	err := r.pool.QueryRow(ctx, query, propertyID, staffID, cycleMonth).Scan(
		&out.ID,
		&out.PropertyID,
		&out.StaffID,
		&out.CycleMonth,
		&out.BaseMonthlyWagePaise,
		&out.ProratedBaseWagePaise,
		&out.TotalBasisDays,
		&out.EmployedBasisDays,
		&out.DaysPresent,
		&out.DaysPaidLeave,
		&out.DaysHoliday,
		&out.DaysAbsent,
		&out.FreeLeaveDaysAllowed,
		&out.ExcessAbsentDays,
		&out.PerDayRatePaise,
		&out.TotalDeductionPaise,
		&out.NetWagePaise,
		&out.PayoutItemID,
		&out.Status,
		&out.CalculatedAt,
		&out.FinalizedBy,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWageSnapshotNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get wage calculation: %w", err)
	}

	return &out, nil
}

func (r *AttendanceRepo) ListWageCalculationsForMonth(ctx context.Context, propertyID uuid.UUID, cycleMonth string) ([]domain.WageCalculation, error) {
	query := `
		SELECT id, property_id, staff_id, cycle_month,
		       base_monthly_wage_paise, prorated_base_wage_paise,
		       total_basis_days, employed_basis_days,
		       days_present, days_paid_leave, days_holiday, days_absent,
		       free_leave_days_allowed, excess_absent_days,
		       per_day_rate_paise, total_deduction_paise, net_wage_paise,
		       payout_item_id, status, calculated_at, finalized_by
		FROM wage_calculations
		WHERE property_id = $1 AND cycle_month = $2
		ORDER BY staff_id ASC;
	`

	rows, err := r.pool.Query(ctx, query, propertyID, cycleMonth)
	if err != nil {
		return nil, fmt.Errorf("list wage calculations: %w", err)
	}
	defer rows.Close()

	var list []domain.WageCalculation
	for rows.Next() {
		var out domain.WageCalculation
		if err := rows.Scan(
			&out.ID,
			&out.PropertyID,
			&out.StaffID,
			&out.CycleMonth,
			&out.BaseMonthlyWagePaise,
			&out.ProratedBaseWagePaise,
			&out.TotalBasisDays,
			&out.EmployedBasisDays,
			&out.DaysPresent,
			&out.DaysPaidLeave,
			&out.DaysHoliday,
			&out.DaysAbsent,
			&out.FreeLeaveDaysAllowed,
			&out.ExcessAbsentDays,
			&out.PerDayRatePaise,
			&out.TotalDeductionPaise,
			&out.NetWagePaise,
			&out.PayoutItemID,
			&out.Status,
			&out.CalculatedAt,
			&out.FinalizedBy,
		); err != nil {
			return nil, fmt.Errorf("scan wage calculation: %w", err)
		}
		list = append(list, out)
	}

	return list, nil
}
