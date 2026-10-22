# PG Cashflow — Financial Invariant Register

This document is the authoritative register of financial, ledger, transactional, and identity data invariants for the PG Cashflow system.

Every requirement, schema change, service implementation, database constraint, and test suite must map to one or more invariants defined herein. **No pull request or deployment may weaken, bypass, or violate any invariant in this register.**

---

## Invariant Index

| Invariant ID | Title | Enforcement Level | Primary Target | Enforced In (Verified Code & Schema Locations) |
| :--- | :--- | :--- | :--- | :--- |
| **INV-001** | Idempotent Payment Creation | DB + Application | `payments`, `payment_intents` | `internal/payment/service.go:436, 603, 721`<br>`internal/postgres/payment_repo.go:287-295`<br>`migrations/001_initial.sql:82` |
| **INV-002** | UTR Uniqueness | DB Constraint + App | `payments(upi_txn_id)` | `migrations/001_initial.sql:82`<br>`internal/payment/service.go:433-440`<br>`internal/bank/matcher.go:120-135` |
| **INV-003** | Atomic Financial Effect | Database Transaction | Payments, Dues, Journal | `internal/payment/service.go:598-717`<br>`internal/postgres/dbtx.go:68-91` |
| **INV-004** | Expense Settlement Bound | DB Lock + Constraint | `expenses`, `expense_payments` | `internal/postgres/finance_repo_expense.go:265-274, 293-295` |
| **INV-005** | Lossless Credit Mutation | Atomic SQL Update | `tenants.credit_balance_paise` | `internal/postgres/property_repo.go:373-391, 420-424` |
| **INV-006** | Strict Property Scoping | DB Constraint + App | All property-scoped tables | `migrations/046_remediate_p0_invariants.sql:129-155`<br>`internal/postgres/dbtx.go:78-82`<br>`internal/payment/service.go:293, 311` |
| **INV-007** | Ledger Immutability | DB Triggers / Append-Only | `journal_entries`, `accounts` | `migrations/043_ledger_integrity_controls.sql:15-34`<br>`migrations/045_financial_integrity_and_rollups.sql:95-105` |
| **INV-008** | Single Correction Determinism | DB Constraint / State | `financial_corrections` | `migrations/046_remediate_p0_invariants.sql:101-103`<br>`internal/payment/service.go:273-284, 307-310` |
| **INV-009** | Read-Model Reconciliation | Batch & Rollup Reconciler | `daily_financial_rollups` | `migrations/045_financial_integrity_and_rollups.sql:58-74`<br>`internal/jobs/daily_rollup.go:46-85`<br>`internal/postgres/finance_repo_statements.go:125-156, 186-265` |
| **INV-010** | Universal Integer-Paise Arithmetic | DB Schema + Go Types | Entire Codebase | `migrations/046_remediate_p0_invariants.sql:1-96`<br>`internal/domain/payment.go`<br>`internal/domain/due.go`<br>`internal/domain/expense.go` |
| **INV-011** | Zero-Partial-State Atomicity | Transaction Boundary | All Financial Operations | `internal/postgres/dbtx.go:68-91`<br>`internal/payment/service.go:86-90` |
| **INV-012** | Manager Spending Cap Inviolability | Concurrency Serialization | `expenses`, `manager_spend` | `internal/postgres/finance_repo_expense.go:42-70, 235-261`<br>`internal/domain/finance.go:EvaluateManagerSpend` |
| **INV-013** | Sensitive Banking Data Encryption | Authenticated Envelope Enc | `payout_payees` | `internal/api/handlers_payouts.go:326-332`<br>`migrations/022_payout_batches_and_payees.sql:14`<br>`internal/crypto/aes_gcm.go` |
| **INV-014** | Fail-Closed Gateway Ingestion | Strict Deserialization | Cashfree Webhooks & Ingestion | `internal/cashfree/decimal.go:17-86`<br>`internal/cashfree/webhook.go:113-296`<br>`internal/cashfree/webhook_test.go:156-200` |
| **INV-015** | Non-Zero Failure Transparency | HTTP Handler Contracts | Owner & Manager Dashboards | `internal/api/handlers_owner_dashboard.go:457-461, 467-471, 481-485, 499-503` |
| **INV-016** | KYC Identity Document Encryption & Caching | Crypto Service + HTTP Header | Identity Photos & Documents | `internal/postgres/kyc_repo.go:40-94`<br>`internal/app/bootstrap.go:214-234`<br>`cmd/kyc-expiry/main.go:58-62`<br>`internal/api/handlers_owner_tenants.go:583, 621` |

