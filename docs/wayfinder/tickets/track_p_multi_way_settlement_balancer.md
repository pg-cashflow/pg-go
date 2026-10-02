# Ticket 15: Track P — Stream 3 Layer 3: End-of-Day Multi-Way Settlement Balancer & Audit Trail

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Prerequisite**: Track N (Gateway Settlements) & Track O (Bank Statements & Unidentified Quarantine) resolved.
- **Resolution Date**: 2026-10-02
- **Verification**:
  - `TestMoneyMath_EODMultiWayBalancer_PropertyEvals` (10,000 iterations pass)
  - `TestMoneyMath_EODMultiWayBalancer_Perturbation_FailClosedEvals` (2,500 iterations pass)
  - `TestOwnerEODSettlementBalance_Endpoints` (4/4 subtests pass)
  - All 31 internal Go packages pass (`go test ./...` code 0)
  - Full PWA build, test, and lint clean (`pg-react`: 16/16 tests pass, 0 lint errors, build 0 errors)


---

## 1. Objective

Implement **Stream 3 Layer 3 (End-of-Day Multi-Way Settlement Balancer & Audit Trail)**:
1. **Tri-Party Balancing Engine**:
   Reconcile and cross-verify three distinct sources of financial truth for each property:
   - **Leg 1: Cashfree Payment Gateway**: Total gross collections, gateway settlements, MDR fees, GST tax, adjustments, and in-transit clearing balance.
   - **Leg 2: Bank Statement Cash Ledger**: Cleared settlement payouts from Cashfree, direct tenant receipts, unapplied quarantine balance, and payout disbursements.
   - **Leg 3: General Ledger Trial Balance**: Double-entry general ledger accounts (`bank`, `gateway_clearing`, `unapplied_receipts`, `refund_payable`, `rent_revenue`, `deposit_liability`, `payment_processing_expense`, `gateway_adjustment`).
2. **Mathematical Balance Invariants**:
   - $\sum \text{Debits} == \sum \text{Credits}$ across all journal entries for the period.
   - In-Transit Gateway Balance = $\text{Gross Collected via Cashfree} - \text{Gross Settled by Cashfree} - \text{Gateway Refunds}$.
   - Unapplied Quarantine Tie-Out = $\text{Net Credit Balance in AcctUnappliedReceipts} == \sum \text{Bank Transactions in 'unmatched' or 'suggested_match' status}$.
   - Cleared Bank Settlement Match = $\text{Gateway Net Settlement Amount} == \text{Bank Cleared Credit Amount}$ matched by UTR / amount window.
3. **Automated Variance Detection**:
   Zero automated data mutation. Any mismatch in balances or missing bank credits is surfaced as an explicit discrepancy with category, aging, and severity.
4. **Persistent Audit Trail**:
   Create migration `031_daily_settlement_balances.sql` storing daily snapshots of the tri-party balancer state for historical audits and compliance.
5. **REST Endpoints**:
   - `GET /api/owner/settlements/eod-balance?date=YYYY-MM-DD`
   - `POST /api/owner/settlements/eod-balance/run`
   - `GET /api/owner/settlements/eod-balance/history`
6. **Track K Invariant Evals**:
   Continuous property-based invariant evaluations proving exact balance conservation and fail-closed variance detection across 10,000 synthetic days.
