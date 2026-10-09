package postgres

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

type recordedSettlement struct {
	PropertyID             uuid.UUID
	DepartureID            uuid.UUID
	DepositPaise           int64
	UnusedRentReversal     int64
	DamagesPaise           int64
	NetRefundPaise         int64
	ProratedRentOwedPaise  int64
	ReceivableBalancePaise int64
}

type testMirrorer struct {
	settlements []recordedSettlement
}

func (m *testMirrorer) MirrorDepartureSettlement(
	ctx context.Context,
	propertyID, departureID uuid.UUID,
	depositPaise, unusedRentReversal, damagesPaise, netRefundPaise, outstandingDuesNettedPaise, receivableBalancePaise int64,
	at time.Time,
) error {
	m.settlements = append(m.settlements, recordedSettlement{
		PropertyID:             propertyID,
		DepartureID:            departureID,
		DepositPaise:           depositPaise,
		UnusedRentReversal:     unusedRentReversal,
		DamagesPaise:           damagesPaise,
		NetRefundPaise:         netRefundPaise,
		ProratedRentOwedPaise:  outstandingDuesNettedPaise,
		ReceivableBalancePaise: receivableBalancePaise,
	})
	return nil
}

func TestLivePostgresDepartureSettlementScenarios(t *testing.T) {
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, fmt.Sprintf("cannot connect to Postgres (%v), skipping live test", err))
	}
	defer pool.Close()

	// Ensure all project migrations applied
	_ = Migrate(ctx, pool, filepath.Join("..", "..", "migrations"))

	mirror := &testMirrorer{}
	payoutRepo := NewPayoutRepo(pool, mirror)

	// Create test Property
	propID := uuid.New()
	inviteCode := fmt.Sprintf("DEP%s", uuid.New().String()[:8])
	_, err = pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Departure Test PG', '123 Test St', '+919999988888', 'owner@upi', 'Owner', 'owner@test.com', $2)`,
		propID, inviteCode,
	)
	if err != nil {
		t.Fatalf("failed to insert property: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, propID)
	}()

	// Create owner user for batch approval
	ownerID := uuid.New()
	ownerPhone := fmt.Sprintf("+91%010d", time.Now().UnixNano()%10000000000)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, phone, role, property_id)
		VALUES ($1, $2, 'owner', $3)`, ownerID, ownerPhone, propID,
	)
	if err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, ownerID)
	}()

	// Create Payee for tenant deposit refund
	payeeID := uuid.New()
	payeeHash := domain.ComputeAccountHash([]byte("test_salt"), "9876543210@upi")
	_, err = pool.Exec(ctx, `
		INSERT INTO payout_payees (
			id, property_id, payee_type, name, phone, account_number_hash, upi_vpa, is_verified, created_at, updated_at
		) VALUES ($1, $2, 'tenant_deposit', 'Tenant Payee', '9876543210', $3, '9876543210@upi', true, NOW(), NOW())`,
		payeeID, propID, payeeHash,
	)
	if err != nil {
		t.Fatalf("failed to insert payee: %v", err)
	}

	// -------------------------------------------------------------
	// SCENARIO 1: Unpaid rent mid-cycle
	// Billed: ₹5,500 (550000 paise). Prorated: ₹2,750 (275000 paise).
	// Tenant paid: ₹0. Deposit: ₹10,000 (1000000 paise).
	// Damage deduction: ₹1,000 (100000 paise).
	// Expected:
	// - Prorated rent owed (275000) netted from deposit
	// - Internal payment created for 275000, due status = 'paid', amount = 0, contractual_ceiling = 275000
	// - Net refund = 1000000 - 275000 - 100000 = 625000 (₹6,250)
	// - Unbatched payout item for 625000 created in 'pending' status
	// -------------------------------------------------------------
	t.Run("Scenario 1: Unpaid Rent Netted from Deposit", func(t *testing.T) {
		tenantID := uuid.New()
		phone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+1)%10000000000)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Tenant Scenario 1', $3, 550000, 1, 'active')`, tenantID, propID, phone,
		)
		if err != nil {
			t.Fatalf("insert tenant: %v", err)
		}

		dueID := uuid.New()
		pStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		pEnd := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
		dueDate := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
		dueCode := fmt.Sprintf("D%s", uuid.New().String()[:7])
		_, err = pool.Exec(ctx, `
			INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
			VALUES ($1, $2, $3, $4, 'rent', 550000, 550000, $5, $6, $7, 'pending')`,
			dueID, dueCode, tenantID, propID, pStart, pEnd, dueDate,
		)
		if err != nil {
			t.Fatalf("insert due: %v", err)
		}

		// Notice given, vacate date Day 15
		depID := uuid.New()
		vacateDate := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenant_departures (id, tenant_id, property_id, notice_given_at, planned_vacate_date, deposit_amount_paise, net_refund_paise, status)
			VALUES ($1, $2, $3, NOW(), $4, 1000000, 0, 'pending')`, depID, tenantID, propID, vacateDate,
		)
		if err != nil {
			t.Fatalf("insert departure: %v", err)
		}

		// Add damage deduction: ₹1,000 (100000 paise)
		ded := &domain.DepartureDeduction{
			DepartureID: depID,
			Description: "Broken wall lamp",
			AmountPaise: 100000,
			Status:      domain.DeductionAgreed,
		}
		if err := payoutRepo.AddDeduction(ctx, ded); err != nil {
			t.Fatalf("add deduction: %v", err)
		}

		// Execute departure settlement
		res, err := payoutRepo.SettleDepartureUnderLock(ctx, SettleDepartureParams{
			DepartureID:       depID,
			ActualVacateDate:  vacateDate,
			ProratedRentPaise: 275000,
			PayeeID:           &payeeID,
		})
		if err != nil {
			t.Fatalf("settle departure failed: %v", err)
		}

		if res.NetRefundPaise != 625000 {
			t.Errorf("expected net refund 625000, got %d", res.NetRefundPaise)
		}
		if res.OutstandingDuesNettedPaise != 275000 {
			t.Errorf("expected outstanding dues netted 275000, got %d", res.OutstandingDuesNettedPaise)
		}
		if res.UnusedRentRefundPaise != 0 {
			t.Errorf("expected unused rent refund 0, got %d", res.UnusedRentRefundPaise)
		}
		if res.Due == nil || res.Due.Status != domain.DueStatusPaid || res.Due.Amount != 0 {
			t.Errorf("expected cycle due to be paid with amount 0, got status=%v amount=%v", res.Due.Status, res.Due.Amount)
		}
		if res.Due.ContractualCeilingPaise == nil || *res.Due.ContractualCeilingPaise != 275000 {
			t.Errorf("expected contractual ceiling 275000, got %v", res.Due.ContractualCeilingPaise)
		}
		if res.PayoutItem == nil || res.PayoutItem.AmountPaise != 625000 {
			t.Errorf("expected payout item for 625000, got %v", res.PayoutItem)
		}

		// Verify tenant status is 'vacated'
		var tStatus string
		_ = pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&tStatus)
		if tStatus != "vacated" {
			t.Errorf("expected tenant status 'vacated', got %s", tStatus)
		}
	})

	// -------------------------------------------------------------
	// SCENARIO 2: Advance rent prepaid in full
	// Billed: ₹5,500 (550000 paise). Prorated: ₹2,750 (275000 paise).
	// Tenant paid: ₹5,500. Deposit: ₹10,000 (1000000 paise).
	// Damage deduction: ₹1,000 (100000 paise).
	// Expected:
	// - Unused rent refund = 550000 - 275000 = 275000
	// - departure_due_adjustments row for 275000 inserted
	// - Due contractual_ceiling = 275000, status remains 'paid'
	// - Net refund = 1000000 + 275000 - 100000 = 1175000 (₹11,750)
	// - Unbatched payout item for 1175000 created in 'pending' status
	// -------------------------------------------------------------
	t.Run("Scenario 2: Advance Rent Reversal and Full Settlement", func(t *testing.T) {
		tenantID := uuid.New()
		phone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+2)%10000000000)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Tenant Scenario 2', $3, 550000, 1, 'active')`, tenantID, propID, phone,
		)
		if err != nil {
			t.Fatalf("insert tenant: %v", err)
		}

		dueID := uuid.New()
		pStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		pEnd := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
		dueDate := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
		dueCode := fmt.Sprintf("D%s", uuid.New().String()[:7])
		now := time.Now().UTC()
		_, err = pool.Exec(ctx, `
			INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status, paid_at)
			VALUES ($1, $2, $3, $4, 'rent', 0, 550000, $5, $6, $7, 'paid', $8)`,
			dueID, dueCode, tenantID, propID, pStart, pEnd, dueDate, now,
		)
		if err != nil {
			t.Fatalf("insert due: %v", err)
		}

		// Insert payment and allocation for 550000
		payID := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO payments (id, tenant_id, property_id, due_id, amount, matched_by, provider, is_unapplied)
			VALUES ($1, $2, $4, $3, 550000, 'cash', 'cash', false)`, payID, tenantID, dueID, propID,
		)
		if err != nil {
			t.Fatalf("insert payment: %v", err)
		}
		// Trg auto allocates to payment_allocations

		depID := uuid.New()
		vacateDate := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenant_departures (id, tenant_id, property_id, notice_given_at, planned_vacate_date, deposit_amount_paise, net_refund_paise, status)
			VALUES ($1, $2, $3, NOW(), $4, 1000000, 0, 'inspected')`, depID, tenantID, propID, vacateDate,
		)
		if err != nil {
			t.Fatalf("insert departure: %v", err)
		}

		// Add deduction: ₹1,000
		ded := &domain.DepartureDeduction{
			DepartureID: depID,
			Description: "Room cleaning",
			AmountPaise: 100000,
			Status:      domain.DeductionAgreed,
		}
		if err := payoutRepo.AddDeduction(ctx, ded); err != nil {
			t.Fatalf("add deduction: %v", err)
		}

		// Execute departure settlement
		res, err := payoutRepo.SettleDepartureUnderLock(ctx, SettleDepartureParams{
			DepartureID:       depID,
			ActualVacateDate:  vacateDate,
			ProratedRentPaise: 275000,
			PayeeID:           &payeeID,
		})
		if err != nil {
			t.Fatalf("settle departure failed: %v", err)
		}

		if res.UnusedRentRefundPaise != 275000 {
			t.Errorf("expected unused rent refund 275000, got %d", res.UnusedRentRefundPaise)
		}
		if res.ProratedRentOwedPaise != 0 {
			t.Errorf("expected prorated rent owed 0, got %d", res.ProratedRentOwedPaise)
		}
		if res.NetRefundPaise != 1175000 {
			t.Errorf("expected net refund 1175000, got %d", res.NetRefundPaise)
		}
		if len(res.DueAdjustments) != 1 || res.DueAdjustments[0].AmountPaise != 275000 {
			t.Errorf("expected 1 due adjustment of 275000, got %v", res.DueAdjustments)
		}
		if res.PayoutItem == nil || res.PayoutItem.AmountPaise != 1175000 {
			t.Errorf("expected payout item for 1175000, got %v", res.PayoutItem)
		}
	})

	// -------------------------------------------------------------
	// SCENARIO 3: Partial payment with excess above prorated owed
	// Billed: ₹5,500 (550000 paise). Prorated: ₹2,750 (275000 paise).
	// Tenant paid: ₹3,000 (300000 paise). Deposit: ₹10,000 (1000000 paise).
	// Damages: ₹1,000 (100000 paise).
	// Excess paid over prorated = 300000 - 275000 = 25000 paise.
	// Expected:
	// - Unused rent refund = 25000
	// - Net refund = 1000000 + 25000 - 100000 = 925000 (₹9,250)
	// - Due marked 'paid', contractual_ceiling = 275000
	// -------------------------------------------------------------
	t.Run("Scenario 3: Partial Payment Over Prorated Owed", func(t *testing.T) {
		tenantID := uuid.New()
		phone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+3)%10000000000)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Tenant Scenario 3', $3, 550000, 1, 'active')`, tenantID, propID, phone,
		)
		if err != nil {
			t.Fatalf("insert tenant: %v", err)
		}

		dueID := uuid.New()
		pStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		pEnd := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
		dueDate := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
		dueCode := fmt.Sprintf("D%s", uuid.New().String()[:7])
		_, err = pool.Exec(ctx, `
			INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
			VALUES ($1, $2, $3, $4, 'rent', 250000, 550000, $5, $6, $7, 'partial')`,
			dueID, dueCode, tenantID, propID, pStart, pEnd, dueDate,
		)
		if err != nil {
			t.Fatalf("insert due: %v", err)
		}

		// Insert payment and allocation for 300000
		payID := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO payments (id, tenant_id, property_id, due_id, amount, matched_by, provider, is_unapplied)
			VALUES ($1, $2, $4, $3, 300000, 'cash', 'cash', false)`, payID, tenantID, dueID, propID,
		)
		if err != nil {
			t.Fatalf("insert payment: %v", err)
		}

		depID := uuid.New()
		vacateDate := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenant_departures (id, tenant_id, property_id, notice_given_at, planned_vacate_date, deposit_amount_paise, net_refund_paise, status)
			VALUES ($1, $2, $3, NOW(), $4, 1000000, 0, 'inspected')`, depID, tenantID, propID, vacateDate,
		)
		if err != nil {
			t.Fatalf("insert departure: %v", err)
		}

		// Add deduction: ₹1,000
		ded := &domain.DepartureDeduction{
			DepartureID: depID,
			Description: "Deep cleaning",
			AmountPaise: 100000,
			Status:      domain.DeductionAgreed,
		}
		if err := payoutRepo.AddDeduction(ctx, ded); err != nil {
			t.Fatalf("add deduction: %v", err)
		}

		// Execute departure settlement
		res, err := payoutRepo.SettleDepartureUnderLock(ctx, SettleDepartureParams{
			DepartureID:       depID,
			ActualVacateDate:  vacateDate,
			ProratedRentPaise: 275000,
			PayeeID:           &payeeID,
		})
		if err != nil {
			t.Fatalf("settle departure failed: %v", err)
		}

		if res.UnusedRentRefundPaise != 25000 {
			t.Errorf("expected unused rent refund 25000, got %d", res.UnusedRentRefundPaise)
		}
		if res.NetRefundPaise != 925000 {
			t.Errorf("expected net refund 925000, got %d", res.NetRefundPaise)
		}
		if res.Due.Status != domain.DueStatusPaid || res.Due.Amount != 0 {
			t.Errorf("expected due to be paid with amount 0, got %s / %d", res.Due.Status, res.Due.Amount)
		}
	})

	// -------------------------------------------------------------
	// SCENARIO 4: Negative Net Refund (Damages Exceed Deposit)
	// Billed: ₹5,500 (550000 paise). Prorated: ₹2,750 (275000 paise).
	// Tenant paid: ₹0. Deposit: ₹5,000 (500000 paise).
	// Damage deductions: ₹6,000 (600000 paise) (severe room damage).
	// Expected:
	// - Prorated rent owed (275000) netted from deposit
	// - Total debits = 275000 + 600000 = 875000
	// - Total credits = 500000 (deposit)
	// - Net refund = 0 (net_refund_paise == 0)
	// - Receivable balance = 875000 - 500000 = 375000 (₹3,750)
	// - Zero payout items created (PayoutItem == nil)
	// - Balanced journal: Dr deposit_liability 500000, Dr tenant_receivable 375000,
	//                     Cr damages_income 600000, Cr rent_revenue 275000 (total dr 875000 == cr 875000)
	// - Tenant status = 'vacated'
	// -------------------------------------------------------------
	t.Run("Scenario 4: Negative Net Refund with Receivable Balance", func(t *testing.T) {
		tenantID := uuid.New()
		phone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+4)%10000000000)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Tenant Scenario 4', $3, 550000, 1, 'active')`, tenantID, propID, phone,
		)
		if err != nil {
			t.Fatalf("insert tenant: %v", err)
		}

		dueID := uuid.New()
		pStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		pEnd := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
		dueDate := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
		dueCode := fmt.Sprintf("D%s", uuid.New().String()[:7])
		_, err = pool.Exec(ctx, `
			INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status)
			VALUES ($1, $2, $3, $4, 'rent', 550000, 550000, $5, $6, $7, 'pending')`,
			dueID, dueCode, tenantID, propID, pStart, pEnd, dueDate,
		)
		if err != nil {
			t.Fatalf("insert due: %v", err)
		}

		depID := uuid.New()
		vacateDate := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenant_departures (id, tenant_id, property_id, notice_given_at, planned_vacate_date, deposit_amount_paise, net_refund_paise, status)
			VALUES ($1, $2, $3, NOW(), $4, 500000, 0, 'inspected')`, depID, tenantID, propID, vacateDate,
		)
		if err != nil {
			t.Fatalf("insert departure: %v", err)
		}

		// Add severe damages: ₹6,000 (600000 paise)
		ded := &domain.DepartureDeduction{
			DepartureID: depID,
			Description: "Severely broken AC unit",
			AmountPaise: 600000,
			Status:      domain.DeductionAgreed,
		}
		if err := payoutRepo.AddDeduction(ctx, ded); err != nil {
			t.Fatalf("add deduction: %v", err)
		}

		// Execute departure settlement (payeeID can even be nil since net_refund is 0)
		res, err := payoutRepo.SettleDepartureUnderLock(ctx, SettleDepartureParams{
			DepartureID:       depID,
			ActualVacateDate:  vacateDate,
			ProratedRentPaise: 275000,
			PayeeID:           nil, // No payee needed when tenant owes money!
		})
		if err != nil {
			t.Fatalf("settle departure failed: %v", err)
		}

		if res.NetRefundPaise != 0 {
			t.Errorf("expected net refund 0, got %d", res.NetRefundPaise)
		}
		if res.ReceivableBalancePaise != 375000 {
			t.Errorf("expected receivable balance 375000, got %d", res.ReceivableBalancePaise)
		}
		if res.PayoutItem != nil {
			t.Errorf("expected no payout item when net refund is 0, got %v", res.PayoutItem)
		}

		// Verify database row for departure
		var dbNetRefund, dbReceivable int64
		var dbStatus string
		err = pool.QueryRow(ctx, `SELECT net_refund_paise, receivable_balance_paise, status FROM tenant_departures WHERE id = $1`, depID).
			Scan(&dbNetRefund, &dbReceivable, &dbStatus)
		if err != nil {
			t.Fatalf("query departure: %v", err)
		}
		if dbNetRefund != 0 || dbReceivable != 375000 || dbStatus != "approved" {
			t.Errorf("db state mismatch: netRefund=%d, receivable=%d, status=%s", dbNetRefund, dbReceivable, dbStatus)
		}

		// Verify mirror journal received the receivable balance and is balanced
		var foundMirror *recordedSettlement
		for i := range mirror.settlements {
			if mirror.settlements[i].DepartureID == depID {
				foundMirror = &mirror.settlements[i]
				break
			}
		}
		if foundMirror == nil {
			t.Fatalf("mirror settlement not recorded for departure %s", depID)
		}
		if foundMirror.ReceivableBalancePaise != 375000 {
			t.Errorf("expected mirror receivable 375000, got %d", foundMirror.ReceivableBalancePaise)
		}
		// Invariant: Total Debits == Total Credits
		totalDr := foundMirror.DepositPaise + foundMirror.UnusedRentReversal + foundMirror.ReceivableBalancePaise
		totalCr := foundMirror.DamagesPaise + foundMirror.NetRefundPaise + foundMirror.ProratedRentOwedPaise
		if totalDr != totalCr {
			t.Errorf("mirror journal unbalanced! Debits: %d != Credits: %d", totalDr, totalCr)
		}
	})

	// -------------------------------------------------------------
	// REFUND ON DEPARTED DUE (Live Net Paid & SSoT Recomputation)
	// Tests Open Question 1:
	// A due has payment_allocations (550000), departure_due_adjustments (275000),
	// AND a subsequent refund_allocations (100000).
	// GetDueNetPaidPaise must compute: 550000 - 100000 - 275000 = 175000 paise.
	// Contractual ceiling = 275000 paise.
	// RecomputeDueStatusMath(175000, 275000) -> status = 'partial', remaining amount = 100000 (₹1,000).
	// -------------------------------------------------------------
	t.Run("Owner Refund on Departed Due with Simultaneous Adjustments and Refund Allocations", func(t *testing.T) {
		tenantID := uuid.New()
		phone := fmt.Sprintf("+91%010d", (time.Now().UnixNano()+5)%10000000000)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
			VALUES ($1, $2, 'Tenant Refund Departed', $3, 550000, 1, 'active')`, tenantID, propID, phone,
		)
		if err != nil {
			t.Fatalf("insert tenant: %v", err)
		}

		dueID := uuid.New()
		pStart := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		pEnd := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
		dueDate := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
		dueCode := fmt.Sprintf("D%s", uuid.New().String()[:7])
		now := time.Now().UTC()
		_, err = pool.Exec(ctx, `
			INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount, period_start, period_end, due_date, status, paid_at)
			VALUES ($1, $2, $3, $4, 'rent', 0, 550000, $5, $6, $7, 'paid', $8)`,
			dueID, dueCode, tenantID, propID, pStart, pEnd, dueDate, now,
		)
		if err != nil {
			t.Fatalf("insert due: %v", err)
		}

		// 1. Initial payment of ₹5,500
		payID := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO payments (id, tenant_id, property_id, due_id, amount, matched_by, provider, is_unapplied)
			VALUES ($1, $2, $4, $3, 550000, 'cash', 'cash', false)`, payID, tenantID, dueID, propID,
		)
		if err != nil {
			t.Fatalf("insert payment: %v", err)
		}

		// 2. Tenant departure with ₹2,750 prorated rent -> ₹2,750 unused rent reversal
		depID := uuid.New()
		vacateDate := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
		_, err = pool.Exec(ctx, `
			INSERT INTO tenant_departures (id, tenant_id, property_id, notice_given_at, planned_vacate_date, deposit_amount_paise, net_refund_paise, status)
			VALUES ($1, $2, $3, NOW(), $4, 1000000, 0, 'inspected')`, depID, tenantID, propID, vacateDate,
		)
		if err != nil {
			t.Fatalf("insert departure: %v", err)
		}

		res, err := payoutRepo.SettleDepartureUnderLock(ctx, SettleDepartureParams{
			DepartureID:       depID,
			ActualVacateDate:  vacateDate,
			ProratedRentPaise: 275000,
			PayeeID:           &payeeID,
		})
		if err != nil {
			t.Fatalf("settle departure failed: %v", err)
		}
		if res.Due.Status != domain.DueStatusPaid || *res.Due.ContractualCeilingPaise != 275000 {
			t.Fatalf("expected settled due status paid with ceiling 275000")
		}

		// Verify net paid at this point is exactly 275000
		payRepo := NewPaymentRepo(pool)
		netPaidBeforeRefund, err := payRepo.GetDueNetPaidPaise(ctx, dueID)
		if err != nil {
			t.Fatalf("GetDueNetPaidPaise before refund: %v", err)
		}
		if netPaidBeforeRefund != 275000 {
			t.Fatalf("expected net paid before refund 275000, got %d", netPaidBeforeRefund)
		}

		// 3. Now simulate owner refund of ₹1,000 on payID
		rfID := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO gateway_refunds (id, payment_id, property_id, provider, amount_paise, status, reason, source, initiated_by)
			VALUES ($1, $2, $3, 'cash', 100000, 'succeeded', 'Owner adjustment', 'owner', $4)`, rfID, payID, propID, ownerID,
		)
		if err != nil {
			t.Fatalf("insert gateway refund: %v", err)
		}

		allocID := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO refund_allocations (id, refund_id, due_id, amount_paise)
			VALUES ($1, $2, $3, 100000)`, allocID, rfID, dueID,
		)
		if err != nil {
			t.Fatalf("insert refund allocation: %v", err)
		}

		// 4. Verify GetDueNetPaidPaise now computes: 550000 - 100000 - 275000 = 175000
		netPaidAfterRefund, err := payRepo.GetDueNetPaidPaise(ctx, dueID)
		if err != nil {
			t.Fatalf("GetDueNetPaidPaise after refund: %v", err)
		}
		if netPaidAfterRefund != 175000 {
			t.Fatalf("CRITICAL: expected netPaid=175000 (550000-100000-275000), got %d", netPaidAfterRefund)
		}

		// 5. Test status recomputation against contractual ceiling (275000)
		var ceiling int64
		err = pool.QueryRow(ctx, `SELECT contractual_ceiling_paise FROM dues WHERE id = $1`, dueID).Scan(&ceiling)
		if err != nil {
			t.Fatalf("query contractual ceiling: %v", err)
		}
		if ceiling != 275000 {
			t.Fatalf("expected contractual ceiling 275000, got %d", ceiling)
		}

		newStatus, newAmount := domain.RecomputeDueStatusMath(netPaidAfterRefund, ceiling)
		if newStatus != domain.DueStatusPartial {
			t.Errorf("expected due status 'partial', got %s", newStatus)
		}
		if newAmount != 100000 {
			t.Errorf("expected due amount 100000 (₹1,000), got %d", newAmount)
		}
	})

	// -------------------------------------------------------------
	// Payout Batching and Checksum Verification
	// -------------------------------------------------------------
	t.Run("Payout Batch Creation and Checksum", func(t *testing.T) {
		unbatched, err := payoutRepo.ListUnbatchedPendingPayoutItems(ctx, propID)
		if err != nil {
			t.Fatalf("list unbatched items: %v", err)
		}
		if len(unbatched) < 3 {
			t.Fatalf("expected at least 3 unbatched items, got %d", len(unbatched))
		}

		checksumSecret := []byte("test_hmac_secret_for_batch")
		batchNum := fmt.Sprintf("BATCH-%d", time.Now().UnixNano())
		notes := "September Exit Payouts"

		batch, items, err := payoutRepo.CreateBatchFromUnbatchedItems(ctx, propID, ownerID, batchNum, checksumSecret, &notes)
		if err != nil {
			t.Fatalf("create batch failed: %v", err)
		}

		if batch.Status != domain.BatchDraft {
			t.Errorf("expected batch status 'draft', got %s", batch.Status)
		}
		if batch.ApprovedBy != nil {
			t.Errorf("expected nil approved_by on creation, got %v", batch.ApprovedBy)
		}

		// Approve batch
		approvedBatch, err := payoutRepo.ApproveBatch(ctx, batch.ID, ownerID)
		if err != nil {
			t.Fatalf("approve batch failed: %v", err)
		}
		if approvedBatch.Status != domain.BatchApproved {
			t.Errorf("expected approved batch status, got %s", approvedBatch.Status)
		}
		if approvedBatch.ApprovedBy == nil || *approvedBatch.ApprovedBy != ownerID {
			t.Errorf("expected approved_by %s, got %v", ownerID, approvedBatch.ApprovedBy)
		}
		if batch.ItemCount != len(items) {
			t.Errorf("expected item count %d, got %d", len(items), batch.ItemCount)
		}

		// Verify HMAC checksum matches domain.ComputeBatchChecksum
		expectedChecksum := domain.ComputeBatchChecksum(checksumSecret, items)
		if batch.FileChecksum == nil || *batch.FileChecksum != expectedChecksum {
			t.Errorf("checksum mismatch: expected %s, got %v", expectedChecksum, batch.FileChecksum)
		}

		// Verify no unbatched items remain
		remaining, err := payoutRepo.ListUnbatchedPendingPayoutItems(ctx, propID)
		if err != nil {
			t.Fatalf("list remaining unbatched: %v", err)
		}
		if len(remaining) != 0 {
			t.Errorf("expected 0 unbatched items, got %d", len(remaining))
		}
	})

	t.Run("Scenario_CrossProperty_Payee_Rejected", func(t *testing.T) {
		// Create second property and a payee belonging to property 2
		prop2ID := uuid.New()
		invite2 := fmt.Sprintf("DEP%s", uuid.New().String()[:8])
		_, err = pool.Exec(ctx, `INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code) VALUES ($1, 'Property 2', '456 Other Rd', '+919999988887', 'owner2@upi', 'Owner 2', 'owner2@test.com', $2)`, prop2ID, invite2)
		if err != nil {
			t.Fatalf("failed to insert prop2: %v", err)
		}
		defer func() { _, _ = pool.Exec(ctx, `DELETE FROM properties WHERE id = $1`, prop2ID) }()

		otherPayeeID := uuid.New()
		payeeHash2 := domain.ComputeAccountHash([]byte("test_salt"), "9876543211@upi")
		_, err = pool.Exec(ctx, `
			INSERT INTO payout_payees (id, property_id, payee_type, name, phone, account_number_hash, upi_vpa, is_verified, created_at, updated_at)
			VALUES ($1, $2, 'tenant_deposit', 'Other Property Tenant', '9876543211', $3, '9876543211@upi', true, NOW(), NOW())`,
			otherPayeeID, prop2ID, payeeHash2)
		if err != nil {
			t.Fatalf("failed to insert other payee: %v", err)
		}
		defer func() { _, _ = pool.Exec(ctx, `DELETE FROM payout_payees WHERE id = $1`, otherPayeeID) }()

		// Create tenant and departure on propID
		tenantID := uuid.New()
		tenantPhone := fmt.Sprintf("+91%010d", time.Now().UnixNano()%10000000000)
		_, err = pool.Exec(ctx, `INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status, created_at, updated_at) VALUES ($1, $2, 'Departing Tenant CP', $3, 10000, 1, 'active', NOW(), NOW())`, tenantID, propID, tenantPhone)
		if err != nil {
			t.Fatalf("failed to insert tenant: %v", err)
		}
		defer func() { _, _ = pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID) }()

		dep := &domain.TenantDeparture{
			TenantID:           tenantID,
			PropertyID:         propID,
			NoticeGivenAt:      time.Now().AddDate(0, 0, -30),
			PlannedVacateDate:  time.Now(),
			DepositAmountPaise: 50_000_00, // ₹50,000 refund due
		}
		err = payoutRepo.CreateDeparture(ctx, dep)
		if err != nil {
			t.Fatalf("create departure failed: %v", err)
		}
		defer func() { _, _ = pool.Exec(ctx, `DELETE FROM tenant_departures WHERE id = $1`, dep.ID) }()

		// Attempt settlement using otherPayeeID from prop2ID
		_, err = payoutRepo.SettleDepartureUnderLock(ctx, SettleDepartureParams{
			DepartureID:       dep.ID,
			ActualVacateDate:  time.Now(),
			ProratedRentPaise: 0,
			PayeeID:           &otherPayeeID,
		})
		if err == nil {
			t.Fatalf("expected cross-property payee to be rejected, but settlement succeeded")
		}
		if !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("expected ErrForbidden for cross-property payee, got: %v", err)
		}
	})
}
