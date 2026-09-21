# ADR 003: Dynamic DB-Checked Spend Policies vs Static JWT Claims

## Status
Accepted

## Context
Managers have daily, monthly, and single-transaction spend limits, as well as emergency spend bypass rules. The platform uses 30-day session JWT tokens signed via Firebase exchange (`token_version` model for revocation). Embedding financial limit thresholds directly into JWT claims would cause stale enforcement if an owner adjusts thresholds mid-month, or force frequent session invalidation.

## Decisions

1. **Database-Backed Enforcement Per Request**:
   - Spend limits and approval thresholds are stored in the database table `approval_policies`:
     - `manager_daily_limit_paise`
     - `single_expense_limit_paise`
     - `manager_monthly_limit_paise`
     - `owner_approval_threshold_paise`
     - `reimbursement_threshold_paise`
     - `emergency_bypass_enabled`
   - Every mutation checks the current policies directly in the database per request, computing accumulated daily and monthly spend in real time.

2. **JWT Stability**:
   - JWT tokens remain static for the session lifetime, containing only invariant identity claims (`sub`, `role`, `property_id`, `token_version`).
   - Policy adjustments by the owner take effect immediately without requiring manager re-login.

3. **Maker-Checker Workflow**:
   - Expenses created by managers that exceed thresholds are automatically set to `status='pending_approval'` and insert an `approval_requests` entry.
   - Emergency expenses bypass immediate owner approval if `emergency_bypass_enabled=true`, but trigger audit events (`EvtExpenseApproved` with emergency flag).
