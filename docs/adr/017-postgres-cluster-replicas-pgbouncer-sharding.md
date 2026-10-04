# ADR-017: PostgreSQL Cluster Architecture, Read Replicas, PgBouncer, and Horizontal Sharding

**Status:** Accepted  
**Date:** 2026-10-04  
**Deciders:** Engineering Lead, Core Platform Team  
**Builds on:** [ADR-015](015-ledger-integrity-db-controls.md), [ADR-016](016-ledger-period-lock-statements-completeness.md)  
**Related:** Master Task Specification (Sections 6–17)

---

## 1. Context and Problem Statement

As PG Cashflow expands across properties, tenants, and background workers, the persistence layer must handle:
1. **High Concurrency and Connection Pooling:** Preventing connection exhaustion under burst loads, supporting PgBouncer transaction-mode connection multiplexing, and enforcing custom TCP keep-alives and timeouts.
2. **Read/Write Workload Segregation:** Directing writes, transactions, and row-level locks strictly to the Primary database while offloading analytics, reporting, and read-heavy queries to Read Replicas via round-robin distribution with transparent primary fallback.
3. **Observability and Query Performance:** Real-time query latency tracing, slow-query alerting, error tracking, and statistics collection without external dependencies.
4. **Horizontal Scaling and Sharding:** Providing a deterministic path from a single PostgreSQL instance to multi-cluster horizontal sharding (consistent hashing and map-based routing by `property_id`) without requiring destructive application rewrites.

---

## 2. Decision and Architecture

### 2.1 Connection Pooling and PgBouncer (`internal/postgres/cluster.go`, `db.go`)
- **Configurable Pool Envelope:** `DATABASE_MAX_CONNS` (default 25), `MinConns` (`max(2, maxConns/5)`), `MaxConnLifetime` (1h), `MaxConnIdleTime` (15m), and `HealthCheckPeriod` (1m).
- **Socket-Level Tuning:** Custom `net.Dialer` with configurable `ConnectTimeout` (10s default) and `TCPKeepAlive` (30s default) to detect and terminate zombie connections across cloud networks.
- **PgBouncer Multiplexing Mode:** Automatic auto-detection on port 6432 or explicit `DATABASE_PGBOUNCER=true`:
  - Enforces `pgx.QueryExecModeSimpleProtocol`.
  - Disables client-side prepared statement and description caching (`StatementCacheCapacity: 0`, `DescriptionCacheCapacity: 0`), preventing cross-transaction prepared statement collision errors.
  - In direct mode (default), uses `StatementCacheCapacity: 512` for ultra-low latency query planning.

### 2.2 DBCluster and Read-Replica Routing (`internal/postgres/cluster.go`)
- **Primary vs. Replica Separation:**
  - `Primary()` and `Writer()`: Exclusively returns the primary read/write pool for all mutations, row-level locks (`FOR UPDATE`), and ACID transactions (`WithinTx()`).
  - `Replica()` and `Reader()`: Distributes read queries across configured read-replicas using atomic round-robin counter (`rrCounter.Add(1)`).
- **Transparent Fallback:** If zero replicas are configured or available, `Replica()` transparently falls back to `Primary()`, maintaining 100% backward compatibility for single-node deployments.
- **Credential Protection:** `RedactURL()` and `sanitizeHealthErr()` strip passwords and sensitive connection parameters from logs, telemetry, and health check reports.
- **Lock-Free Health Checks:** `Health(ctx)` takes snapshot pointers under a read lock and ping checks nodes without holding locks during network I/O, ensuring cluster shutdown (`Close()`) is never blocked by slow network pings.

### 2.3 Query Performance Tracing (`internal/postgres/query_perf.go`)
- Implements `pgx.QueryTracer` interface (`TraceQueryStart` and `TraceQueryEnd`).
- Tracks `TotalQueries`, `SlowQueries`, `ErrorQueries`, `AvgDuration`, and `MaxDuration` using atomic primitives (`atomic.Uint64`, `atomic.Int64`).
- Automatically logs slow queries exceeding `DATABASE_SLOW_QUERY_THRESHOLD` (default 100ms) with SQL snippet, arguments count, and execution duration.
- Excludes `pgx.ErrNoRows` from error query counts as it represents normal business-logic lookups.

### 2.4 Horizontal Sharding Architecture (`internal/postgres/sharding.go`)
- `ShardRouter` Interface defines a contract for multi-cluster routing:
  - `GetShard(propertyID uuid.UUID) *DBCluster`
  - `Default() *DBCluster`
  - `AllShards() []*DBCluster`
  - `Close()`
- **Implementations:**
  1. `SingleShardRouter`: Zero-overhead default wrapping a single `DBCluster`.
  2. `ConsistentHashShardRouter`: Deterministic consistent hashing using SHA-256 over `property_id` modulo $N$ shards, evenly distributing tenant and property data across independent PostgreSQL clusters.
  3. `MapShardRouter`: Explicit property-to-shard mapping allowing dedicated clusters for enterprise properties, with fallback to the default cluster.

---

## 3. Verification and Evidence

- `cluster_test.go`:
  - `TestRedactURL`: Verified credential masking in connection strings.
  - `TestConfigurePoolConfig_DirectMode`: Verified 512 statement cache and socket timeouts.
  - `TestConfigurePoolConfig_PgBouncerMode`: Verified zero statement cache and simple protocol mode.
  - `TestDBCluster_RoutingAndFallback`: Verified round-robin distribution and transparent fallback to primary.
  - `TestDBCluster_Stats`: Verified connection and query performance metrics collection.
  - `TestDBCluster_CloseAndHealthSanitization`: Verified idempotent close and health error sanitization.
  - `TestNewClusterFromEnv_MissingURL`: Verified fail-closed startup when connection strings are missing.
  - `TestNewClusterFromEnv_LiveDB`: Verified live initialization, transaction execution, health report, and pool metrics against live PostgreSQL.
- `query_perf_test.go`:
  - `TestQueryPerfTracer_FastQuery`: Verified fast queries are tracked without being flagged slow.
  - `TestQueryPerfTracer_SlowQueryAndError`: Verified slow query threshold alerting and error counting.
  - `TestQueryPerfTracer_IgnoreErrNoRows`: Verified `pgx.ErrNoRows` is not classified as an error.
- `sharding_test.go`:
  - `TestSingleShardRouter`: Verified single-cluster pass-through.
  - `TestConsistentHashShardRouter_Distribution`: Verified deterministic and even distribution across 300 sampled UUIDs.
  - `TestConsistentHashShardRouter_Validation`: Verified rejection of empty or nil shard clusters.
  - `TestMapShardRouter`: Verified explicit assignment and fallback routing.
