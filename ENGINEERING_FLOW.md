# PG Cashflow — Evidence-Driven Engineering and Acceptance Flow

## 1. Purpose

Use this engineering flow for every financial, security, dashboard, automation, and database change.

The objective is to ensure that:

* Requirements are precise.
* Existing behavior is understood before modification.
* Security controls are enforced at the correct boundary.
* Financial calculations are exact.
* Concurrent requests cannot corrupt financial state.
* Database constraints protect critical invariants.
* Automated tests prove the required behavior.
* Performance is measured.
* Changes can be traced from requirement to implementation to test evidence.
* No feature is marked complete without an acceptance gate.

---

# 2. Engineering Flow

```text
ASD-STE REQUIREMENTS
        ↓
REQUIREMENT IDs + ACCEPTANCE CRITERIA
        ↓
TRACEABILITY MATRIX
        ↓
REPOSITORY INVESTIGATION
        ↓
DATA FLOW + TRUST BOUNDARY ANALYSIS
        ↓
THREAT / ABUSE-CASE ANALYSIS
        ↓
DESIGN + ARCHITECTURE GATE
        ↓
IMPLEMENTATION
        ↓
UNIT TESTS
        ↓
INTEGRATION TESTS
        ↓
DATABASE / TRANSACTION TESTS
        ↓
CONCURRENCY + FAILURE-INJECTION TESTS
        ↓
STATIC ANALYSIS + SECURITY SCANS
        ↓
PERFORMANCE + QUERY ANALYSIS
        ↓
INDEPENDENT CODE REVIEW
        ↓
REQUIREMENT-BY-REQUIREMENT VERIFICATION
        ↓
ACCEPTANCE GATE
        ↓
RELEASE GATE
        ↓
POST-DEPLOYMENT VERIFICATION
```

---

# 3. Gate 0 — ASD-STE Requirement Definition

Write every requirement as a testable technical statement.

Avoid:

* vague objectives;
* multiple actions in one sentence;
* subjective terms;
* implied behavior;
* UI-only security requirements;
* undefined financial terminology.

Use:

```text
REQ-FIN-001
The system shall store monetary amounts as signed 64-bit integer paise.

REQ-FIN-002
The system shall not use floating-point arithmetic for financial calculations.

REQ-FIN-003
The database shall reject monetary values that exceed the supported monetary range.

REQ-PAY-001
The system shall process payment verification as one atomic transaction.

REQ-PAY-002
The system shall reject a duplicate normalized UTR.

REQ-SEC-001
The system shall verify that the authenticated user has access
to the property before a property-scoped financial operation.

REQ-DASH-001
The system shall return the Owner dashboard financial summary
from server-side aggregation.

REQ-EXP-001
The owner shall be able to create an expense for an authorized property.

REQ-EXP-002
The expense amount shall be stored as integer paise.

REQ-EXP-003
The system shall prevent concurrent expense approvals from
exceeding the authorized spending limit.
```

Each requirement shall have:

* Requirement ID.
* Description.
* Security classification.
* Data affected.
* Acceptance criteria.
* Test reference.
* Implementation reference.
* Status.

---

# 4. Gate 1 — Requirement Traceability

Create a requirement traceability matrix.

Example:

| Requirement  | Design               | Code                | Test             | Evidence         | Status |
| ------------ | -------------------- | ------------------- | ---------------- | ---------------- | ------ |
| REQ-FIN-001  | Money model          | `domain/payment.go` | money tests      | DB schema        | PASS   |
| REQ-PAY-001  | Transaction design   | payment service     | integration test | transaction test | PASS   |
| REQ-SEC-001  | Authorization policy | owner handler       | API test         | access test      | PASS   |
| REQ-DASH-001 | Dashboard read model | dashboard service   | API test         | query plan       | PASS   |
| REQ-EXP-003  | Locking strategy     | expense service     | concurrency test | race test        | FAIL   |

A requirement is not complete when code exists.

It is complete only when:

