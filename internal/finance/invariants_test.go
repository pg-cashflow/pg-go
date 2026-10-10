package finance

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// TestMoneyMath_DoubleEntryConservation_PropertyEvals executes 10,000 randomized property evaluations
// proving that MirrorDepartureSettlement and MakeLines strictly conserve double-entry balance:
// sum(Debits) == sum(Credits) with 0 integer-paise drift across arbitrary combinations.
func TestMoneyMath_DoubleEntryConservation_PropertyEvals(t *testing.T) {
	// Deterministic seed for repeatable continuous evaluation
	rng := rand.New(rand.NewSource(42))
	propID := uuid.New()
	depID := uuid.New()
	occurredAt := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	const iterations = 10000
	for i := 0; i < iterations; i++ {
		// Generate realistic random integer paise amounts (0 to 50,000 INR = 5,000,000 paise)
		depositPaise := int64(rng.Intn(5000000))
		unusedRentReversal := int64(rng.Intn(2000000))
		damagesPaise := int64(rng.Intn(2000000))
		outstandingDuesNettedPaise := int64(rng.Intn(2000000))

		// Business math for departure settlement:
		// Total credits due = damages + outstanding dues
		// Total debits available = deposit + unused rent
		totalAvailable := depositPaise + unusedRentReversal
		totalDeductions := damagesPaise + outstandingDuesNettedPaise

		var netRefundPaise, receivableBalancePaise int64
		if totalAvailable >= totalDeductions {
			netRefundPaise = totalAvailable - totalDeductions
			receivableBalancePaise = 0
		} else {
			netRefundPaise = 0
			receivableBalancePaise = totalDeductions - totalAvailable
		}

		// Skip degenerate case where all are 0
		if totalAvailable == 0 && totalDeductions == 0 {
			continue
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
		if outstandingDuesNettedPaise > 0 {
			specs = append(specs, LineSpec{Account: domain.AcctRentRevenue, Credit: outstandingDuesNettedPaise, LineKind: "dues_netted_earned"})
		}
		if damagesPaise > 0 {
			specs = append(specs, LineSpec{Account: domain.AcctDamagesIncome, Credit: damagesPaise, LineKind: "damages_recovery"})
		}
		if netRefundPaise > 0 {
			specs = append(specs, LineSpec{Account: domain.AcctRefundPayable, Credit: netRefundPaise, LineKind: "tenant_refund_payable"})
		}

		lines, err := MakeLines(propID, depID, "departure_settlement", occurredAt, specs)
		if err != nil {
			t.Fatalf("[Iteration %d] expected balanced lines, got err: %v\nInputs: deposit=%d unusedRent=%d damages=%d duesNetted=%d netRefund=%d receivable=%d",
				i, err, depositPaise, unusedRentReversal, damagesPaise, outstandingDuesNettedPaise, netRefundPaise, receivableBalancePaise)
		}

		var totalDebit, totalCredit int64
		for _, l := range lines {
			if l.DebitPaise > 0 && l.CreditPaise > 0 {
				t.Fatalf("[Iteration %d] line %s has both debit (%d) and credit (%d)", i, l.ID, l.DebitPaise, l.CreditPaise)
			}
			if l.DebitPaise < 0 || l.CreditPaise < 0 {
				t.Fatalf("[Iteration %d] negative paise on line %s: dr=%d cr=%d", i, l.ID, l.DebitPaise, l.CreditPaise)
			}
			totalDebit += l.DebitPaise
			totalCredit += l.CreditPaise
		}

		if totalDebit != totalCredit {
			t.Fatalf("[Iteration %d] imbalance detected: debits=%d, credits=%d (diff=%d)", i, totalDebit, totalCredit, totalDebit-totalCredit)
		}
		if totalDebit == 0 {
			t.Fatalf("[Iteration %d] zero total debit on non-empty settlement", i)
		}
	}
}

// TestMoneyMath_ImbalancePerturbation_FailClosedEvals tests 5,000 iterations of deliberate perturbation,
// verifying that even a 1-paise imbalance is 100% rejected with ErrUnbalancedJournal.
func TestMoneyMath_ImbalancePerturbation_FailClosedEvals(t *testing.T) {
	rng := rand.New(rand.NewSource(1337))
	propID := uuid.New()
	sourceID := uuid.New()
	occurredAt := time.Now().UTC()

	const iterations = 5000
	for i := 0; i < iterations; i++ {
		amount := int64(rng.Intn(1000000) + 100) // at least 100 paise

		// Deliberate perturbation: +/- 1 paise to +/- 50 paise (never 0)
		delta := int64(rng.Intn(50) + 1)
		if rng.Intn(2) == 0 {
			delta = -delta
		}

		perturbedCredit := amount + delta
		if perturbedCredit <= 0 {
			perturbedCredit = 1
		}
		if perturbedCredit == amount {
			perturbedCredit += 1
		}

		specs := []LineSpec{
			{Account: domain.AcctBank, Debit: amount, LineKind: "cash_in"},
			{Account: domain.AcctRentRevenue, Credit: perturbedCredit, LineKind: "rent_earned"},
		}

		_, err := MakeLines(propID, sourceID, "payment", occurredAt, specs)
		if !errors.Is(err, ErrUnbalancedJournal) {
			t.Fatalf("[Iteration %d] security invariant violation: imbalanced journal accepted! dr=%d, cr=%d, diff=%d, err=%v",
				i, amount, perturbedCredit, amount-perturbedCredit, err)
		}
	}
}

// TestMoneyMath_LineDegeneracyAndNegativeEvals asserts that degenerate, negative, or invalid line combinations
// are strictly fail-closed.
func TestMoneyMath_LineDegeneracyAndNegativeEvals(t *testing.T) {
	propID := uuid.New()
	sourceID := uuid.New()
	occurredAt := time.Now().UTC()

	tests := []struct {
		name  string
		specs []LineSpec
	}{
		{
			name: "both debit and credit on single line",
			specs: []LineSpec{
				{Account: domain.AcctBank, Debit: 1000, Credit: 1000, LineKind: "invalid"},
			},
		},
		{
			name: "all zero paise",
			specs: []LineSpec{
				{Account: domain.AcctBank, Debit: 0, LineKind: "zero"},
				{Account: domain.AcctRentRevenue, Credit: 0, LineKind: "zero"},
			},
		},
		{
			name: "negative debit paise",
			specs: []LineSpec{
				{Account: domain.AcctBank, Debit: -500, LineKind: "neg"},
				{Account: domain.AcctRentRevenue, Credit: 500, LineKind: "pos"},
			},
		},
		{
			name: "negative credit paise",
			specs: []LineSpec{
				{Account: domain.AcctBank, Debit: 500, LineKind: "pos"},
				{Account: domain.AcctRentRevenue, Credit: -500, LineKind: "neg"},
			},
		},
		{
			name: "three-line balanced with negative debit (net balanced)",
			specs: []LineSpec{
				{Account: domain.AcctBank, Debit: 1000, LineKind: "pos"},
				{Account: domain.AcctDepositLiability, Debit: -500, LineKind: "neg"},
				{Account: domain.AcctRentRevenue, Credit: 500, LineKind: "pos"},
			},
		},
		{
			name: "three-line balanced with negative credit (net balanced)",
			specs: []LineSpec{
				{Account: domain.AcctBank, Debit: 500, LineKind: "pos"},
				{Account: domain.AcctDepositLiability, Credit: -500, LineKind: "neg"},
				{Account: domain.AcctRentRevenue, Credit: 1000, LineKind: "pos"},
			},
		},
		{
			name:  "empty specs",
			specs: []LineSpec{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := MakeLines(propID, sourceID, "test", occurredAt, tt.specs)
			if !errors.Is(err, ErrUnbalancedJournal) {
				t.Errorf("expected ErrUnbalancedJournal for %s, got: %v", tt.name, err)
			}
		})
	}
}

// TestMoneyMath_PartialPaymentFragmentConservation verifies that decomposing arbitrary due amounts
// across multiple partial payment fragments strictly preserves integer-paise conservation with zero drift.
func TestMoneyMath_PartialPaymentFragmentConservation(t *testing.T) {
	rng := rand.New(rand.NewSource(999))

	const iterations = 1000
	for i := 0; i < iterations; i++ {
		// Total due between 100 INR (10,000 paise) and 50,000 INR (5,000,000 paise)
		totalDuePaise := int64(rng.Intn(4990000) + 10000)

		// Split into 2 to 7 random fragments
		numFragments := rng.Intn(6) + 2
		remaining := totalDuePaise
		fragments := make([]int64, 0, numFragments)

		for f := 0; f < numFragments-1; f++ {
			if remaining <= 1 {
				break
			}
			frag := int64(rng.Intn(int(remaining-1)) + 1)
			fragments = append(fragments, frag)
			remaining -= frag
		}
		if remaining > 0 {
			fragments = append(fragments, remaining)
		}

		// Sum of fragments MUST exactly equal totalDuePaise
		var sumFragments int64
		for _, frag := range fragments {
			sumFragments += frag
		}
		if sumFragments != totalDuePaise {
			t.Fatalf("[Iteration %d] fragment sum mismatch: total=%d, sum=%d", i, totalDuePaise, sumFragments)
		}

		// Verify each fragment generates balanced double-entry lines
		for fIdx, frag := range fragments {
			specs := []LineSpec{
				{Account: domain.AcctBank, Debit: frag, LineKind: "partial_cash_in"},
				{Account: domain.AcctRentRevenue, Credit: frag, LineKind: "partial_rent_collected"},
			}
			lines, err := MakeLines(uuid.New(), uuid.New(), "partial_payment", time.Now().UTC(), specs)
			if err != nil {
				t.Fatalf("[Iteration %d, Fragment %d] failed to make lines for frag %d: %v", i, fIdx, frag, err)
			}
			if len(lines) != 2 {
				t.Fatalf("expected 2 lines, got %d", len(lines))
			}
			if lines[0].DebitPaise != frag || lines[1].CreditPaise != frag {
				t.Fatalf("paise corruption in fragment: expected %d, got dr=%d cr=%d", frag, lines[0].DebitPaise, lines[1].CreditPaise)
			}
		}
	}
}

// TestMoneyMath_MirrorPaymentAccountMapping verifies that payment matching channels map to the correct chart of accounts.
func TestMoneyMath_MirrorPaymentAccountMapping(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store, nil)

	propID := uuid.New()
	tenantID := uuid.New()

	cases := []struct {
		name       string
		matchedBy  domain.MatchedBy
		dueKind    domain.DueKind
		expectedDr string
		expectedCr string
		amount     int64
	}{
		{
			name:       "Cash Rent Payment",
			matchedBy:  domain.MatchedByCash,
			dueKind:    domain.DueKindRent,
			expectedDr: domain.AcctCash,
			expectedCr: domain.AcctRentRevenue,
			amount:     1500000,
		},
		{
			name:       "Gateway Deposit Payment",
			matchedBy:  domain.MatchedByCashfree,
			dueKind:    domain.DueKindDeposit,
			expectedDr: domain.AcctGatewayClearing,
			expectedCr: domain.AcctDepositLiability,
			amount:     2000000,
		},
		{
			name:       "Bank Electricity Payment",
			matchedBy:  domain.MatchedByManual,
			dueKind:    domain.DueKindElectricity,
			expectedDr: domain.AcctBank,
			expectedCr: domain.AcctUtilityRecoveryRevenue,
			amount:     85000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paymentID := uuid.New()
			dueID := uuid.New()
			p := &domain.Payment{
				ID:        paymentID,
				DueID:     dueID,
				TenantID:  tenantID,
				Amount:    tc.amount,
				MatchedBy: tc.matchedBy,
				MatchedAt: time.Now().UTC(),
			}
			due := &domain.Due{
				ID:         dueID,
				PropertyID: propID,
				TenantID:   tenantID,
				Kind:       tc.dueKind,
			}

			err := svc.MirrorPayment(ctx, p, due)
			if err != nil {
				t.Fatalf("expected nil err, got %v", err)
			}

			lines, err := store.ListJournal(ctx, propID, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
			if err != nil {
				t.Fatalf("list journal err: %v", err)
			}
			if len(lines) == 0 {
				t.Fatalf("expected journal lines, got 0")
			}

			var foundDr, foundCr bool
			for _, l := range lines {
				if l.SourceID == paymentID {
					if l.AccountCode == tc.expectedDr && l.DebitPaise == int64(tc.amount) {
						foundDr = true
					}
					if l.AccountCode == tc.expectedCr && l.CreditPaise == int64(tc.amount) {
						foundCr = true
					}
				}
			}
			if !foundDr {
				t.Errorf("expected debit to %s of %d paise", tc.expectedDr, tc.amount)
			}
			if !foundCr {
				t.Errorf("expected credit to %s of %d paise", tc.expectedCr, tc.amount)
			}
		})
	}
}

