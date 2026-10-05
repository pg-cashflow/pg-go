# PG Cashflow — Financial Invariant Register

This document is the authoritative register of financial, ledger, and transactional invariants for the PG Cashflow system. 

Every requirement, schema change, service implementation, database constraint, and test suite must map to one or more invariants defined herein. **No pull request or deployment may weaken, bypass, or violate any invariant in this register.**

---

## Invariant Index

| Invariant ID | Title | Enforcement Level | Primary Target |
| :--- | :--- | :--- | :--- |
| **INV-001** | Idempotent Payment Creation | DB + Application | `payments`, `payment_intents` |
| **INV-002** | Normalized UTR Uniqueness | DB Constraint | `payments(normalized_utr)` |
| **INV-003** | Atomic Financial Effect | Database Transaction | Payments, Dues, Journal |
| **INV-004** | Expense Settlement Bound | DB Lock + Constraint | `expenses`, `expense_payments` |
| **INV-005** | Lossless Credit Mutation | Atomic SQL Update | `tenants.credit_balance_paise` |
| **INV-006** | Strict Property Scoping | DB Constraint + App | All property-scoped tables |
| **INV-007** | Ledger Immutability | DB Triggers / Append-Only | `journal_entries`, `accounts` |
| **INV-008** | Single Correction Determinism | DB Constraint / State | `financial_corrections` |
| **INV-009** | Read-Model Reconciliation | Batch & Rollup Reconciler| `daily_financial_rollups` |
| **INV-010** | Universal Integer-Paise Arithmetic | DB Schema + Go Types | Entire Codebase |
| **INV-011** | Zero-Partial-State Atomicity | Transaction Boundary | All Financial Operations |
| **INV-012** | Manager Spending Cap Inviolability | Concurrency Serialization | `expenses`, `manager_spend` |
| **INV-013** | Sensitive Banking Data Encryption | Authenticated Envelope Enc | `payout_payees` |
| **INV-014** | Fail-Closed Gateway Parsing | Strict Deserialization | Cashfree Webhooks & Ingestion |
| **INV-015** | Non-Zero Failure Transparency | HTTP Handler Contracts | Owner & Manager Dashboards |

---

## Detailed Invariant Specifications

### INV-001 — Idempotent Payment Creation
> *A payment shall not be created twice for the same idempotency identity.*
* **Scope**: Manual payment verification, Cashfree webhook ingestion, offline UTR entry.
* **Invariant**: Providing the same idempotency key or webhook event ID must return the existing payment record and execute zero additional state transitions.
* **Enforcement**: Database unique constraint on `idempotency_key` or `event_id`, verified within the payment creation transaction.

### INV-002 — Normalized UTR Uniqueness
> *A normalized UTR shall not identify two independent verified payments within its uniqueness scope.*
* **Scope**: All payment receipts and manual verifications.
* **Invariant**: Bank reference numbers (UTR) must be trimmed, uppercased, and stripped of non-alphanumeric noise. No two payments may share the same normalized UTR.
* **Enforcement**: `UNIQUE INDEX idx_payments_normalized_utr ON payments(normalized_utr) WHERE normalized_utr IS NOT NULL AND status = 'verified'`.

### INV-003 — Atomic Financial Effect
> *A verified payment shall have exactly one corresponding financial effect across all ledgers.*
* **Scope**: Due settlement, tenant balance, double-entry journal, receipt creation.
* **Invariant**: The creation of a verified payment of amount $A$ against due $D$ must atomically decrement $D$'s outstanding balance by $A$ and create balanced journal debit and credit entries totaling $A$.
* **Enforcement**: Executed within a single PostgreSQL transaction (`pgx.Tx`).

### INV-004 — Expense Settlement Bound
> *An expense payment shall not increase cumulative settlement above the approved expense amount under any sequence of concurrent requests.*
* **Scope**: Owner and manager expense payouts, advances, reimbursements.
* **Invariant**: For any expense $E$, $\sum(\text{payments against } E) \le E.\text{amount\_paise}$.
* **Enforcement**: Transactional `SELECT ... FOR UPDATE` row lock on `expenses` record prior to summing payments and inserting new disbursement.

### INV-005 — Lossless Credit Mutation
> *Concurrent credit updates shall not lose increments.*
* **Scope**: Overpayment allocation, security deposit refund, manual credit adjustment.
* **Invariant**: If $N$ concurrent requests add credits $c_1, c_2, \dots, c_N$ to tenant $T$, the resulting balance must equal $\text{Initial} + \sum c_i$.
* **Enforcement**: Single-statement SQL atomic mutation `UPDATE tenants SET credit_balance_paise = credit_balance_paise + $2 WHERE id = $1` or row-locked transaction. Elimination of in-memory read-modify-write.