```text
Requirement
    +
Implementation
    +
Automated Test
    +
Evidence
    =
Verified Requirement
```

---

# 5. Gate 2 — Repository Investigation

Before changing code, inspect the complete execution path.

For a financial change, inspect:

```text
Router
  ↓
Authentication
  ↓
Authorization
  ↓
HTTP Handler
  ↓
Service
  ↓
Transaction Boundary
  ↓
Repository
  ↓
Database Schema
  ↓
Constraints / Triggers
  ↓
Audit / Events
```

Also inspect reverse paths:

```text
Database
  ↓
Repository
  ↓
Service
  ↓
Handler
  ↓
API Response
  ↓
Dashboard
```

Search for:

```text
amount
price
balance
payment
due
expense
refund
reversal
cashflow
property_id
tenant_id
room_id
utr
upi
journal
ledger
transaction
```

Do not assume that a new implementation removed the old implementation.

Search for:

* duplicate functionality;
* legacy money fields;
* legacy status fields;
* old write paths;
* direct repository writes;
* bypass endpoints;
* background jobs;
* test-only behavior;
* administrative endpoints.

---

# 6. Gate 3 — Trust Boundary and Data-Flow Analysis

For every important operation, identify:

```text
INPUT
  ↓
AUTHENTICATION
  ↓
AUTHORIZATION
  ↓
VALIDATION
  ↓
NORMALIZATION
  ↓
TRANSACTION
  ↓
DATABASE CONSTRAINT
  ↓
AUDIT
  ↓
OUTPUT
```

Example: payment verification.

```text
Owner request
     ↓
JWT validation
     ↓
Owner role validation
     ↓
property_id authorization
     ↓
payment ID validation
     ↓
amount validation
     ↓
UTR normalization
     ↓
duplicate check
     ↓
transaction
     ├── lock due
     ├── create payment
     ├── update due
     ├── journal entry
     └── audit entry
     ↓
commit
     ↓
response
```

Every arrow is a possible security or correctness boundary.

---

# 7. Gate 4 — Threat and Abuse-Case Analysis

Do not test only the normal path.

For each financial endpoint, define abuse cases.

Example:

```text
Payment verification

Normal:
Valid payment → successful verification

Duplicate:
Same UTR → reject / idempotent response

Tampering:
Different amount → reject

Cross-property:
Valid payment ID + unauthorized property → reject

Replay:
Same request repeated 10 times → one financial result

Concurrency:
50 simultaneous verification requests → one valid payment

Failure:
Database failure after payment creation → entire transaction rolls back

Malformed input:
Invalid UTR → reject

Boundary:
Maximum supported amount → accepted

Overflow:
Amount above supported integer range → reject
```

This converts security assumptions into executable tests.

---

# 8. Gate 5 — Architecture / Design Gate

Before implementation, define the invariant.

For financial functionality, specify:

### Source of truth

Example:

```text
Payment journal = financial source of truth
Dashboard = derived read model
UI = presentation only
```

### Transaction boundary

Example:

```text
Verify Payment

BEGIN
    lock due
    validate state
    create payment
    update due
    create journal entry
    create audit event
COMMIT
```

### Concurrency strategy

Choose explicitly:

* row locking;
* optimistic versioning;
* unique constraints;
* atomic SQL updates;
* serializable transactions;
* advisory locks where justified.

Do not rely on application-level assumptions.

### Database invariant

For critical rules, enforce the rule in PostgreSQL whenever practical.

Examples:

```text
UTR uniqueness
non-negative constraints
foreign keys
property ownership
immutable audit rows
valid monetary ranges
unique billing period
unique payment idempotency key
```

---

# 9. Gate 6 — Implementation

Implement the smallest change that satisfies the requirement.

Do not introduce:

* unnecessary abstractions;
* duplicate services;
* duplicate models;
* unnecessary dependencies;
* client-side financial logic;
* hidden side effects.

For financial writes:

```text
Handler
    ↓
Service
    ↓
Transaction
    ↓
Repository
    ↓
Database constraints
```