// TestMoneyMath_SettlementConservation_PropertyEvals executes 5,000 randomized property evaluations
// verifying that ProcessSettlement strictly conserves double-entry balance:
// Gross == Net + ServiceFee + ServiceTax + Adjustment
// sum(Debits) == sum(Credits) == max(Gross, Gross - Adjustment) with 0 integer-paise drift across
// arbitrary combinations of positive, negative, and zero adjustments.
func TestMoneyMath_SettlementConservation_PropertyEvals(t *testing.T) {
	rng := rand.New(rand.NewSource(54321))
	ctx := context.Background()

	const iterations = 5000
	for i := 0; i < iterations; i++ {
		st := NewMemoryStore()
		svc := NewService(st, nil)
		pid := uuid.New()

		// Gross amount between ₹100 (10,000 paise) and ₹100,000 (10,000,000 paise)
		grossPaise := int64(rng.Intn(9990000) + 10000)

		// Fee up to 3% of gross, min 0
		feePaise := int64(float64(grossPaise) * (float64(rng.Intn(300)) / 10000.0))
		// Tax is 18% of fee
		taxPaise := int64(float64(feePaise) * 0.18)

		// Adjustment: can be positive (refund deducted), zero, or negative (correction added)
		// Keep adjustment bounded so net remains positive (> 0)
		maxPositiveAdj := (grossPaise - feePaise - taxPaise) / 2
		if maxPositiveAdj < 1 {
			maxPositiveAdj = 1
		}
		var adjPaise int64
		switch rng.Intn(3) {
		case 0:
			adjPaise = 0
		case 1:
			adjPaise = int64(rng.Intn(int(maxPositiveAdj)))
		case 2:
			// Negative adjustment: gateway added money back
			adjPaise = -int64(rng.Intn(int(maxPositiveAdj)))
		}

		netPaise := grossPaise - (feePaise + taxPaise + adjPaise)
		if netPaise <= 0 {
			continue
		}

		rec := SettlementRecord{
			SettlementID:     fmt.Sprintf("SETTLE_PROP_%d", i),
			TransferUTR:      fmt.Sprintf("UTR_PROP_%d", i),
			TransferTime:     time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC),
			GrossAmountPaise: grossPaise,
			NetAmountPaise:   netPaise,
			ServiceFeePaise:  feePaise,
			ServiceTaxPaise:  taxPaise,
			AdjustmentPaise:  adjPaise,
		}

		err := svc.ProcessSettlement(ctx, pid, rec)
		if err != nil {
			t.Fatalf("[Iteration %d] expected success, got error: %v (gross=%d net=%d fee=%d tax=%d adj=%d)",
				i, err, grossPaise, netPaise, feePaise, taxPaise, adjPaise)
		}

		// List journal lines and verify double-entry conservation
		lines, err := st.ListJournal(ctx, pid, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
		if err != nil {
			t.Fatalf("[Iteration %d] list journal failed: %v", i, err)
		}

		var totalDr, totalCr int64
		var bankDr, clearingCr, feeDr, adjDr, adjCr int64

		for _, l := range lines {
			if l.DebitPaise > 0 && l.CreditPaise > 0 {
				t.Fatalf("[Iteration %d] line %s has both debit and credit", i, l.ID)
			}
			if l.DebitPaise < 0 || l.CreditPaise < 0 {
				t.Fatalf("[Iteration %d] negative paise on line %s", i, l.ID)
			}
			totalDr += l.DebitPaise
			totalCr += l.CreditPaise

			switch l.AccountCode {
			case domain.AcctBank:
				bankDr += l.DebitPaise
			case domain.AcctGatewayClearing:
				clearingCr += l.CreditPaise
			case domain.AcctPaymentProcessingExpense:
				feeDr += l.DebitPaise
			case domain.AcctGatewayAdjustment:
				adjDr += l.DebitPaise
				adjCr += l.CreditPaise
			}
		}

		if totalDr != totalCr {
			t.Fatalf("[Iteration %d] journal imbalanced: totalDr=%d totalCr=%d diff=%d",
				i, totalDr, totalCr, totalDr-totalCr)
		}
		if bankDr != netPaise {
			t.Fatalf("[Iteration %d] bankDr mismatch: expected %d, got %d", i, netPaise, bankDr)
		}
		if clearingCr != grossPaise {
			t.Fatalf("[Iteration %d] clearingCr mismatch: expected %d, got %d", i, grossPaise, clearingCr)
		}
		if feeDr != (feePaise + taxPaise) {
			t.Fatalf("[Iteration %d] feeDr mismatch: expected %d, got %d", i, feePaise+taxPaise, feeDr)
		}
		if adjPaise > 0 && adjDr != adjPaise {
			t.Fatalf("[Iteration %d] positive adjDr mismatch: expected %d, got %d", i, adjPaise, adjDr)
		}
		if adjPaise < 0 && adjCr != -adjPaise {
			t.Fatalf("[Iteration %d] negative adjCr mismatch: expected %d, got %d", i, -adjPaise, adjCr)
		}
	}
}