---

## Detailed Invariant Specifications

### INV-001 — Idempotent Payment Creation
> *A payment shall not be created twice for the same idempotency identity.*
* **Scope**: Manual payment verification, Cashfree webhook ingestion, offline UTR entry.
* **Invariant**: Providing the same idempotency key or webhook event ID must return the existing payment record and execute zero additional state transitions.
* **Enforcement**: Database unique constraints on `payments.upi_txn_id` and `processed_webhook_events(provider, event_type, provider_reference_id, event_status)`, combined with transactional deduplication checks before mutating state.
* **Enforced In**:
  - `internal/payment/service.go:436-440` (`GatewaySettle` early check on existing UTR)
  - `internal/payment/service.go:599-612` (`RecordWebhookEvent` deduplication check inside transaction)
  - `internal/payment/service.go:719-724` (replay detection returning existing payment record)
  - `internal/postgres/payment_repo.go:287-295` (atomic insert into `processed_webhook_events` with `ON CONFLICT DO NOTHING`)
  - `migrations/001_initial.sql:82` (`payments.upi_txn_id UNIQUE`)

### INV-002 — UTR Uniqueness
> *A unique UTR / transaction ID shall not identify two independent payments.*
* **Scope**: All payment receipts and verifications.
* **Invariant**: Bank reference numbers (`upi_txn_id`) must be unique across all payment records. Duplicate transaction IDs must be rejected by both application format validation and database integrity constraints.
* **Enforcement**: Database unique constraint `payments.upi_txn_id UNIQUE` (`migrations/001_initial.sql`).
* **Enforced In**:
  - `migrations/001_initial.sql:82` (`payments.upi_txn_id VARCHAR(50) UNIQUE`)
  - `internal/payment/service.go:433-440` (empty check and UTR deduplication)
  - `internal/bank/matcher.go:120-135` (rejection of duplicate bank UTR transactions)

### INV-003 — Atomic Financial Effect
> *A verified payment shall have exactly one corresponding financial effect across all ledgers.*
* **Scope**: Due settlement, tenant balance, double-entry journal, receipt creation.
* **Invariant**: The creation of a verified payment of amount $A$ against due $D$ must atomically decrement $D$'s outstanding balance by $A$ and create balanced journal debit and credit entries totaling $A$.
* **Enforcement**: Executed within a single PostgreSQL transaction (`pgx.Tx`).
* **Enforced In**:
  - `internal/payment/service.go:598-717` (`s.runInTx`: executes `getDueForUpdate`, `due.ApplyPayment`, `dues.Update`, `payments.Create`, and `pub.Publish` within one atomic block)
  - `internal/postgres/dbtx.go:68-91` (`WithinTx` wrapper ensuring rollback on error)

### INV-004 — Expense Settlement Bound
> *An expense payment shall not increase cumulative settlement above the approved expense amount under any sequence of concurrent requests.*
* **Scope**: Owner and manager expense payouts, advances, reimbursements.
* **Invariant**: For any expense $E$, $\sum(\text{payments against } E) \le E.\text{amount\_paise}$.
* **Enforcement**: Transactional `SELECT ... FOR UPDATE` row lock on `expenses` record prior to summing payments and inserting new disbursement.
* **Enforced In**:
  - `internal/postgres/finance_repo_expense.go:265-274` (`SELECT ... FROM expenses WHERE id=$1 FOR UPDATE`)
  - `internal/postgres/finance_repo_expense.go:288-295` (`SELECT COALESCE(SUM(amount_paise), 0) FROM expense_payments WHERE expense_id=$1`; rejects payment with `domain.ErrOverpay` if `paid + p.AmountPaise > e.AmountPaise`)

### INV-005 — Lossless Credit Mutation
> *Concurrent credit updates shall not lose increments.*
* **Scope**: Overpayment allocation, security deposit refund, manual credit adjustment.
* **Invariant**: If $N$ concurrent requests add credits $c_1, c_2, \dots, c_N$ to tenant $T$, the resulting balance must equal $\text{Initial} + \sum c_i$.
* **Enforcement**: Single-statement SQL atomic mutation `UPDATE tenants SET credit_balance_paise = credit_balance_paise + $2 WHERE id = $1` or row-locked transaction. Elimination of in-memory read-modify-write.
* **Enforced In**:
  - `internal/postgres/property_repo.go:373-380` (`AddCredit`: `UPDATE tenants SET credit_balance_paise = GREATEST(0, credit_balance_paise + $2) WHERE id = $1`)
  - `internal/postgres/property_repo.go:382-391` (`DeductCredit`: `UPDATE tenants SET credit_balance_paise = GREATEST(0, credit_balance_paise - $2) WHERE id = $1 RETURNING credit_balance_paise`)
  - `internal/postgres/property_repo.go:420-424` (`TenantRepo.Update`: excludes `credit_balance_paise` to prevent overwrite of concurrent credit changes)