The client must not perform several independent financial writes when the operation is logically one transaction.

---

# 10. Gate 7 — Money Integrity

Perform a repository-wide money audit.

Search every monetary representation.

The target state is:

```text
Go:
int64

PostgreSQL:
BIGINT

API:
integer paise

Display:
₹ amount generated from integer paise
```

Do not permit:

```text
float32
float64
decimal calculations implemented with binary floating point
implicit casts
money conversion through floating point
```

Validate conversions from:

* JSON;
* payment gateways;
* query parameters;
* database values;
* external APIs.

Reject malformed monetary values.

Never allow:

```text
parse error → zero
```

for a financial amount.

---

# 11. Gate 8 — Authorization

Every property-scoped operation shall verify:

```text
authenticated user
        +
required role
        +
property access
        +
resource ownership
```

Do not assume that checking only:

```text
user role = owner
```

is sufficient.

Test:

```text
Owner A → Property A      PASS
Owner A → Property B      DENY

Manager A → Property A    according to policy
Tenant A → owner API      DENY

Valid resource ID
+
different property_id
                      → DENY
```

Prefer database-level isolation for critical systems.

---

# 12. Gate 9 — Database Integrity

Application validation is not enough.

The database should enforce critical invariants.

Review:

```text
PRIMARY KEY
FOREIGN KEY
UNIQUE
CHECK
NOT NULL
INDEX
TRIGGER
ROW LOCKING
```

For each financial invariant ask:

> Can a programmer bypass this rule accidentally?

If yes, move the invariant closer to the database.

Examples:

```text
payments.normalized_utr UNIQUE per required scope

expenses.amount_paise > 0

dues(property_id, tenant_id, billing_period) UNIQUE

financial_corrections immutable

journal entries append-only
```

---

# 13. Gate 10 — Atomicity

For every multi-table financial operation answer:

```text
What happens if step 1 succeeds?
What happens if step 2 fails?
What happens if step 3 fails?
What happens if the process crashes?
```

The expected model is:

```text
BEGIN
  operation A
  operation B
  operation C
COMMIT
```

not:

```text
write A
commit

write B
commit

write C
commit
```

unless partial completion is explicitly intended.

---

# 14. Gate 11 — Concurrency Verification

Normal unit tests do not prove concurrency correctness.

Create concurrent tests for:

```text
payment verification
payment correction
expense approval
expense reimbursement
manager spending limits
tenant credits
due generation
reminders
idempotency
```

Example:

```text
100 concurrent requests
       ↓
same payment
       ↓
expected result:
one financial effect
```

Also test:

```text
two users
same expense
simultaneous approval
```

The system must preserve the financial invariant.

---

# 15. Gate 12 — Failure Injection

Test failures at important transaction points.

Example:

```text
after payment validation
after due lock
after payment insert
after due update
after journal insert
after audit insert
before COMMIT
after COMMIT
```

Verify that the resulting state is valid.

This is more valuable than only testing successful requests.

---

# 16. Gate 13 — Automated Testing Pyramid

Use several test levels.

### Unit

Test:

* normalization;
* amount validation;
* status calculation;
* date calculations;
* cash-flow formulas.

### Integration

Test:

* handlers;
* services;
* repositories;
* PostgreSQL;
* transactions;
* constraints.

### Contract

Test:

* API request schemas;
* response schemas;
* error responses.

### Concurrency

Test:

* duplicate requests;
* simultaneous updates;
* race conditions.

### Migration

Test:

```text
clean database → all migrations
existing database → migration
rollback strategy where supported
```

### Security

Test:

* cross-property access;
* role escalation;
* IDOR;
* unauthorized writes;
* malformed input.

---

# 17. Gate 14 — Static and Security Analysis

Before review run:

```text
go test ./...

go vet ./...

staticcheck ./...

gosec ./...

go test -race ./...

dependency vulnerability scan

migration validation

secret scan
```

Also inspect:

```text
hard-coded secrets
JWT handling
password handling
SQL construction
unsafe logging
PII logging
bank information
Aadhaar information
UPI information
authorization middleware
CORS
CSRF where applicable
rate limits
file uploads
webhook verification
```

No critical or high-risk finding may remain without an explicit accepted risk record.

---

# 18. Gate 15 — Observability

Financial systems need evidence after deployment.

Log operational events such as:

```text
payment verification
payment correction
expense creation
expense approval
expense reversal
failed transaction
duplicate request
authorization denial
scheduled job execution
```

Do not log sensitive values unnecessarily.

Never log:

```text
password
OTP
full bank account number
full Aadhaar number
payment credentials
secrets
tokens
```

Use correlation/request IDs.

Every financial operation should be traceable without exposing sensitive data.

---

# 19. Gate 16 — Dashboard Performance

The dashboard should be a read model.

Preferred architecture:

```text
Financial Ledger
      ↓
Daily / Monthly Rollup
      ↓
Dashboard Query
      ↓
Small Aggregated Payload
      ↓
UI
```

Avoid:

```text
Dashboard request
    ↓
load 10,000 payments
    ↓
load 5,000 expenses
    ↓
client/server repeatedly loops through rows
    ↓
calculate totals
```

Define performance targets.

Example:

```text
Dashboard summary:
one API request

Dashboard database work:
bounded aggregation

Historical chart:
read rollup data

Ledger:
cursor pagination

Maximum page size:
50
```

Measure actual SQL execution plans with:

```text
EXPLAIN
EXPLAIN ANALYZE
```

Do not declare a query optimized without evidence.

---

# 20. Gate 17 — Background Jobs

For every scheduled job verify:

```text
idempotent
retryable
observable
bounded
safe under duplicate execution
safe under concurrent execution
```

Examples:

```text
Monthly rent generation

run twice
→ same financial result

Reminder

run twice
→ one notification

Daily rollup

run twice
→ same aggregate

Payment streak

event replay
→ same final state
```

A scheduler starting the same job twice must not corrupt data.

---

# 21. Gate 18 — Independent Code Review

Perform a second-pass review after implementation.

The reviewer should not ask:

> "Does this code look correct?"

The reviewer should ask:

```text
What can bypass this implementation?

What happens under concurrency?

What happens during database failure?

What happens when the request is replayed?

Can another property be accessed?

Can money be rounded?

Can money overflow?

Can a legacy path still write the old format?

Can an old endpoint bypass the new service?

Can a database constraint reject something valid?

Can a database constraint fail to reject something invalid?

Does the test prove the invariant?
```

This is where many hidden defects are found.

---

# 22. Gate 19 — Requirement Closure Review

Perform a strict requirement-by-requirement review.

Use:

```text
PASS
FAIL
PARTIAL
NOT VERIFIED
NOT APPLICABLE
```

Never use:

```text
implemented
```

as a synonym for:

```text
verified
```

Example:

```text
REQ-FIN-001
Integer paise

Implementation: PARTIAL
Tests: PARTIAL
Database: FAIL
Overall: FAIL
```

This prevents a misleading “21/21 complete” declaration.

---

# 23. Gate 20 — Acceptance Gate

The acceptance gate has four dimensions.

## Correctness

```text
Financial invariants pass
Transactions pass
Concurrency tests pass
Failure tests pass
```

## Security

```text
Authorization passes
Property isolation passes
No critical security finding
Sensitive data controls pass
```

## Performance

```text
Indexes verified
Query plans verified
Pagination verified
Dashboard aggregation verified
Load tests meet target
```

## Operability

```text
Jobs verified
Retries verified
Logging verified
Monitoring verified
Migration verified
Rollback plan verified
```

All four must pass.

---

# 24. Gate 21 — Release Gate

Only release when:

```text
All P0 = 0
All unresolved P1 = explicitly accepted
All financial tests = PASS
Security tests = PASS
Race tests = PASS
Migration tests = PASS
Contract tests = PASS
Performance targets = PASS
Observability = PASS
Rollback procedure = tested
```