### INV-006 — Strict Property Scoping
> *A property-scoped financial or PII operation shall not read, write, or associate with another property's entities.*
* **Scope**: Multi-property operations, rooms, expenses, dues, tenants, payments.
* **Invariant**: All child entities (rooms, dues, expenses, payments) must share the identical `property_id` of their parent and cannot be linked across differing property IDs.
* **Enforcement**: Composite foreign keys `FOREIGN KEY (room_id, property_id) REFERENCES rooms(id, property_id)`, application-level claim verification, and PostgreSQL Row-Level Security (RLS).

### INV-007 — Ledger Immutability
> *A financial correction shall not mutate an existing ledger or journal entry; adjustments must append offsetting reversal entries.*
* **Scope**: Double-entry accounting tables (`journal_entries`, `journal_lines`).
* **Invariant**: `UPDATE` and `DELETE` statements are physically prohibited on committed journal lines. Corrections are recorded strictly as new reversing and adjusting entries.
* **Enforcement**: PostgreSQL trigger / privilege revocation raising an exception on any update/delete operation against journal tables.

### INV-008 — Single Correction Determinism
> *A verified payment shall not have multiple independent active corrections applied against it.*
* **Scope**: Payment adjustments, disputed settlements.
* **Invariant**: An original payment can only be corrected through a deterministic state machine. Duplicate concurrent correction requests must be rejected.
* **Enforcement**: `UNIQUE(original_payment_id)` on `financial_corrections` or explicit active-correction status locking.

### INV-009 — Read-Model Reconciliation
> *Dashboard totals and cached rollups shall strictly reconcile with the authoritative ledger.*
* **Scope**: `daily_financial_rollups`, Owner dashboard summary.
* **Invariant**: The sum of daily rollups for period $P$ must match the direct ledger query $\sum(\text{verified payments}) - \sum(\text{settled expenses})$ for period $P$.
* **Enforcement**: Scheduled reconciliation job comparing rollups to raw journal sums and raising telemetry alerts on drift $> 0$.

### INV-010 — Universal Integer-Paise Arithmetic
> *All monetary amounts across storage, domain types, API contracts, and calculations shall be signed 64-bit integer paise without binary floating-point conversions.*
* **Scope**: Database columns, Go structs, JSON DTOs, UPI QR generators, gateway webhooks.
* **Invariant**: No `float32`, `float64`, or 32-bit `int`/`INTEGER` types for money. No division before multiplication. Rupee decimal formatting occurs only at the string presentation boundary via integer division and modulus.
* **Enforcement**: PostgreSQL `BIGINT` columns with `CHECK (amount >= 0)` where appropriate; Go domain types typed as `int64` (or `type Paise int64`).

### INV-011 — Zero-Partial-State Atomicity
> *A failed financial transaction shall leave zero partial state.*
* **Scope**: Multi-table operations (e.g., payment report match + settlement + review update).
* **Invariant**: If any intermediate write, validation, or event publishing fails within a financial operation, all preceding writes in that operation must be completely rolled back.
* **Enforcement**: Atomic PostgreSQL transaction blocks with deferred constraint validation and explicit error handling on all sub-operations.

### INV-012 — Manager Spending Cap Inviolability
> *Concurrent manager expense submissions shall not exceed authorized daily or monthly spending thresholds.*
* **Scope**: Manager expense creation and advance drawdown.
* **Invariant**: The sum of expenses created by manager $M$ on date $D$ shall not exceed $M$'s configured daily cap under any concurrent request volume.
* **Enforcement**: Serialized concurrency control (manager counter row lock `FOR UPDATE` or scoped transaction locking).

### INV-013 — Sensitive Banking Data Encryption
> *Payout bank account numbers and sensitive payment credentials shall be encrypted with authenticated envelope encryption at rest.*
* **Scope**: `payout_payees.account_number_encrypted`, beneficiary credentials.
* **Invariant**: Plaintext bank account numbers must never be written to PostgreSQL or emitted in application logs. Stored data must include a key version, random IV/nonce, and ciphertext authenticated with AES-256-GCM.
* **Enforcement**: Application crypto service with external KMS / environment master key; database tests asserting ciphertext $\ne$ plaintext.

### INV-014 — Fail-Closed Gateway Ingestion
> *Gateway payloads with missing, malformed, or unparseable monetary values shall fail closed.*
* **Scope**: Cashfree webhook handlers, payment intent confirmation.
* **Invariant**: Any non-numeric, float, negative, or malformed amount received from a payment gateway must result in an immediate parsing error and rejection. It must never silently coerce to 0.
* **Enforcement**: Strict integer string / exact decimal deserializer with explicit error returns.

### INV-015 — Non-Zero Failure Transparency
> *Aggregation or database failures on dashboard endpoints shall fail explicitly with an HTTP error and never return synthetic ₹0 figures.*
* **Scope**: `/api/owner/dashboard/summary`, `/api/owner/dashboard/monthly-cashflow`.
* **Invariant**: If any underlying database query, rollup calculation, or financial summary fails, the API must return HTTP 500 / 503 with an error envelope, never a 200 OK containing ₹0.
* **Enforcement**: Eliminate error swallowing (`_ = err`) in dashboard handlers; propagate errors up to the API error mapper.
