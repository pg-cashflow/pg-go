# Journal Entry Catalogue

Every posting the system makes, derived from `internal/finance/mirror.go` and `settlement*.go`. Amounts are integer paise. A *source* is one `(source_type, source_id)`; every source must balance, per property (control C-2). `Dr` = debit, `Cr` = credit.

**Accounting basis (see the design note in `mirror.go`):** modified collection basis. Dues create no entry; revenue is recognised on collection. Security deposits are liabilities, never revenue.

## Chart of accounts and statement class

| Account | Class | Normal balance | Statement |
|---------|-------|----------------|-----------|
| `cash`, `bank` | asset | Dr | Balance sheet; **cash equivalents** in the cash-flow statement |
| `gateway_clearing` | asset | Dr | Balance sheet (funds in transit at the processor, **not** a cash equivalent) |
| `tenant_receivable` | asset | Dr | Balance sheet |
| `deposit_liability`, `accounts_payable`, `manager_advance_payable`, `reward_liability`, `unapplied_receipts`, `refund_payable` | liability | Cr | Balance sheet |
| `owner_capital` | equity | Cr | Balance sheet; **financing** in cash flow |
| `rent_revenue`, `utility_recovery_revenue`, `damages_income`, `interest_income`, `non_pg_other_income` | revenue | Cr | Income statement |
| `operating_expense`, `loyalty_expense`, `payment_processing_expense` | expense | Dr | Income statement |
| `gateway_adjustment` | other | either | Income statement, net (debit = loss, credit = gain) |

The authoritative mapping is `ledger_account_class()` in migration 044. Adding an account to the Go constants **requires** adding it there in the same change, otherwise it appears as `unclassified` and fails the statement check.

## Entries

| Source type | Trigger | Dr | Cr | Notes |
|-------------|---------|----|----|-------|
| `payment` | Gateway payment applied to dues (`MirrorPayment`) | `bank`/`cash`/`gateway_clearing` (cash_in) | `deposit_liability` (deposit) / `utility_recovery_revenue` (electricity, water) / `rent_revenue` (rent) | **Whole payment is classified by the first due's kind** (finding F3). `cash`-matched payments debit `cash` |
| `unapplied_payment` | Gateway payment with no open dues | `gateway_clearing` | `unapplied_receipts` | Liability until allocated or refunded |
| `refund` | Gateway refund succeeded (`MirrorRefund`) | `unapplied_receipts` (unapplied) / `deposit_liability` / `utility_recovery_revenue` / `rent_revenue` | `gateway_clearing` | Posted once per allocation with the same line kinds; later allocations are dropped (finding F2) |
| `proration` | Due reduced (`MirrorProration`) | `rent_revenue` | `tenant_receivable` | No callers today; latent (finding F5) |
| `reward_issue` | Loyalty points issued | `loyalty_expense` | `reward_liability` | |
| `reward_redeem` | Points redeemed against a due | `reward_liability` | `tenant_receivable` | |
| `departure_settlement` | Tenant leaves (`MirrorDepartureSettlement`) | `deposit_liability` (deposit), `rent_revenue` (unearned rent reversal), `tenant_receivable` (balance) | `rent_revenue` (dues netted, earned), `damages_income`, `refund_payable` (net refund) | Multi-line; one debit group, credit group; must balance |
| `payout_settlement` | Payout paid | `refund_payable` | `bank` | |
| `payout_reversal` | Payout returned | `bank` | `refund_payable` | |
| `bank_statement_credit` | Unmatched bank credit imported | `bank` | `unapplied_receipts` | |
| `unapplied_allocation` | Bank credit matched to a due | `unapplied_receipts` | `rent_revenue` / `utility_recovery_revenue` / `deposit_liability` by due kind | |
| `unapplied_deposit_refund` | Bank credit refunded | `unapplied_receipts` | `bank` | |
| `unapplied_reclassification` | Owner reclassifies a credit | `unapplied_receipts` | target account | |
| `gateway_settlement` | Cashfree settlement ingested | `bank` (net), `payment_processing_expense` (fees + tax) | `gateway_clearing` (gross) | Gross = net + fees + tax + adjustments; adjustments go to `gateway_adjustment` |
| `capital` | Owner capital introduced | `cash`/`bank` | `owner_capital` | Financing in cash flow. A **withdrawal** is the mirror image (Dr `owner_capital` / Cr `cash`/`bank`) under the same source type |
| `expense` | Expense recorded | `operating_expense` | `accounts_payable` | Accrual at entry, so unpaid expenses are recognised immediately (unlike rent) |
| `expense_payment` | Expense paid | `accounts_payable` | `cash`/`bank`, or `manager_advance_payable` when the manager paid personally | |
| `reimbursement` | Manager reimbursed | `manager_advance_payable` | `bank` | |

## Rules every new posting must satisfy

1. **Idempotent source key.** Use a stable id that exists independently of the posting (payment id, refund allocation id, settlement id). The unique index `(source_type, source_id, line_kind)` is the idempotency guard; do not generate fresh ids per attempt.
2. **One property per source.** Enforced by C-2 per property.
3. **Dated in an open period.** `occurred_at` inside a closed period fails with `finance.periodClosed` (409). Late entries are posted in the current open period.
4. **Post in the same transaction as the business row, or via the outbox.** Never in a separate, earlier commit (ADR-016 F1).
5. **Corrections are new entries.** The journal is append-only; reverse with an opposite entry referencing the original in the line note.
6. **Add the account to `ledger_account_class()`** and to the regression suite in the same change.

## Worked example: month of September

| Event | Entry | Effect |
|-------|-------|--------|
| Owner introduces 500,000 | Dr bank / Cr owner_capital | Equity +500,000 (financing) |
| Rent 100,000 by bank, 80,000 via gateway | Dr bank 100,000, Dr gateway_clearing 80,000 / Cr rent_revenue 180,000 | Revenue +180,000 |
| Deposit 40,000 | Dr bank / Cr deposit_liability | Liability, no revenue |
| Utility recovery 5,000 | Dr bank / Cr utility_recovery_revenue | Revenue +5,000 |
| Expense 30,000 recorded, then paid | Dr operating_expense / Cr accounts_payable; then Dr accounts_payable / Cr bank | Expense +30,000 |
| Rent refund 10,000 | Dr rent_revenue / Cr gateway_clearing | Revenue -10,000 |
| Gateway settles 60,000 (fees 1,500) | Dr bank 58,500, Dr payment_processing_expense 1,500 / Cr gateway_clearing 60,000 | Clearing -60,000, expense +1,500 |
| Tenant departs: deposit 40,000 = damages 5,000 + refund 35,000 | Dr deposit_liability 40,000 / Cr damages_income 5,000, Cr refund_payable 35,000 | Revenue +5,000 |
| Refund paid out 35,000 | Dr refund_payable / Cr bank | Bank -35,000 |

Resulting statements (these exact numbers are asserted in `scripts/sql/ledger_controls_test.sql`): total revenue 180,000, total expense 31,500, **net income 148,500**; balance sheet check difference **0**; cash flow check difference **0**.
