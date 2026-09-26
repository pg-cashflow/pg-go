# Ticket 1: Track B.2 — Cashfree Isolation, TDR Absorption & Gateway Error Fail-Safe

- **Type**: `wayfinder:research`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Resolved By**: Track 0.5

## Question

Does the Cashfree gateway implementation maintain strict environment isolation (`CASHFREE_ENV`), guarantee owner-absorbed TDR (zero fee surcharging on tenants), provide fail-safe recovery on gateway 5xx/timeouts during refunds without permanently locking due balances, and sanitize all error responses on refund initiation?

## Resolution

1. **`CASHFREE_ENV` Isolation**:
   - In `internal/cashfree/client.go:29-34`, base URL selects `https://api.cashfree.com/pg` when `strings.EqualFold(c.Env, "production")` and `https://sandbox.cashfree.com/pg` otherwise.
   - In `internal/config/validate.go:92-93`, startup emits an explicit warning when running in sandbox mode. No hardcoded hostnames exist in application code.
2. **TDR Absorption Policy**:
   - In `internal/payment/cashfree_adapter.go:35` and `internal/cashfree/client.go:80-84`, `OrderAmount: float64(amountPaise) / 100.0` passes the exact due amount without markups or surcharge pass-through.
   - In `internal/finance/reports.go:172` and `docs/finance_roi.md:27`, gateway fee is posted to `5020 payment_processing_expense` (`domain.AcctPaymentProcessingExpense`) as an owner-absorbed OPEX cost.
3. **Gateway Refund Fail-Safe & Network Timeout Recovery**:
   - In `internal/api/handlers_pay.go:1350-1365`, when `CreateRefund` fails with 5xx/network error, `refund_allocations` are deleted transactionally (`DELETE FROM refund_allocations WHERE refund_id = $1`) so refundable headroom is released rather than frozen.
   - In `internal/api/handlers_pay.go:860-985` (`handleRefundWebhook`), incoming `REFUND_STATUS_WEBHOOK` events look up ancestry by `cf_refund_id` or `cf_payment_id`. If a refund timed out on the client side but succeeded upstream, subsequent webhook delivery safely transitions the refund to `succeeded` and applies lock-ordered reversals.
4. **H2 Sanitization Applied (Track 0.5)**:
   - Line 1336 sanitized from `"refund initiation failed: " + err.Error()` to `respondErr(c, err)`.
   - Line 1364 sanitized from `"cashfree refund failed: " + cfErr.Error()` to `respondErr(c, clientErr(http.StatusBadGateway, "gateway refund failed"))`.