For a financial defect:

```text
P0/P1
     ↓
NO RELEASE
```

Do not release based on schedule pressure.

---

# 25. Gate 22 — Production Verification

After deployment verify:

```text
application health
database health
migration version
job execution
payment verification
expense creation
dashboard summary
cash flow
authorization
error rate
latency
database locks
```

Run a small controlled financial transaction.

Verify:

```text
request
→ database
→ journal
→ audit
→ dashboard
```

Then verify the transaction can be reconciled independently.

---

# 26. Definition of Done

A feature is DONE only when:

```text
[ ] ASD-STE requirement exists
[ ] Requirement ID exists
[ ] Acceptance criteria exist
[ ] Repository investigation completed
[ ] Trust boundaries identified
[ ] Threat/abuse cases identified
[ ] Architecture approved
[ ] Implementation completed
[ ] Unit tests pass
[ ] Integration tests pass
[ ] Database tests pass
[ ] Concurrency tests pass
[ ] Failure tests pass
[ ] Security tests pass
[ ] Static analysis passes
[ ] Dependency scan passes
[ ] Performance verified
[ ] Code reviewed
[ ] Requirement traceability complete
[ ] No unresolved P0
[ ] P1 findings formally accepted or fixed
[ ] Migration verified
[ ] Observability verified
[ ] Rollback procedure verified
[ ] Acceptance gate PASS
```

---

# 27. Recommended AI-Agent Loop

Use the same sequence when an AI coding agent modifies the repository.

```text
1. READ
   Read requirements and constraints.

2. MAP
   Identify affected files, services, tables, routes and tests.

3. TRACE
   Trace the complete execution path.

4. CHALLENGE
   Look for security, concurrency and failure weaknesses.

5. DESIGN
   Produce the smallest safe design.

6. IMPLEMENT
   Modify the repository.

7. TEST
   Run unit and integration tests.

8. ATTACK
   Test replay, concurrency, malformed input,
   authorization bypass and database failure.

9. MEASURE
   Check query plans, latency and resource usage.

10. REVIEW
    Review the change as if trying to reject it.

11. VERIFY
    Map every requirement to evidence.

12. ACCEPT
    Close only requirements with sufficient evidence.
```

The critical rule is:

```text
DO NOT ASK THE AGENT:
"Did you implement everything?"

ASK:
"Show evidence for every requirement."
```

---

# 28. PG Cashflow Golden Path

For your current repository, the preferred engineering sequence is:

```text
ASD-STE FINANCIAL REQUIREMENTS
              ↓
Money / Ledger Invariants
              ↓
Property Authorization Model
              ↓
Transaction Model
              ↓
Database Constraints
              ↓
Payment + Expense Write Paths
              ↓
Journal / Audit Integrity
              ↓
Dashboard Read Model
              ↓
Scheduled Jobs
              ↓
Unit Tests
              ↓
Integration Tests
              ↓
Concurrency Tests
              ↓
Failure-Injection Tests
              ↓
Security Tests
              ↓
EXPLAIN ANALYZE
              ↓
Static / Dependency / Secret Scans
              ↓
Independent Review
              ↓
Traceability Matrix
              ↓
Acceptance Gate
              ↓
Release Gate
```

# 29. Most Important Rule

For this system, use this principle:

```text
UI correctness
        is not
financial correctness

API validation
        is not
database integrity

Unit tests
        are not
concurrency proof

Code compilation
        is not
system verification

Feature implementation
        is not
requirement acceptance
```

The final standard is:

```text
REQUIREMENT
     ↓
INVARIANT
     ↓
IMPLEMENTATION
     ↓
DATABASE ENFORCEMENT
     ↓
AUTOMATED TEST
     ↓
ADVERSARIAL TEST
     ↓
PERFORMANCE EVIDENCE
     ↓
INDEPENDENT REVIEW
     ↓
ACCEPTANCE
```
