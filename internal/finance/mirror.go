package finance

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// MirrorPayment posts collection journal lines. Rent is recognized on collection.
func (s *Service) MirrorPayment(ctx context.Context, p *domain.Payment, due *domain.Due) error {
	if s == nil || s.Store == nil || p == nil || due == nil {
		return nil
	}
	amt := int64(p.Amount)
	if amt <= 0 {
		return nil
	}
	at := p.MatchedAt
	if at.IsZero() {
		at = s.Now()
	}
	cashAcct := domain.AcctBank
	if p.MatchedBy == domain.MatchedByCash {
		cashAcct = domain.AcctCash
	} else if p.MatchedBy == domain.MatchedByCashfree {
		cashAcct = domain.AcctGatewayClearing
	}
	var specs []LineSpec
	switch due.Kind {
	case domain.DueKindDeposit:
		specs = []LineSpec{
			{Account: cashAcct, Debit: amt, LineKind: "cash_in"},
			{Account: domain.AcctDepositLiability, Credit: amt, LineKind: "deposit_liability"},
		}
	case domain.DueKindElectricity, domain.DueKindWater:
		specs = []LineSpec{
			{Account: cashAcct, Debit: amt, LineKind: "cash_in"},
			{Account: domain.AcctUtilityRecoveryRevenue, Credit: amt, LineKind: "utility_recovery"},
		}
	default:
		specs = []LineSpec{
			{Account: cashAcct, Debit: amt, LineKind: "cash_in"},
			{Account: domain.AcctRentRevenue, Credit: amt, LineKind: "rent_collected"},
		}
	}
	lines, err := MakeLines(due.PropertyID, p.ID, "payment", at, specs)
	if err != nil {
		return err
	}
	err = s.Store.InsertJournal(ctx, lines)
	if err == ErrDuplicateIdempotency {
		return nil
	}
	if err != nil {
		return err
	}
	return nil
}

// MirrorUnappliedPayment posts unapplied payment lines: Dr gateway_clearing, Cr unapplied_receipts.
func (s *Service) MirrorUnappliedPayment(ctx context.Context, propertyID, paymentID uuid.UUID, amountPaise int64, at time.Time) error {
	if s == nil || s.Store == nil || amountPaise <= 0 {
		return nil
	}
	if at.IsZero() {
		at = s.Now()
	}
	lines, err := MakeLines(propertyID, paymentID, "unapplied_payment", at, []LineSpec{
		{Account: domain.AcctGatewayClearing, Debit: amountPaise, LineKind: "gateway_clearing_dr"},
		{Account: domain.AcctUnappliedReceipts, Credit: amountPaise, LineKind: "unapplied_receipt_cr"},
	})
	if err != nil {
		return err
	}
	err = s.Store.InsertJournal(ctx, lines)
	if err == ErrDuplicateIdempotency {
		return nil
	}
	return err
}

// MirrorRefund posts refund reversal lines: Dr unapplied_receipts/rent_revenue/deposit_liability, Cr gateway_clearing.
func (s *Service) MirrorRefund(ctx context.Context, propertyID, refundID uuid.UUID, amountPaise int64, isUnapplied bool, dueKind domain.DueKind, at time.Time) error {
	if s == nil || s.Store == nil || amountPaise <= 0 {
		return nil
	}
	if at.IsZero() {
		at = s.Now()
	}
	var drAccount string
	if isUnapplied {
		drAccount = domain.AcctUnappliedReceipts
	} else {
		switch dueKind {
		case domain.DueKindDeposit:
			drAccount = domain.AcctDepositLiability
		case domain.DueKindElectricity, domain.DueKindWater:
			drAccount = domain.AcctUtilityRecoveryRevenue
		default:
			drAccount = domain.AcctRentRevenue
		}
	}
	lines, err := MakeLines(propertyID, refundID, "refund", at, []LineSpec{
		{Account: drAccount, Debit: amountPaise, LineKind: "refund_reversal_dr"},
		{Account: domain.AcctGatewayClearing, Credit: amountPaise, LineKind: "gateway_clearing_cr"},
	})
	if err != nil {
		return err
	}
	err = s.Store.InsertJournal(ctx, lines)
	if err == ErrDuplicateIdempotency {
		return nil
	}
	return err
}

func (s *Service) MirrorProration(ctx context.Context, due *domain.Due, original, prorated int64) error {
	if s == nil || due == nil {
		return nil
	}
	diff := original - prorated
	if diff <= 0 {
		return nil
	}
	lines, err := MakeLines(due.PropertyID, due.ID, "proration", s.Now(), []LineSpec{
		{Account: domain.AcctRentRevenue, Debit: diff, LineKind: "proration_dr"},
		{Account: domain.AcctTenantReceivable, Credit: diff, LineKind: "proration_cr"},
	})
	if err != nil {
		return err
	}
	err = s.Store.InsertJournal(ctx, lines)
	if err == ErrDuplicateIdempotency {
		return nil
	}
	return err
}

func (s *Service) MirrorRewardRedeem(ctx context.Context, propertyID, tenantID, redemptionID uuid.UUID, points int, amountPaise int64) error {
	if s == nil || amountPaise <= 0 {
		return nil
	}
	at := s.Now()
	txn := &domain.RewardLiabilityTxn{
		ID:          uuid.New(),
		PropertyID:  propertyID,
		TenantID:    &tenantID,
		Kind:        "redeemed",
		Points:      points,
		AmountPaise: amountPaise,
		SourceType:  "redemption",
		SourceID:    redemptionID,
		OccurredAt:  at,
	}
	if err := s.Store.InsertRewardLiability(ctx, txn); err != nil && err != ErrDuplicateIdempotency {
		return err
	}
	lines, err := MakeLines(propertyID, redemptionID, "reward_redeem", at, []LineSpec{
		{Account: domain.AcctRewardLiability, Debit: amountPaise, LineKind: "liability_release"},
		{Account: domain.AcctTenantReceivable, Credit: amountPaise, LineKind: "credit_applied"},
	})
	if err != nil {
		return err
	}
	err = s.Store.InsertJournal(ctx, lines)
	if err == ErrDuplicateIdempotency {
		return nil
	}
	return err
}

