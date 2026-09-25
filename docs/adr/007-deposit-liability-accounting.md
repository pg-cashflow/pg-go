# ADR 007: Deposit Liability Accounting, Anniversary Cycle Proration & Dues Netting

## Status
Accepted
**Date:** 2026-09-24
**Decider:** Divakar (Solo Developer)

## Context
When tenants join a PG, security deposits are collected and held until departure. Concurrently, tenants may join midway through a month, requiring pro-rata rent calculations, or depart with outstanding dues (rent, electricity, repairs) that must be netted against the deposit liability.

## Decisions

### 1. Deposit as Balance Sheet Liability (`deposit_liability`)
- Deposits are never treated as revenue or operating income.
- When collected, deposit payments credit `deposit_liability` (liability account) and debit `bank` or `gateway_clearing` (asset).
- At departure, deductions for unpaid rent or damages debit `deposit_liability` and credit `revenue` (or accounts receivable), while remaining net refundable amounts credit `accounts_payable_refunds`.

### 2. Anniversary Billing Cycle & Exact Daily Proration
- Tenant rent cycles follow their anniversary check-in date or a calendar month alignment.
- Partial month stays use exact daily proration:
  $$\text{Prorated Paise} = \left\lfloor \frac{\text{Monthly Rent Paise} \times \text{Occupied Days}}{\text{Days in Month}} \right\rfloor$$
- All monetary arithmetic uses integer paise with truncation; floating point operations are strictly prohibited.

### 3. Departure Inspection & Dues Netting
- Departure inspection records damages with required authenticated evidence photo references.
- Outstanding unpaid dues are netted against the deposit balance:
  $$\text{Net Refund} = \text{Deposit Paid} - \sum \text{Deductions} - \sum \text{Unpaid Dues}$$
- If unpaid dues exceed the deposit balance, a final balance due invoice is generated.

## Consequences
- Clean balance sheet separation between operating cash and tenant deposit liabilities.
- Zero revenue overstatement.
- Audit trail for all departure deductions backed by photo hashes and inspection timestamps.
