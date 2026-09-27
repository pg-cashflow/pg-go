package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLiveAttendanceRepo(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live Postgres test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skip("config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Skipf("cannot connect to Postgres (%v), skipping live test", err)
	}
	defer pool.Close()

	// Ensure all project migrations applied including 025
	_ = Migrate(ctx, pool, filepath.Join("..", "..", "migrations"))
	_, _ = pool.Exec(ctx, `ALTER TABLE wage_calculations ADD COLUMN IF NOT EXISTS days_unrecorded NUMERIC(4, 1) NOT NULL DEFAULT 0`)

	repo := NewAttendanceRepo(pool)

	// Create test property
	propID := uuid.New()
	inviteCode := fmt.Sprintf("A%s", uuid.New().String()[:7])
	ownerPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+1)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Attendance Test Property', '123 Test St', $2, 'test@upi', 'Test Owner', 'owner@example.com', $3)
		ON CONFLICT (id) DO NOTHING;
	`, propID, ownerPhone, inviteCode)
	if err != nil {
		t.Fatalf("failed to insert test property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	// Create test user for recorded_by / finalized_by
	userID := uuid.New()
	userPhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+2)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3)
		ON CONFLICT (id) DO NOTHING;
	`, userID, userPhone, propID)
	if err != nil {
		t.Fatalf("failed to insert test user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	}()

	// Create test payout payee (PayeeTypeStaff)
	payeeID := uuid.New()
	dummyHash := domain.ComputeAccountHash([]byte("test_salt_123"), uuid.New().String())
	payeePhone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+3)%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO payout_payees (
			id, property_id, payee_type, name, phone, account_number_hash, upi_vpa
		) VALUES ($1, $2, 'staff', 'Ramesh Cook', $3, $4, 'ramesh@upi')
		ON CONFLICT DO NOTHING;
	`, payeeID, propID, payeePhone, dummyHash)
	if err != nil {
		t.Fatalf("failed to insert test payee: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM payout_payees WHERE id = $1`, payeeID)
	}()

	// 1. Staff Profile: Create & Get
	staff := domain.StaffProfile{
		PropertyID:           propID,
		PayeeID:              payeeID,
		Name:                 "Ramesh Cook",
		Role:                 "Head Cook",
		BaseMonthlyWagePaise: 1800000, // 18,000 INR
		EffectiveFrom:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Status:               domain.StaffActive,
	}

	created, err := repo.CreateStaffProfile(ctx, staff)
	if err != nil {
		t.Fatalf("CreateStaffProfile failed: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("expected non-nil staff ID")
	}
	if created.Role != "Head Cook" {
		t.Errorf("expected role 'Head Cook', got %s", created.Role)
	}

	fetched, err := repo.GetStaffProfile(ctx, propID, created.ID)
	if err != nil {
		t.Fatalf("GetStaffProfile failed: %v", err)
	}
	if fetched.BaseMonthlyWagePaise != 1800000 {
		t.Errorf("expected wage 1800000, got %d", fetched.BaseMonthlyWagePaise)
	}

	// 2. List Staff Profiles
	list, err := repo.ListStaffProfiles(ctx, propID, true)
	if err != nil {
		t.Fatalf("ListStaffProfiles failed: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 active staff profile, got %d", len(list))
	}

	// 3. Leave Policy: Upsert & Get
	policy := domain.LeavePolicy{
		PropertyID:           propID,
		MonthlyFreeLeaveDays: 3,
		PaidHolidays:         []string{"2026-09-05", "2026-09-15"},
		WorkingDaysBasis:     domain.WorkingDaysBasisFixed30,
	}
	savedPolicy, err := repo.UpsertLeavePolicy(ctx, policy)
	if err != nil {
		t.Fatalf("UpsertLeavePolicy failed: %v", err)
	}
	if savedPolicy.MonthlyFreeLeaveDays != 3 {
		t.Errorf("expected 3 free leave days, got %d", savedPolicy.MonthlyFreeLeaveDays)
	}
	if len(savedPolicy.PaidHolidays) != 2 {
		t.Errorf("expected 2 holidays, got %d", len(savedPolicy.PaidHolidays))
	}

	getPolicy, err := repo.GetLeavePolicy(ctx, propID)
	if err != nil {
		t.Fatalf("GetLeavePolicy failed: %v", err)
	}
	if getPolicy.WorkingDaysBasis != domain.WorkingDaysBasisFixed30 {
		t.Errorf("expected fixed_30 basis, got %s", getPolicy.WorkingDaysBasis)
	}

	// 4. Daily Attendance: Batch Upsert & List
	entries := []domain.DailyAttendanceEntry{
		{
			StaffID: created.ID,
			Status:  domain.AttendancePresent,
		},
	}
	workDate := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	err = repo.UpsertDailyAttendanceBatchTx(ctx, nil, propID, userID, workDate, entries)
	if err != nil {
		t.Fatalf("UpsertDailyAttendanceBatchTx failed: %v", err)
	}

	records, err := repo.ListAttendanceForMonth(ctx, propID, "2026-09")
	if err != nil {
		t.Fatalf("ListAttendanceForMonth failed: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 attendance record, got %d", len(records))
	}
	if records[0].Status != domain.AttendancePresent {
		t.Errorf("expected status 'present', got %s", records[0].Status)
	}

	// 5. Wage Calculation: Save & Get
	wageCalc := domain.WageCalculation{
		PropertyID:            propID,
		StaffID:               created.ID,
		CycleMonth:            "2026-09",
		BaseMonthlyWagePaise:  1800000,
		ProratedBaseWagePaise: 1800000,
		TotalBasisDays:        30,
		EmployedBasisDays:     30,
		DaysPresent:           28,
		DaysPaidLeave:         0,
		DaysHoliday:           2,
		DaysAbsent:            0,
		DaysUnrecorded:        0,
		FreeLeaveDaysAllowed:  3,
		ExcessAbsentDays:      0,
		PerDayRatePaise:       60000,
		TotalDeductionPaise:   0,
		NetWagePaise:          1800000,
		Status:                domain.WageStatusCalculated,
		FinalizedBy:           userID,
	}

	savedCalc, err := repo.SaveWageCalculationTx(ctx, nil, wageCalc)
	if err != nil {
		t.Fatalf("SaveWageCalculationTx failed: %v", err)
	}
	if savedCalc.NetWagePaise != 1800000 {
		t.Errorf("expected net wage 1800000, got %d", savedCalc.NetWagePaise)
	}

	getCalc, err := repo.GetWageCalculation(ctx, propID, created.ID, "2026-09")
	if err != nil {
		t.Fatalf("GetWageCalculation failed: %v", err)
	}
	if getCalc.CycleMonth != "2026-09" {
		t.Errorf("expected cycle month 2026-09, got %s", getCalc.CycleMonth)
	}

	// 6. Update Staff Profile Status
	effTo := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	err = repo.UpdateStaffProfileStatus(ctx, propID, created.ID, domain.StaffInactive, &effTo)
	if err != nil {
		t.Fatalf("UpdateStaffProfileStatus failed: %v", err)
	}

	activeList, err := repo.ListStaffProfiles(ctx, propID, true)
	if err != nil {
		t.Fatalf("ListStaffProfiles active failed: %v", err)
	}
	if len(activeList) != 0 {
		t.Errorf("expected 0 active staff after deactivation, got %d", len(activeList))
	}
}