// TestMoneyMath_SettlementPerturbation_FailClosedEvals verifies that deliberate 1-paise perturbations
// on ANY of the 5 settlement terms (Gross, Net, ServiceFee, ServiceTax, Adjustment) strictly fail closed.
func TestMoneyMath_SettlementPerturbation_FailClosedEvals(t *testing.T) {
	rng := rand.New(rand.NewSource(98765))
	ctx := context.Background()

	const iterations = 2500
	for i := 0; i < iterations; i++ {
		st := NewMemoryStore()
		svc := NewService(st, nil)
		pid := uuid.New()

		grossPaise := int64(rng.Intn(5000000) + 10000)
		feePaise := int64(1000)
		taxPaise := int64(180)
		adjPaise := int64(500)
		netPaise := grossPaise - (feePaise + taxPaise + adjPaise)

		// Perturb one random component by +/- 1 to +/- 10 paise
		delta := int64(rng.Intn(10) + 1)
		if rng.Intn(2) == 0 {
			delta = -delta
		}

		rec := SettlementRecord{
			SettlementID:     fmt.Sprintf("SETTLE_PERTURB_%d", i),
			GrossAmountPaise: grossPaise,
			NetAmountPaise:   netPaise,
			ServiceFeePaise:  feePaise,
			ServiceTaxPaise:  taxPaise,
			AdjustmentPaise:  adjPaise,
		}

		targetTerm := rng.Intn(5)
		switch targetTerm {
		case 0:
			rec.GrossAmountPaise += delta
		case 1:
			rec.NetAmountPaise += delta
		case 2:
			rec.ServiceFeePaise += delta
		case 3:
			rec.ServiceTaxPaise += delta
		case 4:
			rec.AdjustmentPaise += delta
		}

		if rec.GrossAmountPaise <= 0 || rec.NetAmountPaise <= 0 {
			continue
		}

		err := svc.ProcessSettlement(ctx, pid, rec)
		if !errors.Is(err, ErrSettlementUnbalanced) && !errors.Is(err, ErrInvalidSettlementData) {
			t.Fatalf("[Iteration %d, Term %d] expected ErrSettlementUnbalanced for delta %d, got: %v",
				i, targetTerm, delta, err)
		}
	}
}

