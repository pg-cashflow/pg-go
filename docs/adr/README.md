# Architecture Decision Records (ADR) Index

| ADR | Title | Status | Date |
|:---|:---|:---|:---|
| [ADR-001](file:///c:/Users/divak/Downloads/pg-go/docs/adr/001-reconciliation-control.md) | Dual-Control Ledger Reconciliation Architecture | Accepted | 2026-03-15 |
| [ADR-002](file:///c:/Users/divak/Downloads/pg-go/docs/adr/002-idempotency.md) | Universal Idempotency-Key Design | Accepted | 2026-03-20 |
| [ADR-003](file:///c:/Users/divak/Downloads/pg-go/docs/adr/003-policy-db.md) | Declarative Policy Enforcement Engine | Accepted | 2026-03-25 |
| [ADR-004](file:///c:/Users/divak/Downloads/pg-go/docs/adr/004-tenant-identity-digilocker.md) | Tenant Identity, DigiLocker OAuth & Offline XML | Accepted | 2026-04-01 |
| [ADR-005](file:///c:/Users/divak/Downloads/pg-go/docs/adr/005-owner-payment-screenshot-proof.md) | Owner Offline Payment Proof & OCR Pipeline | Accepted | 2026-04-10 |
| [ADR-006](file:///c:/Users/divak/Downloads/pg-go/docs/adr/006-payment-collection-cashfree.md) | Cashfree Gateway Ingress, Webhooks & Auto-Collect | Accepted | 2026-04-18 |
| [ADR-007](file:///c:/Users/divak/Downloads/pg-go/docs/adr/007-deposit-liability-accounting.md) | Security Deposit Liability & Claim Settlement | Accepted | 2026-04-25 |
| [ADR-008](file:///c:/Users/divak/Downloads/pg-go/docs/adr/008-gateway-clearing-reconciliation.md) | Gateway Clearing Accounts & TDR Accounting | Accepted | 2026-05-02 |
| [ADR-009](file:///c:/Users/divak/Downloads/pg-go/docs/adr/009-payout-approval-lifecycle.md) | Payout Batch Approval & Maker-Checker Lifecycle | Accepted | 2026-05-10 |
| [ADR-010](file:///c:/Users/divak/Downloads/pg-go/docs/adr/010-credential-protection-kms.md) | Secret Keyring Management & Key Derivation | Accepted | 2026-05-18 |
| [ADR-011](file:///c:/Users/divak/Downloads/pg-go/docs/adr/011-minor-tenant-dpdp-compliance.md) | Minor Tenant Guardian Consent & DPDP Compliance | Accepted | 2026-05-25 |
| [ADR-012](file:///c:/Users/divak/Downloads/pg-go/docs/adr/012-search-v2-postgres.md) | PostgreSQL Full-Text Search Architecture | Accepted | 2026-06-05 |
| [ADR-013](file:///c:/Users/divak/Downloads/pg-go/docs/adr/013-search-fanout-merge-observability.md) | Search Fanout, Merging & Latency Tracing | Accepted | 2026-06-12 |
| [ADR-014](file:///c:/Users/divak/Downloads/pg-go/docs/adr/014-search-v2-optimizations.md) | Search Query Optimization & Keyset Paging | Accepted | 2026-06-20 |
| [ADR-015](file:///c:/Users/divak/Downloads/pg-go/docs/adr/015-ledger-integrity-db-controls.md) | Append-Only Triggers & Database Invariants | Accepted | 2026-07-01 |
| [ADR-016](file:///c:/Users/divak/Downloads/pg-go/docs/adr/016-ledger-period-lock-statements-completeness.md) | Accounting Period Lock & Immutable Snapshots | Accepted | 2026-07-15 |
| [ADR-017](file:///c:/Users/divak/Downloads/pg-go/docs/adr/017-postgres-cluster-replicas-pgbouncer-sharding.md) | Connection Pooling, PgBouncer & Replica Strategy | Accepted | 2026-08-01 |
| [ADR-018](file:///c:/Users/divak/Downloads/pg-go/docs/adr/018-rls-scope-wiring-and-db-performance-hardening.md) | RLS Scope Wiring & DB Performance Hardening | Accepted | 2026-08-15 |
| [ADR-019](file:///c:/Users/divak/Downloads/pg-go/docs/adr/019-database-review-scopeddb-defects-rls-scaling.md) | ScopedDB Remediation, RLS Cost & Scaling Triggers | Accepted | 2026-08-25 |
| [ADR-020](file:///c:/Users/divak/Downloads/pg-go/docs/adr/020-rls-privilege-hardening-dual-pools-and-atomic-billing.md) | Dual-Pool Architecture (`pgapp_app` & `pgapp_maint`) | Accepted | 2026-09-05 |
| [ADR-021](file:///c:/Users/divak/Downloads/pg-go/docs/adr/021-scale-capacity-gamification-economics-and-expense-void-defects.md) | Gamification Economics & Expense Void Corrections | Accepted | 2026-09-12 |
| [ADR-022](file:///c:/Users/divak/Downloads/pg-go/docs/adr/022-concurrency-hardening-deploy-pipeline-and-audit-gates.md) | Concurrency Hardening & Continuous Verification | Accepted | 2026-09-20 |
| [ADR-023](file:///c:/Users/divak/Downloads/pg-go/docs/adr/023-route-parity-metrics-failclosed-and-finance-invariants.md) | Route Parity, Metrics Gating & Financial Invariants | Accepted | 2026-09-28 |
| [ADR-024](file:///c:/Users/divak/Downloads/pg-go/docs/adr/024-finance-policy-invariants-error-parity-and-token-hardening.md) | Policy Invariants & Token Hardening | Accepted | 2026-10-02 |
| [ADR-025](file:///c:/Users/divak/Downloads/pg-go/docs/adr/025-verification-hardening-and-dead-field-cleanup.md) | Verification Hardening & Dead Field Cleanup | Accepted | 2026-10-05 |
| [ADR-026](file:///c:/Users/divak/Downloads/pg-go/docs/adr/026-hermetic-load-test-url-override-sandbox-smoke-and-label-reconciliation.md) | Hermetic Load Testing & Sandbox Smoke Harness | Accepted | 2026-10-08 |
| [ADR-027](file:///c:/Users/divak/Downloads/pg-go/docs/adr/027-coverage-ratchet-restoration-cashfree-guard-hardening-and-load-workflow-remediation.md) | Coverage Ratchet Restoration & Cashfree Guarding | Accepted | 2026-10-10 |
| [ADR-028](file:///c:/Users/divak/Downloads/pg-go/docs/adr/028-security-and-invariant-remediation-round-9.md) | Security & Invariant Remediation Round 9 | Accepted | 2026-10-12 |
| [ADR-029](file:///c:/Users/divak/Downloads/pg-go/docs/adr/029-tenant-isolation-remediation.md) | Tenant Isolation & Fail-Closed Controls | Accepted | 2026-10-13 |
| [ADR-030](file:///c:/Users/divak/Downloads/pg-go/docs/adr/030-job-db-role-and-startup-guard.md) | Background Job DB Role (`pgapp_worker`) & Guard | Accepted | 2026-10-14 |
| [ADR-031](file:///c:/Users/divak/Downloads/pg-go/docs/adr/031-webhook-signing-secret-policy.md) | Cashfree Webhook Signing Secret Policy & Length Rule | Accepted | 2026-10-14 |
| [ADR-032](file:///c:/Users/divak/Downloads/pg-go/docs/adr/032-delegated-manager-capabilities.md) | Delegated Manager Property Capabilities & Scoping | Accepted | 2026-10-14 |
| [ADR-033](file:///c:/Users/divak/Downloads/pg-go/docs/adr/033-reserved.md) | Reserved ADR Number | Reserved | 2026-10-14 |
| [ADR-034](file:///c:/Users/divak/Downloads/pg-go/docs/adr/034-webhook-payout-parity-and-metrics-failclosed.md) | Payout Webhook Parity and Metrics Fail-Closed | Accepted | 2026-10-14 |
| [ADR-035](file:///c:/Users/divak/Downloads/pg-go/docs/adr/035-deployment-vps-managed-postgres.md) | Production Deployment (VPS + Managed PostgreSQL) | Accepted | 2026-10-14 |
