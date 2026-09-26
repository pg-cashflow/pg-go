# Ticket 2: Track C — Payout & Payroll Tamper Window (Approval to Bank Upload)

- **Type**: `wayfinder:research`
- **Status**: Open (Claimed next)
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Question

Does the payout/payroll subsystem protect against tampering and data-leakage during the window between owner batch approval, instruction sheet CSV export, and bank upload? Specifically:
1. Are batch file checksums (`domain.ComputeBatchChecksum`) tamper-evident and validated?
2. Are payout items and payee bank account identifiers protected against IDOR, enumeration, and in-flight mutation once batched?
3. Does the CSV export header and row formatting strictly prevent formula injection (CSV injection) and enforce integer paise math?
