# ADR-034: Payout Webhook Parity and Metrics Fail-Closed

**Status:** Accepted
**Date:** 2026-10-14
**Standard:** ASD-STE100 (Simplified Technical English)
**Method:** Empirical First-Principles Verification (Karpathy Method)
**Builds on:** ADR-023, ADR-031

## Context

Payment webhooks write `webhook_events` before they change money state.
Payout webhooks verified HMAC and then changed payout state with no audit row.
`/metrics` accepted an owner JWT when `METRICS_SECRET` was empty.
The metrics handler also read `METRICS_TOKEN`. Two checks disagreed.

## Gate 0 requirements

```text
REQ-WH-001
The system shall persist every verified Cashfree payout webhook to webhook_events before mutation.

REQ-WH-002
The system shall record payout webhook processing in processed_webhook_events inside the same database transaction as payout state changes.

REQ-WH-003
A duplicate payout webhook shall return HTTP 200 and shall not change payout status or ledger state twice.

REQ-SEC-003
The /metrics and /api/metrics endpoints shall reject unauthenticated requests when METRICS_SECRET is unset, unless METRICS_ALLOW_OPEN=true.

REQ-SEC-003b
The metrics handler shall not apply a second auth layer. Middleware is the only gate.
```

## Decision

1. Persist a `webhook_events` row after HMAC and timestamp checks. Fail with HTTP 503 when no audit store exists.
2. Insert `processed_webhook_events` and the payout status update in one transaction when a pool is present.
3. Return HTTP 200 on a duplicate event. Do not update status again.
4. Protect `/metrics` with `X-Metrics-Token` equal to `METRICS_SECRET`. Do not accept a JWT on this route.
5. Require `METRICS_SECRET` when `APP_ENV=production`.
6. Keep `METRICS_ALLOW_OPEN=true` as the only explicit open switch for local development.

## Verification

1. Look at `webhook_events` and `processed_webhook_events` after one signed payout webhook.
2. Send the same webhook again. Assert HTTP 200. Assert one processed row.
3. Call `GET /metrics` with no token. Assert HTTP 401.
4. Call `GET /metrics` with `X-Metrics-Token`. Assert HTTP 200.
5. Run `go test ./internal/api -run "PayoutWebhook|Metrics" ./internal/config -count=1`.