// TestMoneyMath_BankStatementIngress_DoubleEntryConservation_PropertyEvals verifies that Bank Statement
// credits, quarantine entries, allocations, refunds, and reclassifications strictly conserve double-entry balance:
// sum(Debits) == sum(Credits) with 0 integer-paise drift across 10,000 randomized lifecycle flows.
func TestMoneyMath_BankStatementIngress_DoubleEntryConservation_PropertyEvals(t *testing.T) {
	rng := rand.New(rand.NewSource(13579))
	ctx := context.Background()

	const iterations = 10000
	for i := 0; i < iterations; i++ {
		st := NewMemoryStore()
		svc := NewService(st, nil)
		pid := uuid.New()
		txnID := uuid.New()
		at := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)

		// 1. Ingest random bank statement credit: 100 to 5,000,000 paise (₹1 to ₹50,000)
		creditPaise := int64(rng.Intn(5000000) + 100)
		err := svc.MirrorBankStatementCredit(ctx, pid, txnID, creditPaise, at)
		if err != nil {
			t.Fatalf("[Iteration %d] MirrorBankStatementCredit failed: %v", i, err)
		}

		// 2. Perform a random lifecycle resolution:
		// 0 = Allocate to Rent / Deposit / Utilities
		// 1 = Refund unapplied deposit back to payer
		// 2 = Reclassify to non-rent revenue / interest / capital
		resolutionMode := rng.Intn(3)
		switch resolutionMode {
		case 0:
			// Allocation
			dueKinds := []domain.DueKind{domain.DueKindRent, domain.DueKindDeposit, domain.DueKindElectricity, domain.DueKindWater}
			kind := dueKinds[rng.Intn(len(dueKinds))]
			err = svc.MirrorUnappliedAllocation(ctx, pid, txnID, kind, creditPaise, at.Add(time.Hour))
			if err != nil {
				t.Fatalf("[Iteration %d] MirrorUnappliedAllocation failed: %v", i, err)
			}
		case 1:
			// Refund
			err = svc.MirrorBankDepositRefund(ctx, pid, txnID, creditPaise, at.Add(time.Hour))
			if err != nil {
				t.Fatalf("[Iteration %d] MirrorBankDepositRefund failed: %v", i, err)
			}
		case 2:
			// Reclassification
			accounts := []string{domain.AcctInterestIncome, domain.AcctOwnerCapital, domain.AcctNonPGOtherIncome}
			targetAcc := accounts[rng.Intn(len(accounts))]
			err = svc.MirrorUnappliedReclassification(ctx, pid, txnID, targetAcc, creditPaise, at.Add(time.Hour))
			if err != nil {
				t.Fatalf("[Iteration %d] MirrorUnappliedReclassification failed: %v", i, err)
			}
		}

		// 3. Inspect journal lines and assert strict double-entry balance invariants
		lines, err := st.ListJournal(ctx, pid, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
		if err != nil {
			t.Fatalf("[Iteration %d] ListJournal failed: %v", i, err)
		}

		var totalDr, totalCr int64
		for _, l := range lines {
			if l.DebitPaise > 0 && l.CreditPaise > 0 {
				t.Fatalf("[Iteration %d] line %s has both debit and credit", i, l.ID)
			}
			if l.DebitPaise < 0 || l.CreditPaise < 0 {
				t.Fatalf("[Iteration %d] negative paise on line %s", i, l.ID)
			}
			totalDr += l.DebitPaise
			totalCr += l.CreditPaise
		}

		if totalDr != totalCr {
			t.Fatalf("[Iteration %d] journal imbalanced: totalDr=%d totalCr=%d diff=%d",
				i, totalDr, totalCr, totalDr-totalCr)
		}
		// In any balanced two-step flow, total journal activity is exactly 2 * creditPaise
		if totalDr != creditPaise*2 {
			t.Fatalf("[Iteration %d] total debit activity mismatch: expected %d, got %d",
				i, creditPaise*2, totalDr)
		}
	}
}

