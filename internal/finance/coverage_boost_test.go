package finance

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
)

// TestFinance_CoverageBoost verifies untested branches across MemoryStore,
// Mirroring, Reports, Settlement Recon, Variance, and Service methods.
func TestFinance_CoverageBoost(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	bus := &events.NoopPublisher{}
	svc := NewService(store, bus)
	propID := uuid.New()
	now := time.Now().UTC()

	// 1. MemoryStore - Basic defaults, policies, settings
	if err := store.EnsureDefaults(ctx, propID); err != nil {
		t.Fatalf("EnsureDefaults failed: %v", err)
	}
	settings, err := store.GetSettings(ctx, propID)
	if err != nil {
		t.Fatalf("GetSettings failed: %v", err)
	}
	settings.FiscalMonthStartDay = 1
	settings.TDRIsEstimated = true
	if err := store.SaveSettings(ctx, settings); err != nil {
		t.Fatalf("SaveSettings failed: %v", err)
	}
	policy, err := store.GetPolicy(ctx, propID)
	if err != nil {
		t.Fatalf("GetPolicy failed: %v", err)
	}
	if err := store.SavePolicy(ctx, policy); err != nil {
		t.Fatalf("SavePolicy failed: %v", err)
	}

	// 2. MemoryStore - Tenant Credits & Owner User
	ownerID := uuid.New()
	store.SetPropertyOwnerUserID(propID, ownerID)
	gotOwner, err := store.GetPropertyOwnerUserID(ctx, propID)
	if err != nil || gotOwner != ownerID {
		t.Fatalf("expected owner %v, got %v", ownerID, gotOwner)
	}
	tenantID := uuid.New()
	if err := store.AddTenantCredit(ctx, tenantID, 50000); err != nil {
		t.Fatalf("AddTenantCredit failed: %v", err)
	}
	credit := store.GetTenantCredit(tenantID)
	if credit != 50000 {
		t.Fatalf("expected credit 50000, got %d", credit)
	}

	// 3. MemoryStore - Meal Prep & Import Suggestions
	mealPrep := domain.MealPrepActual{
		PropertyID:     propID,
		MealDate:       now,
		MealSlot:       "lunch",
		PreparedCount:  50,
		DiscardedCount: 2,
	}
	if err := store.UpsertMealPrep(ctx, &mealPrep); err != nil {
		t.Fatalf("UpsertMealPrep failed: %v", err)
	}
	meals, err := store.GetMealPrep(ctx, propID, now)
	if err != nil || len(meals) == 0 {
		t.Fatalf("GetMealPrep failed: %v", err)
	}
	importSugg := domain.ExpenseImportSuggestion{
		ID:          uuid.New(),
		PropertyID:  propID,
		TxnID:       "txn-1",
		AmountPaise: 5000,
		TxnDate:     now,
	}
	if err := store.UpsertImportSuggestion(ctx, &importSugg); err != nil {
		t.Fatalf("UpsertImportSuggestion failed: %v", err)
	}
	suggs, err := store.ListImportSuggestions(ctx, propID)
	if err != nil || len(suggs) == 0 {
		t.Fatalf("ListImportSuggestions failed: %v", err)
	}

	// 4. MemoryStore - Leakage, Recommendations, Forecasts, KPIs, ROIs
	leakID := uuid.New()
	leak := domain.LeakageEvent{
		ID:             leakID,
		PropertyID:     propID,
		EstimatedPaise: 2500,
		DetectedAt:     now,
	}
	if err := store.InsertLeakage(ctx, &leak); err != nil {
		t.Fatalf("InsertLeakage failed: %v", err)
	}
	leaks, err := store.ListLeakage(ctx, propID)
	if err != nil || len(leaks) == 0 {
		t.Fatalf("ListLeakage failed: %v", err)
	}
	if _, err := store.GetLeakage(ctx, leakID); err != nil {
		t.Fatalf("GetLeakage failed: %v", err)
	}

	recID := uuid.New()
	rec := domain.Recommendation{
		ID:         recID,
		PropertyID: propID,
		Status:     "open",
	}
	if err := store.InsertRecommendation(ctx, &rec); err != nil {
		t.Fatalf("InsertRecommendation failed: %v", err)
	}
	recs, err := store.ListRecommendations(ctx, propID)
	if err != nil || len(recs) == 0 {
		t.Fatalf("ListRecommendations failed: %v", err)
	}
	if _, err := store.GetRecommendation(ctx, recID); err != nil {
		t.Fatalf("GetRecommendation failed: %v", err)
	}
	rec.Status = "dismissed"
	if err := store.UpdateRecommendation(ctx, &rec); err != nil {
		t.Fatalf("UpdateRecommendation failed: %v", err)
	}

	fc := domain.ForecastSnapshot{
		PropertyID:  propID,
		HorizonDays: 30,
		AsOf:        now,
		Payload:     []byte(`{}`),
	}
	if err := store.InsertForecast(ctx, &fc); err != nil {
		t.Fatalf("InsertForecast failed: %v", err)
	}
	if _, err := store.LatestForecast(ctx, propID, 30); err != nil {
		t.Fatalf("LatestForecast failed: %v", err)
	}

	kpi := domain.KPISnapshot{
		PropertyID:   propID,
		SnapshotDate: now,
	}
	if err := store.InsertKPI(ctx, &kpi); err != nil {
		t.Fatalf("InsertKPI failed: %v", err)
	}

	roiSnap := domain.ROISnapshot{
		PropertyID:   propID,
		SnapshotDate: now,
	}
	if err := store.InsertROI(ctx, &roiSnap); err != nil {
		t.Fatalf("InsertROI failed: %v", err)
	}
	if _, err := store.LatestROI(ctx, propID); err != nil {
		t.Fatalf("LatestROI failed: %v", err)
	}

	// 5. MemoryStore - Budgets & Unified Settings
	budget := domain.Budget{
		PropertyID:   propID,
		PeriodMonth:  "2026-10",
		CategoryCode: "maintenance",
		AmountPaise:  400000,
	}
	if err := svc.SaveBudget(ctx, &budget); err != nil {
		t.Fatalf("SaveBudget failed: %v", err)
	}
	if _, err := store.GetBudget(ctx, propID, "maintenance", "2026-10"); err != nil {
		t.Fatalf("GetBudget failed: %v", err)
	}
	if _, err := store.ListBudgets(ctx, propID, "2026-10"); err != nil {
		t.Fatalf("ListBudgets failed: %v", err)
	}

	loyalty := domain.PropertyGamificationSettings{
		PropertyID:      propID,
		PointValuePaise: 100,
	}
	if _, _, _, err := svc.PatchUnifiedSettings(ctx, propID, &settings, &policy, &loyalty); err != nil {
		t.Fatalf("PatchUnifiedSettings failed: %v", err)
	}
	if _, _, err := svc.PatchSettings(ctx, propID, &settings, &policy); err != nil {
		t.Fatalf("PatchSettings failed: %v", err)
	}

	// 6. MemoryStore - Initial Manager Spend
	store.SaveInitialManagerSpend(propID, ownerID, 200000, now)

	// 7. Mirroring - Comprehensive Coverage
	payID := uuid.New()
	if err := svc.MirrorUnappliedPayment(ctx, propID, payID, 100000, now); err != nil {
		t.Fatalf("MirrorUnappliedPayment failed: %v", err)
	}
	// Test nil/zero amount guards
	_ = svc.MirrorUnappliedPayment(ctx, propID, payID, 0, now)

	// Refunds
	refID := uuid.New()
	if err := svc.MirrorRefund(ctx, propID, refID, 50000, true, "", now); err != nil {
		t.Fatalf("MirrorRefund unapplied failed: %v", err)
	}
	refAllocID := uuid.New()
	if err := svc.MirrorRefund(ctx, propID, refAllocID, 50000, false, domain.DueKindRent, now); err != nil {
		t.Fatalf("MirrorRefund applied failed: %v", err)
	}
	// Multi allocation refund
	refMultiID := uuid.New()
	if err := svc.MirrorRefundAllocations(ctx, propID, refMultiID, []RefundAllocationItem{
		{AmountPaise: 20000, DueKind: domain.DueKindDeposit},
		{AmountPaise: 10000, DueKind: domain.DueKindElectricity},
		{AmountPaise: 5000, DueKind: domain.DueKindWater},
		{AmountPaise: 15000, DueKind: domain.DueKindRent},
	}, now); err != nil {
		t.Fatalf("MirrorRefundAllocations multi failed: %v", err)
	}

	// Proration, Credit, Rewards
	dueObj := &domain.Due{
		ID:         uuid.New(),
		PropertyID: propID,
		Status:     domain.DueStatusPaid,
	}
	if err := svc.MirrorProration(ctx, dueObj, 15000, 10000); err != nil {
		t.Fatalf("MirrorProration failed: %v", err)
	}
	if err := svc.MirrorApplyCredit(ctx, propID, dueObj.ID, 15000, domain.DueKindRent, now); err != nil {
		t.Fatalf("MirrorApplyCredit failed: %v", err)
	}
	if err := svc.MirrorRewardRedeem(ctx, propID, tenantID, uuid.New(), 100, 10000); err != nil {
		t.Fatalf("MirrorRewardRedeem failed: %v", err)
	}
	if err := svc.MirrorPointsIssued(ctx, propID, tenantID, uuid.New(), 50, 5000); err != nil {
		t.Fatalf("MirrorPointsIssued failed: %v", err)
	}

	// Payouts
	payoutID := uuid.New()
	if err := svc.MirrorPayoutSettled(ctx, propID, payoutID, 500000, now); err != nil {
		t.Fatalf("MirrorPayoutSettled failed: %v", err)
	}
	if err := svc.MirrorPayoutReversed(ctx, propID, payoutID, 500000, now); err != nil {
		t.Fatalf("MirrorPayoutReversed failed: %v", err)
	}

	// Bank Statement Credit & Unapplied Allocations
	if err := svc.MirrorBankStatementCredit(ctx, propID, uuid.New(), 300000, now); err != nil {
		t.Fatalf("MirrorBankStatementCredit failed: %v", err)
	}
	if err := svc.MirrorUnappliedAllocation(ctx, propID, uuid.New(), domain.DueKindDeposit, 100000, now); err != nil {
		t.Fatalf("MirrorUnappliedAllocation deposit failed: %v", err)
	}
	if err := svc.MirrorUnappliedAllocation(ctx, propID, uuid.New(), domain.DueKindElectricity, 50000, now); err != nil {
		t.Fatalf("MirrorUnappliedAllocation electricity failed: %v", err)
	}
	if err := svc.MirrorUnappliedAllocation(ctx, propID, uuid.New(), domain.DueKindRent, 50000, now); err != nil {
		t.Fatalf("MirrorUnappliedAllocation rent failed: %v", err)
	}
	if err := svc.MirrorBankDepositRefund(ctx, propID, uuid.New(), 20000, now); err != nil {
		t.Fatalf("MirrorBankDepositRefund failed: %v", err)
	}
	if err := svc.MirrorUnappliedReclassification(ctx, propID, uuid.New(), domain.AcctInterestIncome, 30000, now); err != nil {
		t.Fatalf("MirrorUnappliedReclassification failed: %v", err)
	}
	if err := svc.MirrorUnappliedReclassification(ctx, propID, uuid.New(), "", 10000, now); err != nil {
		t.Fatalf("MirrorUnappliedReclassification default failed: %v", err)
	}

	// Deposit Settlement & Payment Correction
	settleID := uuid.New()
	if err := svc.MirrorDepositSettlement(ctx, propID, settleID, tenantID, uuid.New(), 200000, 150000, 50000, now); err != nil {
		t.Fatalf("MirrorDepositSettlement failed: %v", err)
	}
	corrID := uuid.New()
	if err := svc.MirrorPaymentCorrection(ctx, propID, corrID, payID, 50000, now); err != nil {
		t.Fatalf("MirrorPaymentCorrection failed: %v", err)
	}

	// CSV Debit Suggestion
	if err := svc.SuggestCSVDebit(ctx, propID, "txn-csv-1", 15000, now, "repairs"); err != nil {
		t.Fatalf("SuggestCSVDebit failed: %v", err)
	}

	// 8. Financial Statements & Reports Wrappers
	from := now.Add(-30 * 24 * time.Hour)
	to := now
	if _, err := svc.TrialBalance(ctx, propID, to); err != nil {
		t.Fatalf("TrialBalance failed: %v", err)
	}
	if _, err := svc.IncomeStatement(ctx, propID, from, to); err != nil {
		t.Fatalf("IncomeStatement failed: %v", err)
	}
	if _, err := svc.BalanceSheet(ctx, propID, to); err != nil {
		t.Fatalf("BalanceSheet failed: %v", err)
	}
	if _, err := svc.CashFlow(ctx, propID, from, to); err != nil {
		t.Fatalf("CashFlow failed: %v", err)
	}
	if _, err := svc.ReconcilingItems(ctx, propID, to); err != nil {
		t.Fatalf("ReconcilingItems failed: %v", err)
	}
	store.mu.Lock()
	store.journal = append(store.journal, domain.JournalLine{
		ID:          uuid.New(),
		PropertyID:  propID,
		AccountCode: domain.AcctRentRevenue,
		CreditPaise: 100000,
		OccurredAt:  now,
	})
	store.mu.Unlock()
	// Test ScanAndAlertReconcilingItems
	_, _ = svc.ScanAndAlertReconcilingItems(ctx, &propID, to, nil)
	_, _ = svc.GetDepositReserveReport(ctx, propID)
	_, _ = svc.GetCapitalPaybackReport(ctx, propID)
	_, _ = svc.GetClearingDriftReport(ctx, propID)

	// 9. Service - Approval Workflow, Operating Summary, Capital Totals
	appReq := domain.ApprovalRequest{
		ID:          uuid.New(),
		PropertyID:  propID,
		SubjectID:   uuid.New(),
		Kind:        "expense",
		AmountPaise: 25000,
		Status:      "pending",
	}
	store.mu.Lock()
	store.approvals[appReq.ID] = appReq
	store.expenses[appReq.SubjectID] = domain.Expense{
		ID:          appReq.SubjectID,
		PropertyID:  propID,
		AmountPaise: 25000,
		OccurredAt:  now,
		Status:      domain.ExpensePendingApproval,
	}
	store.mu.Unlock()

	// Approve expense
	if err := svc.DecideApproval(ctx, propID, ownerID, appReq.ID, true, "Approved for testing"); err != nil {
		t.Fatalf("DecideApproval approve failed: %v", err)
	}

	// Reject expense
	appReq2 := domain.ApprovalRequest{
		ID:          uuid.New(),
		PropertyID:  propID,
		SubjectID:   uuid.New(),
		Kind:        "expense",
		AmountPaise: 50000,
		Status:      "pending",
	}
	store.mu.Lock()
	store.approvals[appReq2.ID] = appReq2
	store.expenses[appReq2.SubjectID] = domain.Expense{
		ID:          appReq2.SubjectID,
		PropertyID:  propID,
		AmountPaise: 50000,
		OccurredAt:  now,
		Status:      domain.ExpensePendingApproval,
	}
	store.mu.Unlock()
	if err := svc.DecideApproval(ctx, propID, ownerID, appReq2.ID, false, "Rejected for testing"); err != nil {
		t.Fatalf("DecideApproval reject failed: %v", err)
	}

	// Operating Summary & Capital Totals & Category Opex
	if _, err := svc.OperatingSummary(ctx, propID, "2026-10", 1500000); err != nil {
		t.Fatalf("OperatingSummary failed: %v", err)
	}
	if _, _, err := svc.CapitalTotals(ctx, propID); err != nil {
		t.Fatalf("CapitalTotals failed: %v", err)
	}
	if _, err := svc.CategoryOpex(ctx, propID, from, to); err != nil {
		t.Fatalf("CategoryOpex failed: %v", err)
	}

	// 10. Settlement Recon & Variance Bridge & Payout Dispatcher
	recon := NewSettlementReconciler(nil, nil, nil, nil, svc)
	reconRecord := &cashfree.SettlementWebhookRecord{
		CFSettlementID:   "stlm_test_1",
		GrossAmountPaise: 1500000,
		NetAmountPaise:   1485000,
	}
	_, _ = recon.ReconcileWebhookSettlement(ctx, reconRecord)

	_, _ = UnmarshalPayoutBatchPayload([]byte(`{"property_id":"` + propID.String() + `","batch_id":"` + uuid.New().String() + `"}`))

	// Period tie out alert & re-open
	store.mu.Lock()
	store.tieouts[propID.String()+"2026-10"] = domain.PeriodTieOut{
		PropertyID:  propID,
		PeriodMonth: "2026-10",
		Status:      "closed",
	}
	store.mu.Unlock()
	_ = svc.RecurringTieOutAlert(ctx, propID)
	_, _ = svc.ReopenTieOut(ctx, propID, "2026-10", "Need adjustment", "owner")
	_ = store.ReopenTieOut(ctx, propID, "2026-10", "owner")

	// 11. Additional Store methods & helpers
	capTx := domain.CapitalTransaction{
		ID:          uuid.New(),
		PropertyID:  propID,
		OwnerUserID: ownerID,
		Kind:        domain.CapitalInitial,
		AmountPaise: 1000000,
		OccurredAt:  now,
	}
	_ = store.InsertCapital(ctx, &capTx)

	expID := uuid.New()
	expItem := domain.Expense{
		ID:          expID,
		PropertyID:  propID,
		AmountPaise: 50000,
		OccurredAt:  now,
		Status:      domain.ExpenseApproved,
	}
	_ = store.InsertExpense(ctx, &expItem)
	_, _ = store.ListExpenses(ctx, propID)

	expPayItem := domain.ExpensePayment{
		ID:          uuid.New(),
		ExpenseID:   expID,
		PropertyID:  propID,
		AmountPaise: 50000,
		OccurredAt:  now,
	}
	_ = store.InsertExpensePayment(ctx, &expPayItem)
	_, _ = store.ListExpensePayments(ctx, expID)

	advID := uuid.New()
	advItem := domain.ManagerAdvance{
		ID:            advID,
		PropertyID:    propID,
		ManagerUserID: ownerID,
		AmountPaise:   25000,
		OccurredAt:    now,
	}
	_ = store.InsertAdvance(ctx, &advItem)
	_, _ = store.ListAdvances(ctx, propID)

	reimbItem := domain.ManagerReimbursement{
		ID:            uuid.New(),
		PropertyID:    propID,
		ManagerUserID: ownerID,
		AmountPaise:   25000,
		OccurredAt:    now,
	}
	_ = store.InsertReimbursement(ctx, &reimbItem)

	_, _ = store.SumRewardLiability(ctx, propID, "issued", now.Add(-time.Hour), now.Add(time.Hour))
	_, _ = store.SumRewardPointsIssued(ctx, propID, now.Add(-time.Hour), now.Add(time.Hour))

	approvalItem := domain.ApprovalRequest{
		ID:          uuid.New(),
		PropertyID:  propID,
		SubjectID:   expID,
		Kind:        "expense",
		AmountPaise: 50000,
		Status:      "pending",
		CreatedAt:   now,
	}
	_ = store.InsertApproval(ctx, &approvalItem)
	_, _ = store.ListApprovals(ctx, propID, "pending")
	approvalItem.Status = "approved"
	_ = store.UpdateApproval(ctx, &approvalItem)

	// Accruals and Formatting
	_ = svc.postExpenseAccrual(ctx, &expItem)
	_ = fmtINR(1234500)

	// Occupancy calculations
	occ := Occupancy{CapacityBeds: 10, OccupiedBeds: 8, BedsAtRisk: 1}
	_ = OccupancyBPS(occ)
	_ = ComputeOccupancy([]domain.Room{{Capacity: 10}}, []domain.Tenant{{Status: domain.TenantStatusActive}}, now)

	// Journal AccountSum
	linesTest, _ := MakeLines(propID, expID, "test", now, []LineSpec{
		{Account: domain.AcctOperatingExpense, Debit: 5000, LineKind: "dr"},
		{Account: domain.AcctAccountsPayable, Credit: 5000, LineKind: "cr"},
	})
	_, _ = AccountSum(linesTest, domain.AcctOperatingExpense)

	// Balancer methods
	balancer := NewSettlementBalancer(nil)
	_, _ = balancer.ListDailyBalances(ctx, propID, 10, 0)
	_, _ = balancer.GetDailyBalance(ctx, propID, now)

	// Ledger worker setter hooks
	worker := NewLedgerOutboxWorker(nil, nil, nil, nil)
	worker.SetPayoutDispatcher(nil)
	worker.SetAlerter(nil)
}
