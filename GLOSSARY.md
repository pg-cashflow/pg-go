# Domain Glossary: pg-cashflow

This glossary defines standard domain terms and vocabulary for the PG/hostel billing backend and data models. Use these exact domain terms when defining modules, interfaces, and seams.

---

### Core Domain Entities

- **Property**: The tenant housing unit, hostel, or paying guest facility. Identified by a unique `property_id` (UUID). All multi-tenant isolation pivots on this identity.
- **Tenant**: An occupant residing at a Property with a signed agreement, contractual due ceiling, and assigned room.
- **Due**: An outstanding monetary obligation accrued by a Tenant (rent, maintenance, utility surcharge, or late fee) denominated strictly in integer paise.
- **Payment**: An inflow transaction verified via UPI UTR or Payment Gateway (Cashfree), crediting a tenant's due balance or overpayment credit balance.
- **PayoutItem**: An approved disbursement line item owed to an owner, vendor, or departing tenant (e.g., security deposit refund).
- **PayoutBatch**: An immutable bundle of verified `PayoutItem` records compiled for atomic disbursement, guarded by cooling-off invariants and signed with an HMAC checksum.
- **PayoutEngine**: The domain execution module responsible for compiling unbatched payout items into a validated `PayoutBatch` within an authenticated property scope.
- **ScopedStorage**: The database isolation boundary adapter that guarantees `SET LOCAL app.current_property_id` and role switching (`pgapp_app`) within every database transaction before queries execute.
- **SettlementWorkflow**: The double-entry accounting process that reconciles bank deposits, gateway settlements, and expense approvals against the financial ledger.
- **FinancialJournal**: The immutable ledger containing double-entry debit and credit lines where `SUM(debits) == SUM(credits)`.
- **Cooling-Off Guard**: A security requirement (REQ-SEC-007) that blocks payout batch inclusion for any payee registered less than 24 hours prior.