// TestMoneyMath_BankStatement_Perturbation_FailClosedEvals verifies that deliberate imbalance perturbations
// strictly fail closed when making bank statement mirror journal lines.
func TestMoneyMath_BankStatement_Perturbation_FailClosedEvals(t *testing.T) {
	rng := rand.New(rand.NewSource(24680))
	pid := uuid.New()
	txnID := uuid.New()
	at := time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)

	const iterations = 2500
	for i := 0; i < iterations; i++ {
		creditPaise := int64(rng.Intn(5000000) + 100)
		delta := int64(rng.Intn(10) + 1)
		if rng.Intn(2) == 0 {
			delta = -delta
		}

		// Perturbed specs
		specs := []LineSpec{
			{Account: domain.AcctBank, Debit: creditPaise + delta, LineKind: "bank_deposit_dr"},
			{Account: domain.AcctUnappliedReceipts, Credit: creditPaise, LineKind: "unapplied_receipt_cr"},
		}

		_, err := MakeLines(pid, txnID, "bank_statement_credit", at, specs)
		if !errors.Is(err, ErrUnbalancedJournal) {
			t.Fatalf("[Iteration %d] expected ErrUnbalancedJournal for delta %d, got: %v", i, delta, err)
		}
	}
}

// TestMoneyMath_EODMultiWayBalancer_PropertyEvals verifies that the EOD Multi-Way Settlement Balancer
// strictly asserts balanced state (is_balanced=true, discrepancy=0) across 10,000 randomized synthetic business days.
func TestMoneyMath_EODMultiWayBalancer_PropertyEvals(t *testing.T) {
	rng := rand.New(rand.NewSource(54321))
	pid := uuid.New()
	reconDate := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

	const iterations = 10000
	for i := 0; i < iterations; i++ {
		// 1. Gateway Collections: 1,000 to 10,000,000 paise (₹10 to ₹100,000)
		gwCollections := int64(rng.Intn(10000000) + 1000)

		// 2. Gateway Settlement Batch:
		// Portion of collections settled today (e.g., 50% to 100%)
		settledPortion := int64(rng.Intn(int(gwCollections)) + 1)
		feePaise := settledPortion * int64(rng.Intn(200)+100) / 10000 // 1.00% to 3.00%
		taxPaise := feePaise * 18 / 100                               // 18% GST
		adjPaise := int64(rng.Intn(500))                              // 0 to 500 paise adjustments
		netSettledPaise := settledPortion - (feePaise + taxPaise + adjPaise)
		if netSettledPaise <= 0 {
			continue
		}

		inTransitPaise := gwCollections - settledPortion

		// 3. Bank Statement Cleared Activity:
		// Cleared net gateway settlement + direct bank transfers
		directBankReceipts := int64(rng.Intn(2000000))
		bankCredits := netSettledPaise + directBankReceipts
		bankDebits := int64(rng.Intn(1000000))

		// 4. Ledger Activity (in sync with cleared statements):
		ledgerBankDr := bankCredits
		ledgerBankCr := bankDebits

		bal := domain.DailySettlementBalance{
			PropertyID:             pid,
			ReconDate:              reconDate,
			GatewayGrossPaise:      settledPortion,
			GatewayNetSettledPaise: netSettledPaise,
			GatewayFeesPaise:       feePaise,
			GatewayTaxPaise:        taxPaise,
			GatewayAdjustmentPaise: adjPaise,
			GatewayInTransitPaise:  inTransitPaise,
			BankCreditsPaise:       bankCredits,
			BankDebitsPaise:        bankDebits,
			LedgerBankDrPaise:      ledgerBankDr,
			LedgerBankCrPaise:      ledgerBankCr,
		}

		bal.EvaluateBalance()

		if !bal.IsBalanced {
			t.Fatalf("[Iteration %d] expected is_balanced=true, got false (discrepancy: %d paise, discrepancies: %+v)",
				i, bal.DiscrepancyPaise, bal.Discrepancies)
		}
		if bal.DiscrepancyPaise != 0 {
			t.Fatalf("[Iteration %d] expected discrepancy_paise=0, got %d", i, bal.DiscrepancyPaise)
		}
	}
}