### INV-006 — Strict Property Scoping
> *A property-scoped financial or PII operation shall not read, write, or associate with another property's entities.*
* **Scope**: Multi-property operations, rooms, expenses, dues, tenants, payments.
* **Invariant**: All child entities (rooms, dues, expenses, payments) must share the identical `property_id` of their parent and cannot be linked across differing property IDs.
* **Enforcement**: Composite foreign keys `FOREIGN KEY (room_id, property_id) REFERENCES rooms(id, property_id)`, application-level claim verification, and PostgreSQL Row-Level Security (RLS).
* **Enforced In**:
  - `migrations/046_remediate_p0_invariants.sql:129-155` (composite unique `rooms(id, property_id)` and foreign keys `fk_expenses_room_property` and `fk_tenants_room_property`)
  - `internal/postgres/dbtx.go:78-82` (`set_config('role', 'pgapp_app', true)` and `set_config('app.current_property_id', $1, true)`)
  - `internal/payment/service.go:293-295, 311-313` (mandatory property scope verification inside `CorrectPayment`)
  - `internal/postgres/finance_repo_expense.go:281-283` (`if e.PropertyID != p.PropertyID { return nil, domain.ErrForbidden }`)

### INV-007 — Ledger Immutability
> *A financial correction shall not mutate an existing ledger or journal entry; adjustments must append offsetting reversal entries.*
* **Scope**: Double-entry accounting tables (`journal_entries`, `financial_journal_entries`, `financial_corrections`).
* **Invariant**: `UPDATE` and `DELETE` statements are physically prohibited on committed journal lines. Corrections are recorded strictly as new reversing and adjusting entries.
* **Enforcement**: PostgreSQL triggers raising exceptions on any update/delete operation against journal and audit tables.
* **Enforced In**:
  - `migrations/043_ledger_integrity_controls.sql:15-34` (`trg_journal_append_only`: blocks all `UPDATE`s on `financial_journal_entries`; blocks `DELETE`s unless maintenance flag is set)
  - `migrations/044_ledger_period_lock_and_reporting.sql:67-75` (`financial_journal_entries` append-only trigger)
  - `migrations/045_financial_integrity_and_rollups.sql:95-105` (`trg_financial_corrections_immutable`: blocks `UPDATE` and `DELETE` on `financial_corrections`)
  - `migrations/016_kyc_aadhaar.sql:69-75` (`kyc_audit_log` immutability trigger)

### INV-008 — Single Correction Determinism
> *A verified payment shall not have multiple independent active corrections applied against it.*
* **Scope**: Payment adjustments, disputed settlements.
* **Invariant**: An original payment can only be corrected through a deterministic state machine. Duplicate concurrent correction requests must be rejected.
* **Enforcement**: Database constraint `UNIQUE(original_payment_id)` on `financial_corrections`, application-level `HasCorrection` check, and row locking on target due.
* **Enforced In**:
  - `migrations/046_remediate_p0_invariants.sql:101-103` (`CONSTRAINT uq_financial_corrections_original UNIQUE (original_payment_id)`)
  - `internal/payment/service.go:273-284` (`HasCorrection` check rejecting duplicate attempts)
  - `internal/payment/service.go:307-310` (`getDueForUpdate` row-level lock preventing concurrent payment settlement during correction)

### INV-009 — Read-Model Reconciliation
> *Dashboard totals and cached rollups shall strictly reconcile with the authoritative ledger.*
* **Scope**: `daily_financial_rollups`, Owner dashboard summary.
* **Invariant**: The sum of daily rollups for period $P$ must match the direct ledger query $\sum(\text{verified payments}) - \sum(\text{settled expenses})$ for period $P$.
* **Enforcement**: Scheduled rollup jobs aggregating payments, dues, and expenses per property per day into `daily_financial_rollups`.
* **Enforced In**:
  - `migrations/045_financial_integrity_and_rollups.sql:58-74` (`daily_financial_rollups` schema with `CONSTRAINT uq_daily_financial_rollups_prop_date UNIQUE (property_id, rollup_date)`)
  - `internal/jobs/daily_rollup.go:46-85` (`DailyRollupJob.Run`: daily calculation and upsert per property)
  - `internal/postgres/finance_repo_statements.go:125-156` (`UpsertDailyFinancialRollup`)
  - `internal/postgres/finance_repo_statements.go:186-265` (`CollectDailyMetrics`: aggregate queries across `payments`, `dues`, and `expenses`)

