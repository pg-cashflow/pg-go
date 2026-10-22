# Module: M5 Gateway & Reconciliation

**Sprint:** 5 · **ADR:** ADR-006, ADR-008, ADR-034

## RLS (R9-F1, R9-F2)

Implement bank account RLS and ScopedDB wiring per `RLS_D1_STATUS.md` during this sprint.

## Gate 8

- [x] Webhook and settlement routes on owner API
- [x] R9-F1 closed (`migrations/086_bank_accounts_rls.sql`, `BankAccountRepo` DBTX/ScopedDB, unit tests verified)
- [x] R9-F2 closed (`BankTransactionRepo` DBTX/ScopedDB wiring in `bootstrap.go`, unit tests verified)