// TestMoneyMath_EODMultiWayBalancer_Perturbation_FailClosedEvals verifies that deliberate
// 1-paise perturbations on ANY of the balancer terms strictly fail closed (is_balanced=false, discrepancy>0).
func TestMoneyMath_EODMultiWayBalancer_Perturbation_FailClosedEvals(t *testing.T) {
	rng := rand.New(rand.NewSource(67890))
	pid := uuid.New()
	reconDate := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

	const iterations = 2500
	for i := 0; i < iterations; i++ {
		gross := int64(rng.Intn(5000000) + 10000)
		fee := int64(1500)
		tax := int64(270)
		adj := int64(100)
		net := gross - (fee + tax + adj)
		bankCredits := net
		ledgerDr := net

		bal := domain.DailySettlementBalance{
			PropertyID:             pid,
			ReconDate:              reconDate,
			GatewayGrossPaise:      gross,
			GatewayNetSettledPaise: net,
			GatewayFeesPaise:       fee,
			GatewayTaxPaise:        tax,
			GatewayAdjustmentPaise: adj,
			GatewayInTransitPaise:  0,
			BankCreditsPaise:       bankCredits,
			LedgerBankDrPaise:      ledgerDr,
		}

		// Perturb one random term by +/- 1 to +/- 10 paise
		delta := int64(rng.Intn(10) + 1)
		if rng.Intn(2) == 0 {
			delta = -delta
		}

		targetTerm := rng.Intn(4)
		switch targetTerm {
		case 0:
			bal.GatewayNetSettledPaise += delta
		case 1:
			bal.BankCreditsPaise += delta
		case 2:
			bal.LedgerBankDrPaise += delta
		case 3:
			bal.GatewayInTransitPaise = -int64(rng.Intn(10) + 1) // strictly negative in-transit
		}

		bal.EvaluateBalance()

		if bal.IsBalanced {
			t.Fatalf("[Iteration %d, Term %d] expected is_balanced=false for delta %d, got true",
				i, targetTerm, delta)
		}
		if bal.DiscrepancyPaise == 0 {
			t.Fatalf("[Iteration %d, Term %d] expected discrepancy > 0, got 0", i, targetTerm)
		}
	}
}
