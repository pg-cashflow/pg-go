# ADR-031: Webhook Signing Secret Policy (Cashfree PG and Payouts)

**Status:** Accepted
**Date:** 2026-10-14
**Deciders:** Sasi
**Builds on:** Track B (gateway ingress), ADR-028
**Resolves:** M12 finding F2, and the amendment to F5 for `CF_PAYOUT_WEBHOOK_SECRET`

## Context

- `config.go:114` reads `CASHFREE_PG_WEBHOOK_SECRET`, then `CASHFREE_WEBHOOK_SECRET`.
- `config.go:141-143` copies `CASHFREE_PG_SECRET_KEY` into the webhook secret when both are empty.
- Cashfree documents that PG webhook signatures use the **PG client secret key** ("you need your Cashfree Payment Gateway secret key along with the payload"): HMAC-SHA256 over timestamp + raw body, Base64. There is no separate PG webhook secret. Sources: `cashfree.com/docs/api-reference/payments/latest/webhooks/signature-verification` and `docs.cashfree.com/docs/test-and-validate-webhooks-1`.
- So the fallback is the **documented behavior**, not a weakness. The audit finding F2 treated it as one.
- Side effects in the code today: the L1 warning (`validate.go:144`) fires on every correct setup; the "required" errors (`config.go:183`, `validate.go:110`) fire only when the PG secret is absent and name the wrong variable.
- `CF_PAYOUT_WEBHOOK_SECRET` is a different credential. Its issuer and format were **not verified** here. F5 applied a 32-character minimum to it. A vendor-issued value shorter than 32 characters would block boot.

## Decision

1. **Keep the fallback.** PG webhook secret = `CASHFREE_PG_WEBHOOK_SECRET` if set (override, for key rotation windows), else the PG secret key.
2. **Remove the L1 warning.** Replace with a startup log line naming which source was used (`webhook_secret_source=pg_secret_key|override`), never the value.
3. **Fix the error text** at `config.go:183` and `validate.go:110` to name `CASHFREE_PG_SECRET_KEY`.
4. **Payout webhook secret:** required when payout credentials are set (unchanged); placeholder check stays; **drop the 32-character minimum for this one secret**. Keep the minimum for secrets the operator generates (JWT, OTP, magic link, payout encryption and checksum, metrics).
5. Before production, check the real payout webhook secret in the Cashfree Payouts dashboard and confirm its format. If it is at least 32 characters, the minimum can return.

## Options Considered

| Option | Complexity | Risk |
|---|---|---|
| A. Require an explicit webhook secret in every environment | Low | **Breaks webhook verification**, since Cashfree signs with the PG secret and no other value exists to configure |
| B. Keep fallback, clean up checks and messages (selected) | Low | None found |
| C. Do nothing | None | Noisy warning trains the operator to ignore warnings; wrong variable in errors |

## Consequences

- Easier: a correct config starts silently; warnings regain meaning.
- Harder: nothing.
- Revisit: if Cashfree introduces a separate PG webhook secret (re-read the docs at each API version bump; `CASHFREE_API_VERSION` is `2025-01-01`).

## Action Items

1. [x] R1: edit messages, remove L1, add tests: fallback resolves to PG secret; override wins; both empty with PG keys set is impossible; both empty with no PG keys is allowed.
2. [x] R2: payout secret placeholder-only test; the other secrets keep the 32-char test.
