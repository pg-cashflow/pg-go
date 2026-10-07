# ADR-021: Scale Capacity, Gamification Economics, and Expense-Void Defect Remediation

**Status:** Accepted  
**Date:** 2026-10-07  
**Basis:** `pg-go-div_dev` following ADR-020 review  
**Deciders:** Owner / Engineering Lead  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification  

---

## 1. Context

Architecture analysis against operational scale (+100 tenants/year, 500 tenants at Y5, ₹3.3 Cr annual run-rate) established:
1. **Capacity Evaluation:** Cumulative 5-year volume is ~2.7 million rows with peak throughput <0.05 payments/sec. A single PostgreSQL primary with PITR backups easily supports this workload. Complex sharding/replicas from ADR-017 remain parked.
2. **Defect F1 (Approval Resurrection):** Voiding an expense with a pending approval left the approval request open. A subsequent owner approval restored the cancelled expense to `approved` and posted journal lines.
3. **Defect F2 (Void Race Condition):** Reversal decisions were made outside database row locks, allowing race conditions with concurrent payments or approvals.
4. **Defect F3 (Audit and Maker-Checker Gap):** Void reason was not persisted, and managers could void approved expenses above owner thresholds.
5. **Defect F5 (Gamification Fail-Open):** Settings read errors defaulted to arbitrary values instead of failing closed.
6. **Defect F6 (Unindexed Month Queries):** `TO_CHAR(created_at, 'YYYY-MM')` prevented index usage and shifted month boundaries across time zones.

---

## 2. Decisions

### D1. Migrations 059 & 060 (Void Audit Trail & Covering Ledger Indexes)
- Migration 059 (`059_expense_void_audit.sql`):
  - Add columns to `expenses`: `void_reason TEXT`, `voided_by UUID REFERENCES users(id)`, `voided_at TIMESTAMPTZ`.
  - Add check constraint `chk_expense_void_audit` enforcing all three fields populated together and reason length between 3 and 500 characters.
- Migration 060 (`060_points_ledger_month_indexes.sql`):
  - Add covering index `idx_points_ledger_tenant_month` on `points_ledger (tenant_id, created_at) INCLUDE (delta, rule_code)`.
  - Add covering index `idx_points_ledger_property_month_cov` on `points_ledger (property_id, created_at) INCLUDE (delta)`.
  - Drop redundant plain index `idx_points_ledger_property_month`.

### D2. Approval Resurrection Prevention (F1)
- In `VoidExpenseAtomic`, cancel all pending approvals for the expense:
  `UPDATE approval_requests SET status = 'cancelled', note = COALESCE(note || ' [voided]', 'voided') WHERE kind = 'expense' AND subject_id = $1 AND status = 'pending'`.
- In `DecideApprovalAtomic`, guard expense update:
  `UPDATE expenses SET status = $2 WHERE id = $1 AND status = 'pending_approval'`.
  If zero rows match, return error (`ErrForbidden`), rejecting approval of non-pending expenses.

### D3. Concurrency Protection & In-Transaction Reversal (F2)
- Move state validation and reversal line generation inside the transaction block with row lock:
  `SELECT ... FROM expenses WHERE id = $1 FOR UPDATE`.
- Verify status is not `cancelled` or `paid`, and verify zero payments in `expense_payments`.
- If status is `approved`, generate and insert reversing journal entries inside the transaction:
  - Debit: `accounts_payable`
  - Credit: `operating_expense`
- Update status to `cancelled` with audit fields under row lock.

### D4. Maker-Checker Authorization (F3)
- Require non-empty void reason.
- Enforce policy check: managers cannot void approved expenses exceeding `OwnerApprovalThresholdPaise`.

### D5. Fail-Closed Cash Reward Redemptions (F5)
- Terminate with error if property settings cannot be loaded or if `PointValuePaise <= 0`.
- Clamp discount paise strictly to `reward.PointsCost * settings.PointValuePaise`.

### D6. Time-Zone Safe Range Queries (F6)
- Replace `TO_CHAR(created_at, 'YYYY-MM')` in `GetTenantMonthPoints`, `GetPropertyMonthPoints`, and `GetRuleMonthPoints`.
- Parse month bounds in Indian Standard Time (IST) and query `created_at >= $from AND created_at < $to` using b-tree indexes.

### D7. Comprehensive Test Suite (F4)
- Add unit and invariant test suite in `internal/finance/void_test.go` covering:
  - Approved expense void nets ledger to zero.
  - Pending approval void leaves zero journal entries and cancels approval.
  - Approval of voided expense is blocked.
  - Double-void and voiding paid expenses are rejected.
  - Maker-checker threshold enforcement.
  - Concurrent void races with exactly one winner.
  - Expense date range validation (past 90 days / future skew).

---

## 3. Consequences

### Positive
- Expense voiding is mathematically safe against concurrent payments and approvals.
- Cancelled expenses cannot be resurrected via stale approval requests.
- Full auditability (`void_reason`, `voided_by`, `voided_at`) enforced by database check constraints.
- Gamification redemption fails closed against configuration errors.
- Ledger queries leverage b-tree covering index scans instead of table scans.

---

## 4. Verification Matrix

| Target | Description | Status |
| :--- | :--- | :--- |
| `migrations/059_expense_void_audit.sql` | Added audit columns and length-bounded check constraint | VERIFIED |
| `migrations/060_points_ledger_month_indexes.sql` | Added covering month range indexes on points_ledger | VERIFIED |
| `internal/postgres/finance_repo.go` | Added row-locking `VoidExpenseAtomic` and guarded `DecideApprovalAtomic` | VERIFIED |
| `internal/finance/mem.go` | Implemented in-memory store void logic with approval cancellation | VERIFIED |
| `internal/finance/service.go` | Enforced maker-checker threshold and reason validation in `VoidExpense` | VERIFIED |
| `internal/gamification/redemptions.go` | Implemented fail-closed cash credit validation | VERIFIED |
| `internal/postgres/gamification_repo.go` | Switched to IST range bounds and b-tree index queries | VERIFIED |
| `internal/finance/void_test.go` | Comprehensive test suite covering all void invariants and concurrency | VERIFIED |
