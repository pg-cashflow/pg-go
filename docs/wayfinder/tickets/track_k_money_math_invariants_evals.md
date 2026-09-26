# Track K (Ticket 10): Continuous Money-Math Invariants & Fuzzing Evals

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Objective

Establish a continuous, rigorous property-based and fuzzing evaluation suite for the core financial accounting engine (`internal/finance/mirror.go` and `internal/finance/journal.go`). Beyond standard static unit tests, verify that double-entry balance ($\sum \text{Debits} == \sum \text{Credits}$), zero float precision drift, and strict fail-closed rejection of imbalanced entries hold across thousands of randomized permutations.

## Implementation Details

1. **Property-Based Double-Entry Conservation (`TestMoneyMath_DoubleEntryConservation_PropertyEvals`)**:
   - Executes 10,000 randomized permutations of tenant departure settlements with deterministic pseudo-random seeds.
   - Covers deposit deductions, unearned rent proration reversals, damages recovery, and outstanding dues netting.
   - Mathematically verifies that $\sum \text{Debits} == \sum \text{Credits}$ with 0 paise drift, non-zero debit volume, positive integer-paise conservation, and correct account line kinds.

2. **Fail-Closed Imbalance Perturbation Fuzzing (`TestMoneyMath_ImbalancePerturbation_FailClosedEvals`)**:
   - Executes 5,000 iterations injecting random perturbations of $\pm 1$ to $\pm 50$ paise.
   - Proves a 100% rejection rate with `ErrUnbalancedJournal`. Zero imbalanced entries can enter the double-entry accounting ledger.

3. **Line Degeneracy & Negative Value Guards (`TestMoneyMath_LineDegeneracyAndNegativeEvals`)**:
   - Asserts strict fail-closed rejection when encountering negative paise, simultaneous debits & credits on the same line, zero-total entries, or empty specs.

4. **Partial Payment Fragment Conservation (`TestMoneyMath_PartialPaymentFragmentConservation`)**:
   - Decomposes arbitrary due amounts across 2–7 random integer-paise fragments over 1,000 iterations.
   - Proves exact integer-paise conservation ($\sum \text{Fragments} == \text{Total Due}$) and 1:1 balanced double-entry line generation.

5. **Chart of Accounts Invariant Mapping (`TestMoneyMath_MirrorPaymentAccountMapping`)**:
   - Proves that cash, UPI/bank, and gateway clearing channels accurately debit and credit their respective balance sheet and revenue accounts without account code mutation.

6. **Fail-Closed Dead-Letter Notifier Hardening**:
   - Updated `internal/finance/alert.go` to return an explicit configuration error if `EmailDeadLetterNotifier` is invoked with a nil mailer or empty admin email, preventing silent failures.
