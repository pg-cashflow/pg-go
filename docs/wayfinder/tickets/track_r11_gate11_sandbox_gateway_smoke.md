# Ticket 27: Track R.11 — Gate 11: Scheduled Cashfree Gateway Sandbox Smoke Harness

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: None
- **Tier**: Weekly Scheduled + Pre-Release

## Objective

Automate end-to-end smoke testing against the live Cashfree Sandbox environment.
Verify API compatibility without slowing down PR pipelines or risking gateway flakiness in blocking builds.
Run `cmd/sandbox-smoke` on a scheduled weekly CI workflow and pre-release gate.

## Technical Context

`cmd/sandbox-smoke` exercises real network calls against `https://sandbox.cashfree.com/pg`.
It creates real orders, simulates test customer checkouts, and polls payment statuses.
Running external third-party calls in PR builds causes non-deterministic build breaks when third-party sandboxes experience downtime.
A dedicated scheduled workflow gives reliable API contract verification without impacting developer velocity.

## Karpathy Test Discipline

1. **Look at data first:**
   Run `cmd/sandbox-smoke` locally with valid sandbox credentials:
   `CASHFREE_APP_ID=... CASHFREE_SECRET_KEY=... go run ./cmd/sandbox-smoke`.
   Inspect the emitted JSON logs and HTTP response status codes.

2. **Check state at initialization:**
   Verify that `CASHFREE_APP_ID` and `CASHFREE_SECRET_KEY` exist in GitHub Secrets.
   Verify that `CASHFREE_ENV` is set to `"sandbox"`.

3. **Overfit one example:**
   Execute a single order creation call: `CreateOrder(ctx, 10000)`.
   Confirm Cashfree Sandbox returns an active `payment_session_id`.

4. **Compare with dumb baseline:**
   Verify the returned order amount matches the requested amount exactly in paise.

5. **Fix seeds:**
   Generate unique, time-stamped order IDs: `smoke_order_<timestamp>`.

6. **Change one thing at a time:**
   Test order creation first.
   Test payment status polling second.
   Test refund initiation third.

## STE-100 Implementation Steps

1. Review `cmd/sandbox-smoke/main.go`.
2. Ensure the command exits with code 0 on complete smoke success.
3. Ensure the command exits with code 1 and writes structured errors on gateway failure.
4. Create a dedicated GitHub Actions workflow `.github/workflows/gateway-smoke.yml`:
   - Trigger on `schedule` (Weekly on Monday 02:00 UTC).
   - Trigger on `workflow_dispatch` (Manual on-demand trigger).
   - Inject repository secrets `CASHFREE_SANDBOX_APP_ID` and `CASHFREE_SANDBOX_SECRET_KEY`.
   - Run `go run ./cmd/sandbox-smoke`.
5. Configure Slack or email notification if the weekly smoke test fails.

## Acceptance Criteria

- `cmd/sandbox-smoke` runs on a weekly automated schedule in GitHub Actions.
- PR checks never make live outbound network requests to Cashfree.
- Any breaking change in Cashfree Sandbox API raises an automated alert before release.
- The command completes in under 30 seconds.
