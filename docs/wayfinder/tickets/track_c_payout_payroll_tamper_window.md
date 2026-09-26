# Ticket 2: Track C — Payout & Payroll Tamper Window (Approval to Bank Upload)

- **Type**: `wayfinder:research`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Resolved By**: Track C.1

## Question

Does the payout/payroll subsystem protect against tampering and data-leakage during the window between owner batch approval, instruction sheet CSV export, and bank upload? Specifically:
1. Are batch file checksums (`domain.ComputeBatchChecksum`) tamper-evident and validated?
2. Are payout items and payee bank account identifiers protected against IDOR, enumeration, and in-flight mutation once batched?
3. Does the CSV export header and row formatting strictly prevent formula injection (CSV injection) and enforce integer paise math?

## Resolution

1. **Item & Batch Immutability**:
   - Confirmed: Exactly one `UPDATE payout_items` statement exists in the entire repository (`internal/postgres/payout_repo.go:468`), which assigns `batch_id` atomically at batch creation time.
   - Zero `DELETE FROM payout_items` and zero `UPDATE payout_batches` statements exist anywhere in the codebase. Batched items are strictly immutable and append-only.
2. **Property Isolation**:
   - Confirmed: `internal/postgres/payout_repo.go:393-401` enforces `JOIN payout_payees pp ON pp.id = pi.payee_id WHERE pp.property_id = $1` with `FOR UPDATE OF pi` row locking. Cross-property item bundling is impossible by construction.
3. **Active Checksum Verification Gate (Track C.1)**:
   - In `internal/api/handlers_payouts.go:490-505`, `OwnerExportPayoutBatch` recomputes `domain.ComputeBatchChecksum` against current DB items using the checksum secret.
   - Compares with stored `batch.FileChecksum` using constant-time `hmac.Equal`.
   - On mismatch, emits `slog.Error` alerting on potential DB tampering or out-of-band mutation and blocks the export with `409 Conflict`.
4. **CSV Formula Injection Mitigation (Track C.1)**:
   - Implemented `sanitizeCSVCell`: prepends `'` to any cell starting with `=`, `+`, `-`, `@`, `\t`, or `\r`.
   - Applied to all free-text fields in exported rows: `ReferenceNumber`, `Purpose`, `PeriodLabel`, and `UTR`.
5. **Integer-Paise Money Math (Track C.1)**:
   - Replaced `float64(it.AmountPaise)/100.0` with pure integer formatting (`formatPaiseToINR(paise int64) string` -> `%s%d.%02d`).