### INV-010 — Universal Integer-Paise Arithmetic
> *All monetary amounts across storage, domain types, API contracts, and calculations shall be signed 64-bit integer paise without binary floating-point conversions.*
* **Scope**: Database columns, Go structs, JSON DTOs, UPI QR generators, gateway webhooks.
* **Invariant**: No `float32`, `float64`, or 32-bit `int`/`INTEGER` types for money. No division before multiplication. Rupee decimal formatting occurs only at the string presentation boundary via integer division and modulus.
* **Enforcement**: PostgreSQL `BIGINT` columns with `CHECK (amount >= 0)` or `CHECK (amount > 0)`; Go domain types typed as `int64`.
* **Enforced In**:
  - `migrations/046_remediate_p0_invariants.sql:1-55` (migrates all legacy amount columns to `BIGINT`)
  - `migrations/046_remediate_p0_invariants.sql:63, 71, 87, 94` (`chk_tenants_rent_positive`, `chk_dues_amount_non_negative`, `chk_payments_amount_positive`, `chk_payment_intents_amount_positive`)
  - `internal/domain/payment.go` (`Amount int64`)
  - `internal/domain/due.go` (`Amount int64`, `OriginalAmount int64`)
  - `internal/domain/expense.go` (`AmountPaise int64`)
  - `internal/domain/tenant.go` (`RentAmount int64`, `CreditBalancePaise int64`)

### INV-011 — Zero-Partial-State Atomicity
> *A failed financial transaction shall leave zero partial state.*
* **Scope**: Multi-table operations (e.g., payment report match + settlement + review update).
* **Invariant**: If any intermediate write, validation, or event publishing fails within a financial operation, all preceding writes in that operation must be completely rolled back.
* **Enforcement**: Atomic PostgreSQL transaction blocks with deferred constraint validation, automatic rollback via `defer tx.Rollback(ctx)`, and explicit error propagation.
* **Enforced In**:
  - `internal/postgres/dbtx.go:68-91` (`WithinTx`: rolls back automatically on error or panic)
  - `internal/payment/service.go:86-90` (wires all repository instances to shared `tx`)

### INV-012 — Manager Spending Cap Inviolability
> *Concurrent manager expense submissions shall not exceed authorized daily or monthly spending thresholds.*
* **Scope**: Manager expense creation and advance drawdown.
* **Invariant**: The sum of expenses created by manager $M$ on date $D$ shall not exceed $M$'s configured daily or monthly cap under any concurrent request volume.
* **Scope Boundary**: Enforced strictly for manager role (`CreatedByRole == "manager"`). Owner-initiated expenses bypass this cap check.
* **Enforcement**: PostgreSQL transaction-scoped advisory lock `pg_advisory_xact_lock(hashtext('manager-spend:{property}:{manager}'))` serializing concurrent submissions from the same manager.
* **Enforced In**:
  - `internal/postgres/finance_repo_expense.go:42-70` (`InsertExpenseAtomic`: advisory lock and live SQL spend summation)
  - `internal/postgres/finance_repo_expense.go:235-261` (`RecordExpensePayment`: advisory lock and daily/monthly budget verification)
  - `internal/domain/finance.go:EvaluateManagerSpend` (domain policy evaluation)

### INV-013 — Sensitive Banking Data Encryption
> *Payout bank account numbers and sensitive payment credentials shall be encrypted with authenticated envelope encryption at rest.*
* **Scope**: `payout_payees.account_number_encrypted`, beneficiary credentials.
* **Invariant**: Plaintext bank account numbers must never be written to PostgreSQL or emitted in application logs. Stored data must include a key version, random IV/nonce, and ciphertext authenticated with AES-256-GCM.
* **Enforcement**: Application crypto service with external KMS / environment master key; database tests asserting ciphertext $\ne$ plaintext.
* **Enforced In**:
  - `internal/api/handlers_payouts.go:326-332` (`crypto.EncryptWithKeyRing(h.getPayoutKeyRing(), []byte(raw))` before storage; fails closed on encryption failure)
  - `migrations/022_payout_batches_and_payees.sql:14` (`account_number_encrypted BYTEA`)
  - `internal/crypto/aes_gcm.go` (AES-256-GCM envelope encryption with random 12-byte nonce)

