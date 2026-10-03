# ADR 012: Search V2 — Live PostgreSQL (Full-Text & pg_trgm) Federated Lookup

## Status
Accepted
**Date:** 2026-10-03
**Decider:** Divakar (Solo Developer)

## Context
The previous search implementation relied on a secondary `search_documents` table populated by an ad-hoc indexer, SHA-256 hash embeddings, and Reciprocal Rank Fusion (RRF). This introduced significant operational and architectural deficiencies:
1. **Index Staleness & Duplication:** `search_documents` had no deletion or update hooks; entries duplicated live table data and stale/deleted records persisted indefinitely (violating DPDP erasure principles).
2. **Artificial Vector Embeddings:** SHA-256 hashing produces pseudorandom noise rather than semantic relevance; external LLM embedding APIs would require shipping sensitive tenant names, inspection notes, and bank narrations to third parties.
3. **Query Inflexibility:** Multi-word queries (e.g., "rahul 201") failed due to monolithic substring matching (`%q%`), and queries wrapped columns in `COALESCE`, defeating trigram index acceleration.
4. **Type Starvation:** Global limits truncated results sequentially by entity type, causing earlier types to starve payments and documents.
5. **PII Scraping Risk:** Unrestricted partial phone queries exposed tenant contact details to unauthorized roles.

## Scale Target
1,000+ users, ~12 dues/year each, and 5 years of historical records yield roughly 100k–500k searchable rows. At ~20 searches per user per day (< 1 QPS average, a few QPS peak), native PostgreSQL comfortably handles the workload with sub-100ms latency without external services.

## Decisions

### 1. Architecture: Native Postgres Live-Table Search
- Remove `search_documents`, SHA-256 embedder, and vector RRF fusion.
- Search live tables directly using PostgreSQL full-text search (`tsvector` + `websearch_to_tsquery`) for free text (inspections, hazards, violations, notes) and `pg_trgm` (`similarity()` + prefix `ILIKE`) for identifiers and names.
- Drop `COALESCE` wrappers in queries to ensure indexes match query predicates directly.

### 2. Query Pipeline
1. **Normalize:** Lowercase and collapse consecutive whitespace; enforce length bounds [2, 100].
2. **Tokenize & Classify:** Split query into tokens:
   - 10+ digits: Phone number
   - Short numeric: Room number or due amount
   - Alphanumeric code-like: Due code, UTR, or identifier
   - Alphabetical/other: Name or free text
3. **Multi-Term AND Conjunction:** All tokens must match across the entity's allowed searchable fields (e.g., "rahul 201" matches a tenant named Rahul in room 201).
4. **Fuzzy Scope:** Trigram similarity is restricted to names at length ≥ 4 characters. Identifiers (UTRs, due codes, phone numbers, amounts) are never fuzzy-matched to prevent financial misattribution.
5. **Deterministic Ranking:** Order by exact match > prefix match > word prefix > trigram similarity > recency (`created_at`/`due_date` DESC).
6. **Per-Type Capping & Pagination:** Return results grouped by type with a per-type cap (default 5) and global limit to eliminate type starvation.

### 3. Role-Based Access Scoping
- **Owner:** Full visibility (tenants, dues, payments, UTR reports, join requests, events, inspections, hazards, violations). Partial phone search permitted.
- **Manager:** Tenants (name, room, exact 10-digit phone match only — no partial phone search to prevent scraping), inspections, hazards, violations. No payments or financial records. Hazard reporter tenant identity is never searchable (anonymity).
- **Tenant:** Strictly scoped to their own `tenant_id` from the JWT claims (own dues and own payments only). Unauthenticated or incomplete tenant profiles fail closed (0 results).

### 4. API Contract Decoupling
- Search results return structured descriptors `{type, id, title, subtitle}`.
- Hardcoded frontend route construction is eliminated from the backend; client applications map `{type, id}` to local routes.

### 5. Migration & Dual-Run Strategy
- Add targeted trigram and FTS indexes via migration `038_search_v2_indexes.sql`.
- Feature flag `search_v2` enables dual-running and canary validation.
- Decommission `search_documents` table in a future forward-only migration once verified.

## Consequences
- Target < 5ms search execution for standard lookups, targeting zero index sync lag or stale data leaks through direct live-table queries.
- DPDP compliance: no derived text copies, strict role-based PII scoping, and immediate reflection of updates/deletions.
- Elimination of unneeded third-party dependencies, network hops, and operational services.
