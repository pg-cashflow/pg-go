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
	if p.MatchedBy == domain.MatchedByCashfree {
		st, _ := s.Store.GetSettings(ctx, due.PropertyID)
		if st.TDREffectiveBPS > 0 {
			tdr := amt * int64(st.TDREffectiveBPS) / 10000
			if tdr > 0 {
				tdrLines, err := MakeLines(due.PropertyID, p.ID, "tdr_estimate", at, []LineSpec{
					{Account: domain.AcctPaymentProcessingExpense, Debit: tdr, LineKind: "tdr_dr"},
					{Account: domain.AcctBank, Credit: tdr, LineKind: "tdr_cr"},
				})
				if err == nil {
					_ = s.Store.InsertJournal(ctx, tdrLines)
				}
			}
		}
	}
	return nil
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
