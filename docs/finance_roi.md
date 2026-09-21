# Finance & ROI Architecture Guide

## 1. Money Taxonomy Matrix

All monetary amounts across the system are stored in **bigint paise** (1 Rupee = 100 Paise).

### Money In

| Money In Type | Accounting Treatment | Primary Debit Account | Primary Credit Account | Notes |
| :--- | :--- | :--- | :--- | :--- |
| **Tenant Rent Collection** | Operating Revenue | `1010 bank` / `1000 cash` | `4000 rent_revenue` | authoritative control total in `ReconciliationSummary.RentCollected` |
| **Utility Recovery** | Revenue / Cost Recovery | `1010 bank` / `1000 cash` | `4010 utility_recovery_revenue` | Tenant electricity / water recovery |
| **Owner Capital Contribution** | Owner Equity | `1010 bank` | `3000 owner_capital` | **Never** treated as revenue |
| **Tenant Security Deposit** | Liability | `1010 bank` | `2010 deposit_liability` | Excluded from operating profit / OCF |
| **Manager Advance (Pocket Spend)** | Manager Payable Liability | `2000 accounts_payable` | `2020 manager_advance_payable` | Expense recognized in full; payable owed to manager |
| **Reward Redemption (Rent Credit)** | Liability Release / Credit | `5010 loyalty_expense` | `2030 reward_liability` / `4000 rent_revenue` | Reduces cash collected; mirrors `credits_held_paise` |

### Money Out

| Money Out Type | Classification | Primary Debit Account | Primary Credit Account | Notes |
| :--- | :--- | :--- | :--- | :--- |
| **Vendor Operating Expense** | OPEX | `5000 operating_expense` | `2000 accounts_payable` | Classified by category (food, maintenance, etc.) |
| **Owner Direct Vendor Payment** | Cash Outflow | `2000 accounts_payable` | `1010 bank` / `1000 cash` | Settles payable directly |
| **Manager Reimbursement** | Liability Settlement | `2020 manager_advance_payable` | `1010 bank` / `1000 cash` | Owner reimburses manager for out-of-pocket spend |
| **Owner Capital Withdrawal** | Equity Draw | `3000 owner_capital` | `1010 bank` | Reduces invested capital |
| **Tenant Deposit Refund** | Liability Clearance | `2010 deposit_liability` | `1010 bank` | Settles deposit obligation |
| **Payment Processing (TDR)** | Variable Cost | `5020 payment_processing_expense`| `1010 bank` | Owner-absorbed Cashfree gateway fee |
| **Reward Points Issuance** | Expense / Liability Build | `5010 loyalty_expense` | `2030 reward_liability` | Points issued × point_value_paise |

---

## 2. Standard Chart of Accounts

```
1000  cash                            (Asset)
1010  bank                            (Asset)
1020  accounts_receivable             (Asset)
2000  accounts_payable                (Liability)
2010  deposit_liability               (Liability)
2020  manager_advance_payable         (Liability)
2030  reward_liability                (Liability)
3000  owner_capital                   (Equity)
4000  rent_revenue                    (Revenue)
4010  utility_recovery_revenue        (Revenue)
5000  operating_expense               (Expense)
5010  loyalty_expense                 (Expense)
5020  payment_processing_expense      (Expense)
```

---

## 3. Double-Entry Journal Scenarios

### Scenario A: Manager Advance Split Payment & Reimbursement
**Context**: ₹10,000 vendor repair bill. Manager pays ₹4,000 out of pocket. Owner pays remaining ₹6,000 from bank. Later, owner reimburses manager ₹4,000.

#### Step 1: Expense Accrual (Full Bill Recorded)
```
DR operating_expense (repairs)      10,000.00
CR accounts_payable                 10,000.00
```

#### Step 2a: Manager Pays ₹4,000 (Out of Pocket)
```
DR accounts_payable                  4,000.00
CR manager_advance_payable           4,000.00
```
*(Manager advance record inserted in `manager_advances`; outstanding advance = ₹4,000)*

#### Step 2b: Owner Pays ₹6,000 (From Bank)
```
DR accounts_payable                  6,000.00
CR bank                              6,000.00
```
*(Accounts payable balance is now ₹0; expense status updated to `paid`)*

#### Step 3: Owner Reimburses Manager ₹4,000
```
DR manager_advance_payable           4,000.00
CR bank                              4,000.00
```
*(Manager advance payable balance is now ₹0; total owner cash out = ₹6,000 + ₹4,000 = ₹10,000)*

---

### Scenario B: Proration on Mid-Cycle Vacate
**Context**: Tenant vacates early; due is prorated from ₹12,000 down to ₹8,000 (credit adjustment of ₹4,000).

```
DR rent_revenue                      4,000.00
CR accounts_receivable               4,000.00
```

---

### Scenario C: Reward Redemption for Rent Credit
**Context**: Tenant redeems ₹500 worth of reward points as rent credit against their due.

```
DR loyalty_expense (or reward_liability)   500.00
CR rent_revenue (or accounts_receivable)   500.00
```
*(Mirrored in `reward_liability_transactions` with `kind='redeemed'`; reconciles with `credits_held_paise`)*

---

### Scenario D: Payment Gateway TDR (Owner-Absorbed)
**Context**: ₹10,000 tenant payment processed through Cashfree gateway at 1.8% effective fee.

```
DR bank (net deposit)                9,820.00
DR payment_processing_expense          180.00
CR rent_revenue                     10,000.00
```
*(When fee feed is pending, TDR is computed using config effective bps with `tdr_is_estimated=true`)*

---

## 4. Reconciliation Tie-Out Control Total vs Operating Ledger

```
Reconciliation Control (Authoritative)       ₹1,00,000.00
  ├── Timing differences                     -   ₹2,000.00
  ├── Proration adjustments                  +   ₹1,000.00
  ├── Reward rent credit applied             +     ₹500.00
  └── Unexplained discrepancy (investigate)          ₹0.00
Ledger Rent Revenue Credits                  ₹99,500.00
```

**Policy Rules**:
1. Zero tolerance: Any unexplained discrepancy (`difference_paise != 0`) strictly blocks period close.
2. Recurring exceptions: Unresolved investigate items across 3 consecutive months escalate to a Level 3 alert with `is_action_required: true`.
3. Official ROI: Only closed periods qualify for official ROI snapshot records.

---

## 5. Monthly Variance Bridge Decomposition

Explains the difference between Budgeted Operating Cashflow and Actual Operating Cashflow:

```
Budgeted Operating Cashflow                   ₹1,20,000.00
  |-- Occupancy volume variance                -₹18,000.00
  |-- Rate variance (rent per bed)              +₹4,000.00
  |-- Collection variance                       -₹6,200.00
  |-- Food cost variance                        -₹9,800.00
  |-- Electricity cost variance                 -₹7,200.00
  |-- Reward liability variance                 -₹3,100.00
  |-- Payment processing TDR variance (est)       -₹800.00
Actual Operating Cashflow                       ₹79,700.00
```
Residual variance target = ₹0.00.
