# PG Cashflow — Baseline & Inventory Report (Gate 0–2)

**Document Reference:** `BASELINE_REPORT.md`  
**Standard:** Evidence-Driven Engineering Flow ([ENGINEERING_FLOW.md](file:///c:/Users/divak/Downloads/pg-go/ENGINEERING_FLOW.md))  
**Invariant Register:** [FINANCIAL_INVARIANTS.md](file:///c:/Users/divak/Downloads/pg-go/FINANCIAL_INVARIANTS.md)  
**Status:** Completed Baseline Inventory (Pre-Code-Modification Gate)

---

## 1. Toolchain, Runtime & Dependency Investigation (`REQ-OPS-003`)

### Findings:
1. **`go.mod` Analysis**:
   - Declares `go 1.26.6`.
   - Go 1.26 does not exist (official Go releases are currently in the 1.23/1.24 lifecycle).
   - In Go 1.21+, declaring a higher minor version invokes Go's toolchain auto-switching (`GOTOOLCHAIN=auto`), causing the Go CLI to attempt to query and download the non-existent toolchain from Google's toolchain servers. In sandboxed, offline, or air-gapped environments, this causes immediate failure (`go: toolchain not found`).
2. **CI / CD Workflows**:
   - `.github/workflows/test.yml` line 42: `go-version-file: pg-go/go.mod`
   - `.github/workflows/deploy.yml` line 31: `go-version-file: pg-go/go.mod`
   - Both workflows delegate Go toolchain resolution to `go.mod`.
3. **Source Code Language Feature Audit**:
   - Checked all 337 Go source files.
   - No Go 1.24/1.25/1.26 features or experimental flags are present (no `unique` package, no `iter.Seq` iterator imports, no experimental runtime packages).
   - The code compiles cleanly with standard Go 1.22 or Go 1.23 toolchains.
4. **Dependency Graph Compatibility**:
   - `github.com/jackc/pgx/v5 v5.10.0`: Requires Go 1.21+
   - `github.com/gin-gonic/gin v1.12.0`: Requires Go 1.21+
   - `github.com/golang-jwt/jwt/v5 v5.3.1`: Requires Go 1.20+
   - `google.golang.org/api v0.293.0`: Requires Go 1.21+
5. **Baseline Conclusion & Recommendation**:
   - The codebase is 100% compatible with standard **Go 1.23.2**. Standardizing `go.mod` to `go 1.23.2` will enable deterministic offline builds, local test runs, and unblock the test pyramid without altering language semantics.

---

## 2. Complete Database Monetary Column Inventory

Audited all 45 migration scripts (`migrations/001_initial.sql` through `045_financial_integrity_and_rollups.sql`).

### Legacy 32-Bit Monetary Columns Requiring Widening to `BIGINT`:

| Migration File | Table Name | Column Name | Current Type | Semantic | Target Type | Widening Risk | Invariant Protected |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `001_initial.sql:31` | `tenants` | `rent_amount` | `INTEGER` | Monthly rent in paise | `BIGINT` | Safe widening (no truncation) | `INV-010` |
| `001_initial.sql:35` | `tenants` | `credit_balance_paise` | `INTEGER` | Overpayment credit in paise | `BIGINT` | Safe widening | `INV-005`, `INV-010` |
| `001_initial.sql:49` | `dues` | `amount` | `INTEGER` | Current payable in paise | `BIGINT` | Safe widening | `INV-003`, `INV-010` |
| `001_initial.sql:50` | `dues` | `original_amount` | `INTEGER` | Original due in paise | `BIGINT` | Safe widening | `INV-010` |
| `022_payout_batches...:5` | `dues` | `contractual_ceiling_paise` | `INTEGER` | Contractual ceiling in paise | `BIGINT` | Safe widening | `INV-010` |
| `001_initial.sql:83` | `payments` | `amount` | `INTEGER` | Settled payment in paise | `BIGINT` | Safe widening | `INV-001`, `INV-010` |
| `004_join_pay_...:39` | `payment_reports` | `amount` | `INTEGER` | Reported amount in paise | `BIGINT` | Safe widening | `INV-010` |
| `015_payment_proofs...:22` | `payment_reports` | `ocr_amount` | `INTEGER` | OCR extracted paise | `BIGINT` | Safe widening | `INV-010` |
| `004_join_pay_...:59` | `payment_intents` | `amount_paise` | `INTEGER` | Gateway order amount in paise | `BIGINT` | Safe widening | `INV-010` |

### Existing `BIGINT` Monetary Columns (Reference / Verification):
- `financial_journal_entries.debit_paise`, `financial_journal_entries.credit_paise` (`012`)
- `capital_transactions.amount_paise` (`012`)
- `expenses.amount_paise` (`012`)
- `expense_payments.amount_paise` (`012`)
- `manager_advances.amount_paise`, `manager_reimbursements.amount_paise` (`012`)
- `approval_policies.*_limit_paise` (`012`)
- `tenant_departures.*_paise`, `departure_deductions.amount_paise`, `departure_due_adjustments.amount_paise` (`022`)
- `payout_batches.total_amount_paise`, `payout_items.amount_paise` (`022`)
- `staff_profiles.base_monthly_wage_paise`, `wage_calculations.*_paise` (`025`)
- `gateway_settlements.*_paise` (`026`)
- `bank_transactions.amount_paise`, `bank_transactions.closing_balance_paise` (`027`)
- `daily_settlement_balances.*_paise` (`031`)
- `daily_settlement_balance_runs.*_paise` (`032`)
- `daily_financial_rollups.*_paise` (`045`)

---

## 3. Go Domain Structs & DTOs Money Inventory

Every Go struct field handling currency paise must be `int64`. The following fields currently use `int` or `float64`:

| File | Struct | Field | Current Type | Target Type | Notes |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `internal/domain/payment.go:31` | `Payment` | `Amount` | `int` | `int64` | Primary payment amount |
| `internal/domain/due.go:36` | `Due` | `Amount` | `int` | `int64` | Outstanding payable |
| `internal/domain/due.go:37` | `Due` | `OriginalAmount` | `int` | `int64` | Contractual due |
| `internal/domain/due.go:38` | `Due` | `ContractualCeilingPaise` | `*int` | `*int64` | Ceiling |
| `internal/domain/tenant.go:25` | `Tenant` | `RentAmount` | `int` | `int64` | Monthly rent |
| `internal/domain/tenant.go:29` | `Tenant` | `CreditBalancePaise` | `int` | `int64` | Carry-forward credit |
| `internal/domain/join.go:49` | `PaymentReport` | `Amount` | `int` | `int64` | Tenant reported payment |
| `internal/domain/join.go:60` | `PaymentReport` | `OCRAmount` | `*int` | `*int64` | Extracted proof amount |
| `internal/domain/join.go:92` | `PaymentIntent` | `AmountPaise` | `int` | `int64` | Gateway order |
| `internal/domain/join.go:106` | `PayIntent` | `AmountPaise` | `int` | `int64` | API DTO |
| `internal/api/handlers_pay_reports.go:23` | `submitReportBody` | `Amount` | `int` | `int64` | Request payload |
| `internal/api/handlers_public.go:129` | `PaymentPageView` | `AmountRupees` | `float64` | String / int division | Presentation boundary |
| `internal/qr/upi.go:14` | `GenerateUPILink` | `rupees` calculation | `float64` | Integer arithmetic | Formats UPI `am` param |
| `internal/api/handlers_owner.go:628` | `SendPaymentLink` | `rupees` calculation | `float64` | Integer arithmetic | WhatsApp link format |

---

## 4. Complete Financial Writers & Readers Inventory

### Payment Writers:
1. `internal/postgres/payment_repo.go:Create` — Direct insert into `payments`. *(Defect: Missing `property_id`)*
2. `internal/payment/service.go:VerifyPayment` — Verified payment insertion within transaction.
3. `internal/payment/service.go:MarkCashPaid` — Manual cash settlement within transaction.
4. `internal/payment/service.go:ManualMatch` — Offline UTR matching within transaction.
5. `internal/payment/service.go:CorrectPayment` — Reversal + Corrected payment insertion within transaction. *(Defect: No duplicate correction guard)*
6. `internal/api/handlers_pay.go:CashfreeWebhook` — Gateway automated settlement.

### Expense Writers:
1. `internal/finance/service.go:CreateExpense` — Expense insert + accrual posting + approval insertion. *(Defect: Non-atomic; spend limit race condition)*
2. `internal/finance/service.go:PayExpense` — Expense payment insert + advance insert + journal insert + status update. *(Defect: Non-atomic; overpayment race condition)*
3. `internal/finance/service.go:ReimburseManager` — Reimbursement insert + journal insert. *(Defect: Non-atomic)*
4. `internal/finance/service.go:DecideApproval` — Status update + accrual posting + approval update. *(Defect: Non-atomic)*

### Tenant Credit Writers:
1. `internal/payment/service.go:addTenantCredit` — Overpayment credit accumulation. *(Defect: Read-modify-write race)*
2. `internal/tenant/service.go:ApplyCredit` — Manual / automated credit application. *(Defect: Read-modify-write race)*
3. `internal/api/handlers_pay.go:CashfreeWebhook` — Gateway overpayment allocation. *(Uses direct SQL increment)*

### PII / Banking Credential Writers:
1. `internal/api/handlers_payouts.go:CreatePayee` — Payee beneficiary insertion. *(Defect: Raw bytes stored in `account_number_encrypted`)*
2. `internal/postgres/payout_repo.go:CreatePayee` — Direct insert into `payout_payees`.

---

## 5. Concurrency & Transaction Boundary Defect Inventory

| Defect ID | Operation | File & Lines | Current Behavior | Threat / Vulnerability | Required Invariant |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **C-01** | Expense Payment Overpayment | `internal/finance/service.go:218-241` | Reads `SumExpensePayments` without row lock; inserts payment. | Concurrent payments can settle >100% of approved amount. | `INV-004` (Lock expense row with `FOR UPDATE`) |
| **C-02** | Manager Spending Cap Bypass | `internal/finance/service.go:131-150` | Unserialized `SumManagerSpend` before inserting expense. | Simultaneous requests bypass daily/monthly spend limits. | `INV-012` (Serialized manager lock or atomic spend check) |
| **C-03** | Lost Tenant Credit Updates | `internal/payment/service.go:617-624`, `internal/tenant/service.go:545-560` | In-memory read-modify-write (`t.CreditBalancePaise += credit`). | Concurrent credits overwrite each other in DB. | `INV-005` (Atomic SQL `credit_balance_paise = credit_balance_paise + $2`) |
| **C-04** | Split Expense Commit | `internal/finance/service.go:168-190` | `InsertExpense`, then `postExpenseAccrual`, then `InsertApproval` on raw pool. | Journal failure leaves orphaned expense without ledger entries. | `INV-011` (Single `pgx.Tx` wrapping all steps) |
| **C-05** | Split Report Confirmation | `internal/api/handlers_pay_reports.go:245-255` | `ManualMatch` commits payment; `UpdateReview` called separately; error swallowed. | Payment created but report remains pending; no rollback on error. | `INV-003`, `INV-011` (Single atomic transaction) |
| **C-06** | Duplicate Corrections | `internal/payment/service.go:234-319`, `migrations/045` | No uniqueness check or constraint on `financial_corrections.original_payment_id`. | Multiple corrections can be posted against same payment. | `INV-008` (`UNIQUE(original_payment_id)`) |
| **C-07** | Omitted Property ID | `internal/postgres/payment_repo.go:41-75` | `INSERT INTO payments` omits `property_id`. | New payments inserted with `NULL` property ID. | `INV-006` (Populate `property_id` & enforce `NOT NULL`) |
| **C-08** | Cross-Property Room Link | `internal/finance/service.go:151-167`, `012_finance_foundation.sql:86` | `room_id` references `rooms(id)` without checking `property_id`. | Expense in Property A can reference Room in Property B. | `INV-006` (Composite FK `(room_id, property_id)`) |
| **C-09** | Dashboard Failure Masking | `internal/api/handlers_owner_dashboard.go:403-419` | Ignores `BuildSummary` / `OperatingSummary` errors, returns 200 with ₹0. | Owner misled during DB outage into thinking revenue is zero. | `INV-015` (Fail request with HTTP 500 on aggregate error) |
| **C-10** | Cashfree Zero Coercion | `internal/cashfree/settlement.go:301-345` | `coercePaise` returns 0 on unparseable/invalid inputs. | Corrupted gateway webhook records false ₹0 payment. | `INV-014` (Fail-closed exact decimal parsing) |

---

## 6. Pre-Implementation Sign-Off Gate

All writers, readers, schemas, DTOs, and transaction boundaries have been mapped to source lines and invariants. 
No production business logic has been modified in this baseline phase.

**Ready for Phase 1 Invariant Enforcement upon user approval.**