### INV-014 — Fail-Closed Gateway Ingestion
> *Gateway payloads with missing, malformed, or unparseable monetary values shall fail closed.*
* **Scope**: Cashfree webhook handlers, payment intent confirmation.
* **Invariant**: Any non-numeric, float, negative, or malformed amount received from a payment gateway must result in an immediate parsing error and rejection. It must never silently coerce to 0.
* **Enforcement**: Strict string-based decimal parser avoiding floating-point operations; explicit error returns on malformed inputs.
* **Enforced In**:
  - `internal/cashfree/decimal.go:17-86` (`ParseRupeesToPaise`: exact decimal string-to-paise parser; rejects empty, multi-decimal, negative, or non-numeric tokens)
  - `internal/cashfree/webhook.go:113-296` (`ParseWebhook`: enforces `ParseRupeesToPaise` on all webhook amount fields)
  - `internal/cashfree/webhook_test.go:156-200` (`TestParseWebhook_MalformedAmountFailsClosed`)

### INV-015 — Non-Zero Failure Transparency
> *Aggregation or database failures on dashboard endpoints shall fail explicitly with an HTTP error and never return synthetic ₹0 figures.*
* **Scope**: `/api/owner/dashboard/summary`, `/api/owner/dashboard/monthly-cashflow`.
* **Invariant**: If any underlying database query, rollup calculation, or financial summary fails, the API must return HTTP 500 / 503 with an error envelope, never a 200 OK containing ₹0.
* **Enforcement**: Systematic elimination of error-swallowing (`_ = err`); propagation of all database and service errors to HTTP error handlers.
* **Enforced In**:
  - `internal/api/handlers_owner_dashboard.go:457-461` (occupancy computation error handling: returns 500)
  - `internal/api/handlers_owner_dashboard.go:467-471` (payment summary build error handling: returns 500)
  - `internal/api/handlers_owner_dashboard.go:481-485` (operating summary error handling: returns 500)
  - `internal/api/handlers_owner_dashboard.go:499-503` (monthly cash flow computation error handling: returns 500)

### INV-016 — KYC Identity Document Encryption & Caching
> *Tenant identity documents and photos shall be encrypted at rest with authenticated AES-256-GCM and delivered with strict no-store HTTP cache controls.*
* **Scope**: `tenants.id_photo_bytes`, `kyc_verifications.attested_photo_bytes`, `/api/owner/tenants/:id/photo`, `/api/owner/kyc/:id/document`.
* **Invariant**:
  1. All stored Aadhaar documents and photos must be encrypted using AES-256-GCM with a 32-byte key (`KYC_DOC_CRYPTO_KEY`).
  2. A malformed encryption key must fail closed at system startup (`bootstrap.go`) and job execution (`cmd/kyc-expiry`). An empty key logs an explicit warning for developer environments.
  3. Decryption must not swallow AEAD authentication errors; tag mismatch returns an explicit error. Legacy unencrypted binaries (JPEG, PNG, PDF) are detected using magic bytes to maintain read compatibility during migration.
  4. Identity endpoints must send `Cache-Control: no-store` and `X-Content-Type-Options: nosniff` headers to prevent CDN and browser caching of PII.
* **Enforcement**: Application crypto service with fail-closed key validation, magic-byte sniffing migration, and mandatory HTTP response headers.
* **Enforced In**:
  - `internal/postgres/kyc_repo.go:40-49` (`encryptBytes`: validates 32-byte key; executes `crypto.Encrypt`)
  - `internal/postgres/kyc_repo.go:55-66` (`isLegacyPlaintextDoc`: sniffing JPEG `\xFF\xD8\xFF`, PNG `\x89PNG`, PDF `%PDF`)
  - `internal/postgres/kyc_repo.go:68-94` (`decryptBytes`: verifies AEAD authentication; prevents silent data corruption)
  - `internal/app/bootstrap.go:214-234` (startup validation: returns `fmt.Errorf` on invalid key length)
  - `cmd/kyc-expiry/main.go:58-62` (CLI validation: `log.Fatalf` on invalid key length)
  - `internal/api/handlers_owner_tenants.go:583, 621` (`c.Header("Cache-Control", "no-store")`, `c.Header("X-Content-Type-Options", "nosniff")`)
  - `internal/api/router_manager.go:35` (`capOf("tenants.view")` capability gate on `/kyc/:id/document`)
