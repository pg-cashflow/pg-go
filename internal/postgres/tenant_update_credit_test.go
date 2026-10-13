package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

type mockRecordingDBTX struct {
	lastSQL  string
	lastArgs []any
}

func (m *mockRecordingDBTX) Exec(_ context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	m.lastSQL = sql
	m.lastArgs = arguments
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (m *mockRecordingDBTX) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

func (m *mockRecordingDBTX) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return nil
}

func TestTenantRepo_Update_ExcludesCreditBalance_Unit(t *testing.T) {
	mockDB := &mockRecordingDBTX{}
	repo := NewTenantRepo(mockDB)

	tenantID := uuid.New()
	roomID := uuid.New()
	phone := "+919876543210"
	roomNum := "101"
	aadhaar := "1234"
	var dueDay int16 = 5
	joinedOn := time.Now().UTC()

	tnt := &domain.Tenant{
		ID:                 tenantID,
		PropertyID:         uuid.New(),
		Name:               "Test Tenant",
		Phone:              &phone,
		RoomNumber:         &roomNum,
		RoomID:             &roomID,
		AadhaarLast4:       &aadhaar,
		RentAmount:         1500000,
		DueDay:             &dueDay,
		NoticePeriodDays:   30,
		CreditBalancePaise: 999999, // Should NOT be included in UPDATE
		Status:             "active",
		PermanentAddress:   "Perm Addr",
		CurrentAddress:     "Curr Addr",
		ParentName:         "Parent",
		EmergencyPhone:     "+919876543211",
		JoinedOn:           &joinedOn,
	}

	err := repo.Update(context.Background(), tnt)
	if err != nil {
		t.Fatalf("unexpected error from Update: %v", err)
	}

	// 1. Verify SQL statement does not mention credit_balance_paise
	if strings.Contains(mockDB.lastSQL, "credit_balance_paise") {
		t.Fatalf("TenantRepo.Update SQL must not touch credit_balance_paise: %s", mockDB.lastSQL)
	}

	// 2. Verify arguments do not include the tenant's credit balance
	for idx, arg := range mockDB.lastArgs {
		if val, ok := arg.(int64); ok && val == 999999 {
			t.Fatalf("TenantRepo.Update passed credit balance at argument index %d", idx)
		}
	}

	// 3. Verify exactly 24 arguments are passed (ID at $1 + 23 columns)
	if len(mockDB.lastArgs) != 24 {
		t.Fatalf("expected 24 arguments to Update Exec, got %d", len(mockDB.lastArgs))
	}
}

func TestTenantRepo_Update_PreservesConcurrentCredit_Live(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repo := NewTenantRepo(pool)

	var tableExists bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='properties')`).Scan(&tableExists)
	if !tableExists {
		testutil.FailOnSkipIfDBRequired(t, "schema not migrated, skipping live database test")
		return
	}

	propID := uuid.New()
	tenantID := uuid.New()
	phone := "+9199999" + propID.String()[:5]
	inviteCode := "TCRD" + propID.String()[:4]

	// 1. Seed property and tenant with initial credit 10000 paise
	_, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Credit Test PG', '123 Test St', $2, 'test@upi', 'Owner', 'owner@example.com', $3)
		ON CONFLICT (id) DO NOTHING`, propID, phone, inviteCode)
	if err != nil {
		t.Fatalf("seed property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE property_id=$1`, propID)
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id=$1`, propID)
	}()

	_, err = pool.Exec(ctx, `
		INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status, credit_balance_paise,
			permanent_address, current_address, parent_name, emergency_phone, joined_on)
		VALUES ($1, $2, 'Initial Name', $3, 100000, 1, 'active', 10000,
			'Permanent', 'Current', 'Parent', '+919999999999', NOW())`,
		tenantID, propID, phone)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	// 2. Fetch tenant struct with stale credit_balance_paise (10000)
	tnt, err := repo.GetByID(ctx, tenantID)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	tnt.Name = "Updated Name"

	// 3. Concurrently add 5000 credit (overpayment) while generic Update runs
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_ = repo.AddCredit(ctx, tenantID, 5000)
	}()

	go func() {
		defer wg.Done()
		_ = repo.Update(ctx, tnt)
	}()

	wg.Wait()

	// 4. Verify that credit_balance_paise is 15000 (AddCredit survived generic Update)
	var finalCredit int64
	var finalName string
	err = pool.QueryRow(ctx, `SELECT credit_balance_paise, name FROM tenants WHERE id=$1`, tenantID).Scan(&finalCredit, &finalName)
	if err != nil {
		t.Fatalf("failed to query updated tenant: %v", err)
	}

	if finalCredit != 15000 {
		t.Fatalf("credit balance was overwritten! expected 15000, got %d", finalCredit)
	}
	if finalName != "Updated Name" {
		t.Fatalf("expected updated name 'Updated Name', got '%s'", finalName)
	}
}
