# Ticket 29: Track R.13 — Gate 13: Calendar Month-End Clamping, Leap Year & Timezone Invariants

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: None
- **Tier**: PR Gate (< 10 min)

## Objective

Validate date calculation invariants across month boundaries, leap years, and Indian Standard Time (IST).
Ensure due-day clamping correctly handles short months (February 28/29, April 30).
Ensure billing cycles do not skip months or create duplicate dues across UTC vs IST midnight transitions.

## Calendar Edge Cases

1. **Month-end Day Clamping**:
   - Tenant due day set to 31st.
   - February billing must clamp to 28th (non-leap) or 29th (leap year).
   - April, June, September, November must clamp to 30th.
2. **Leap Year Transitions**:
   - Transition from February 28 to February 29 in leap years.
   - Transition across leap day in proration calculations.
3. **IST Timezone Boundary**:
   - Indian Standard Time is UTC+05:30.
   - A payment received at 00:15 IST (18:45 UTC previous day) belongs to the IST calendar day.

## Karpathy Test Discipline

1. **Look at data first:**
   Inspect `internal/billing/due.go` and `internal/finance/prorate.go`.
   Find all calls to `time.Now()` and timezone conversions.

2. **Check state at initialization:**
   Introduce an injectable mock clock interface: `type Clock interface { Now() time.Time }`.

3. **Overfit one example:**
   Set mock clock to `2024-02-29 00:00:00 +0530 IST` (Leap day).
   Generate cycle dues for tenant with due day 31.
   Assert generated due date is `2024-02-29`.

4. **Compare with dumb baseline:**
   Compare computed due dates with a reference lookup table for all 12 months.

5. **Fix seeds:**
   Deterministic timestamps in table-driven tests.

6. **Change one thing at a time:**
   Test month-end clamping first.
   Test leap year transitions second.
   Test IST vs UTC date calculations third.

## STE-100 Implementation Steps

1. Create test file `internal/billing/calendar_invariants_test.go`.
2. Test 1 (Due Day Clamping Table):
   - Test due day 31 across all 12 months.
   - Assert Jan 31, Feb 28 (or 29), Mar 31, Apr 30, May 31, Jun 30, Jul 31, Aug 31, Sep 30, Oct 31, Nov 30, Dec 31.
   - Assert zero panics and zero invalid date errors.
3. Test 2 (Proration Math Across February):
   - Tenant checks in on February 20th in a 28-day month.
   - Verify remaining days equal 9 days (Feb 20 to Feb 28 inclusive).
   - Assert proration fraction equals `9 / 28`.
   - Assert integer-paise rounding does not lose 1 paise.
4. Test 3 (IST Midnight Boundary):
   - Set server time to `2026-10-08 23:59:00 +0530`.
   - Advance clock by 2 minutes to `2026-10-09 00:01:00 +0530`.
   - Assert day advances correctly in local reports and daily rollups.

## Acceptance Criteria

- Due day clamping generates valid calendar dates for all 12 months.
- February 29 is generated correctly during leap years and rejected during non-leap years.
- Daily financial rollups partition strictly by IST date (`Asia/Kolkata`), not UTC.
- All tests execute in under 1 second.
