# ADR 001: Reconciliation Control Total, Dual Summaries, and Tie-Out Discipline

## Status
Accepted

## Context
The property management platform has an existing, authoritative collections engine centered on `payment.ReconciliationSummary` (`GET /api/owner/reconciliation`), tracking `rent_collected_paise`, `outstanding_rent_paise`, `credits_held_paise`, and `deposits_held_paise`.

Introducing operating cashflow (OCF), break-even, and ROI calculations requires tracking expenses, capital transactions, and adjustments in a financial journal. Displaying conflicting "revenue" figures without clear reconciliation would destroy owner trust.

## Decisions

1. **Reconciliation as Authoritative Control Total**:
   - `ReconciliationSummary` figures remain the authoritative control total for Tier-1 collections.
   - Operating P&L / OCF is computed from double-entry lines in `financial_journal_entries` and classified OPEX.
   - Owner UI must never show two unqualified revenue numbers. Labels are explicit:
     - `"Collections (reconciliation)"`
     - `"Operating view (ledger)"`

2. **Distinct API Endpoints (No Collision)**:
   - `GET /api/owner/finance/tie-out?period=YYYY-MM`: Reconciles collections control (`ReconciliationSummary.RentCollected`) against journal collection lines (`rent_revenue`). Label: `"Collections tie-out"`.
   - `GET /api/owner/finance/variance-bridge?period=YYYY-MM`: Decomposes plan vs actual OCF by business driver (`occupancy_volume`, `rate_rent_per_bed`, `collection`, `food_cost`, `electricity_cost`, `reward_liability`, `payment_processing_tdr`). Label: `"Plan vs actual bridge"`.
   - The term `reconciliation-bridge` is intentionally forbidden to avoid confusion with `GET /api/owner/reconciliation`.

3. **Strict Monthly Close Gate**:
   - Each monthly period requires a `period_tie_outs` record.
   - `POST /api/owner/finance/tie-out/close` strictly blocks with an error (`ErrPeriodNotCloseable`) if there is any unexplained difference (`difference_paise != 0`).
   - Categorized bridge items (`timing`, `adjustment`, `proration`, `reward_credit`, `investigate`) account for expected deviations.

4. **Dual-Signal Discrepancy Escalation**:
   - **Immediate Magnitude Alert**: Any single monthly period where unreconciled difference `|difference_paise| >= ₹5,000` (500,000 paise) immediately triggers an urgent operator alert (`EvtCriticalTieOutVariance`) directing the owner to `/owner/finance/tie-out`.
   - **Chronic Recurrence Escalation**: Bridge line items categorized as `investigate` (or recurring unexplained discrepancies from the same source) that persist unresolved across 2 consecutive closed monthly periods trigger an automatic Level 3 alert (`EvtRecurringTieOutException`).
   - These alerts publish actionable notifications with `is_action_required: true` directing the owner to `/owner/finance/tie-out`. Neither auto-closes periods.

5. **Official ROI Snapshot Pre-requisite**:
   - An ROI snapshot for a period is only marked `official: true` after the corresponding monthly tie-out is successfully closed.