func (s *Service) MirrorPointsIssued(ctx context.Context, propertyID, tenantID, ledgerID uuid.UUID, points int, amountPaise int64) error {
	if s == nil || amountPaise <= 0 {
		return nil
	}
	at := s.Now()
	txn := &domain.RewardLiabilityTxn{
		ID:          uuid.New(),
		PropertyID:  propertyID,
		TenantID:    &tenantID,
		Kind:        "issued",
		Points:      points,
		AmountPaise: amountPaise,
		SourceType:  "points_ledger",
		SourceID:    ledgerID,
		OccurredAt:  at,
	}
	if err := s.Store.InsertRewardLiability(ctx, txn); err != nil && err != ErrDuplicateIdempotency {
		return err
	}
	lines, err := MakeLines(propertyID, ledgerID, "reward_issue", at, []LineSpec{
		{Account: domain.AcctLoyaltyExpense, Debit: amountPaise, LineKind: "loyalty_dr"},
		{Account: domain.AcctRewardLiability, Credit: amountPaise, LineKind: "liability_cr"},
	})
	if err != nil {
		return err
	}
	err = s.Store.InsertJournal(ctx, lines)
	if err == ErrDuplicateIdempotency {
		return nil
	}
	return err
}

func (s *Service) SuggestCSVDebit(ctx context.Context, propertyID uuid.UUID, txnID string, amount int, date time.Time, note string) error {
	sug := &domain.ExpenseImportSuggestion{
		PropertyID:  propertyID,
		TxnID:       txnID,
		AmountPaise: int64(amount),
		TxnDate:     date,
		Note:        note,
		Status:      "pending",
	}
	return s.Store.UpsertImportSuggestion(ctx, sug)
}

// MirrorDepartureSettlement posts balanced journal entries for tenant departure settlement:
// Dr deposit_liability (depositPaise)
// Dr rent_revenue (unusedRentReversal, if > 0)
// Dr tenant_receivable (receivableBalancePaise, if > 0)
// Cr rent_revenue (proratedRentOwedPaise, if unpaid cycle netted from deposit)
// Cr damages_income (damagesPaise, if > 0)
// Cr refund_payable (netRefundPaise, if > 0)
func (s *Service) MirrorDepartureSettlement(
	ctx context.Context,
	propertyID, departureID uuid.UUID,
	depositPaise, unusedRentReversal, damagesPaise, netRefundPaise, proratedRentOwedPaise, receivableBalancePaise int64,
	at time.Time,
) error {
	if s == nil || s.Store == nil {
		return nil
	}
	if at.IsZero() {
		at = s.Now()
	}

	var specs []LineSpec
	if depositPaise > 0 {
		specs = append(specs, LineSpec{Account: domain.AcctDepositLiability, Debit: depositPaise, LineKind: "deposit_release"})
	}
	if unusedRentReversal > 0 {
		specs = append(specs, LineSpec{Account: domain.AcctRentRevenue, Debit: unusedRentReversal, LineKind: "unearned_rent_reversal"})
	}
	if receivableBalancePaise > 0 {
		specs = append(specs, LineSpec{Account: domain.AcctTenantReceivable, Debit: receivableBalancePaise, LineKind: "tenant_receivable"})
	}
	if proratedRentOwedPaise > 0 {
		specs = append(specs, LineSpec{Account: domain.AcctRentRevenue, Credit: proratedRentOwedPaise, LineKind: "prorated_rent_earned"})
	}
	if damagesPaise > 0 {
		specs = append(specs, LineSpec{Account: domain.AcctDamagesIncome, Credit: damagesPaise, LineKind: "damages_recovery"})
	}
	if netRefundPaise > 0 {
		specs = append(specs, LineSpec{Account: domain.AcctRefundPayable, Credit: netRefundPaise, LineKind: "tenant_refund_payable"})
	}

	lines, err := MakeLines(propertyID, departureID, "departure_settlement", at, specs)
	if err != nil {
		return err
	}
	err = s.Store.InsertJournal(ctx, lines)
	if err == ErrDuplicateIdempotency {
		return nil
	}
	return err
}

// MirrorPayoutSettled posts journal entry when a refund_payable payout item is settled from the bank account:
// Dr refund_payable (amountPaise)
// Cr bank (amountPaise)
func (s *Service) MirrorPayoutSettled(ctx context.Context, propertyID, payoutItemID uuid.UUID, amountPaise int64, at time.Time) error {
	if s == nil || s.Store == nil || amountPaise <= 0 {
		return nil
	}
	if at.IsZero() {
		at = s.Now()
	}
	lines, err := MakeLines(propertyID, payoutItemID, "payout_settlement", at, []LineSpec{
		{Account: domain.AcctRefundPayable, Debit: amountPaise, LineKind: "refund_paid"},
		{Account: domain.AcctBank, Credit: amountPaise, LineKind: "cash_out"},
	})
	if err != nil {
		return err
	}
	err = s.Store.InsertJournal(ctx, lines)
	if err == ErrDuplicateIdempotency {
		return nil
	}
	return err
}

