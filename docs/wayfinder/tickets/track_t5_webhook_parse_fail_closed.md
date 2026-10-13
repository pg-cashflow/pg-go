# Ticket T5: Webhook Parse Errors Fail Closed (G6)

- **Track:** T5
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/cashfree/webhook.go`, `internal/cashfree/webhook_test.go`, `internal/api/handlers_pay_test.go`

## Acceptance Criteria
- [x] Parser fails closed on malformed amount strings or fractional rupee violations instead of silently falling back to 0:
  1. `PAYMENT_FAILED_WEBHOOK`: `cashfree.ParseWebhook` returns error on invalid decimal amount format (e.g. `5500.555`).
  2. `PAYMENT_DISPUTE_CREATED_WEBHOOK`: `cashfree.ParseWebhook` returns error on invalid decimal amount format (e.g. `12000.999`).
- [x] Gateway webhook handler routes parse errors to dead-letter storage:
  1. `TestCashfreeWebhook_MalformedAmountPaymentFailedDeadLetter`: asserts malformed amount in payment-failed payload updates event status to `dead_letter` and responds 200 OK to stop gateway retry storms.
  2. `TestCashfreeWebhook_MalformedAmountDisputeDeadLetter`: asserts malformed amount in dispute payload updates event status to `dead_letter` and responds 200 OK.
