# M7 Go-Live Checklist (Payouts & Payroll)

Use this list before enabling payouts in production.

1. Set `CF_PAYOUT_CLIENT_ID`, `CF_PAYOUT_CLIENT_SECRET`, `CF_PAYOUT_FUNDSOURCE_ID`, and `CF_PAYOUT_WEBHOOK_SECRET`.
2. Set `PAYOUT_ENCRYPTION_SECRET` and `PAYOUT_CHECKSUM_SECRET` (distinct from `JWT_SECRET`).
3. Confirm `property_settings.active_modules["payouts"]` is true for the property.
4. Run maker-checker test: manager cannot approve batches (owner only).
5. Run step-up OTP test on solo-owner property (`POST .../approve/request-otp`).
6. Verify `pgapp_worker` jobs use `DATABASE_WORKER_URL`.
7. Complete R9-F3 and R9-F4 from `RLS_D1_STATUS.md` before multi-property production.
