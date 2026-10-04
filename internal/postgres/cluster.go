package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolOptions holds configuration options for tuning an individual pgx connection pool.
type PoolOptions struct {
	MaxConns                 int
	MinConns                 int
	MaxConnLifetime          time.Duration
	MaxConnIdleTime          time.Duration
	HealthCheckPeriod        time.Duration
	ConnectTimeout           time.Duration
	TCPKeepAlive             time.Duration
	PgBouncer                bool
	AppName                  string
	StatementTimeout         time.Duration
	IdleInTransactionTimeout time.Duration
	LockTimeout              time.Duration
	Tracer                   pgx.QueryTracer
}

// ClusterConfig configures a database cluster containing a primary node and optional read replicas.
type ClusterConfig struct {
	PrimaryURL               string
	ReplicaURLs              []string
	MaxConns                 int
	MinConns                 int
	MaxConnLifetime          time.Duration
	MaxConnIdleTime          time.Duration
	HealthCheckPeriod        time.Duration
	ConnectTimeout           time.Duration
	TCPKeepAlive             time.Duration
	PgBouncer                bool
	SlowQueryThreshold       time.Duration
	StatementTimeout         time.Duration
	IdleInTransactionTimeout time.Duration
	LockTimeout              time.Duration
	Logger                   *slog.Logger
}

// ClusterPoolHealth represents the health status of an individual pool in the cluster.
type ClusterPoolHealth struct {
	Role       string        `json:"role"` // "primary" or "replica"
	URL        string        `json:"url"`  // Redacted URL
	Healthy    bool          `json:"healthy"`
	Latency    time.Duration `json:"latency"`
	Error      string        `json:"error,omitempty"`
	TotalConns int32         `json:"total_conns"`
	IdleConns  int32         `json:"idle_conns"`
}

// ReplicaLagInfo captures streaming replication lag metrics from pg_stat_replication.
type ReplicaLagInfo struct {
	ClientAddr string `json:"client_addr"`
	AppName    string `json:"application_name"`
	State      string `json:"state"`
	LagBytes   int64  `json:"lag_bytes"`
}

// ClusterHealthReport summarizes the overall health of the DB cluster.
type ClusterHealthReport struct {
	PrimaryHealthy  bool                `json:"primary_healthy"`
	ReplicasTotal   int                 `json:"replicas_total"`
	ReplicasHealthy int                 `json:"replicas_healthy"`
	Pools           []ClusterPoolHealth `json:"pools"`
	ReplicationLag  []ReplicaLagInfo    `json:"replication_lag,omitempty"`
}

// ClusterStats reports aggregated connection and query performance metrics.
type ClusterStats struct {
	PrimaryTotalConns int32          `json:"primary_total_conns"`
	PrimaryIdleConns  int32          `json:"primary_idle_conns"`
	ReplicaPoolsCount int            `json:"replica_pools_count"`
	QueryPerf         QueryPerfStats `json:"query_perf"`
}

// DBCluster coordinates connection pools across a Primary (Read/Write) database
// and optional Read-Replicas with automatic round-robin load distribution and transparent primary fallback.
type DBCluster struct {
	primaryPool  *pgxpool.Pool
	replicaPools []*pgxpool.Pool
	tracer       *QueryPerfTracer
	logger       *slog.Logger
	rrCounter    atomic.Uint64
	mu           sync.RWMutex
	closed       bool
}

// RedactURL strips credentials from a PostgreSQL connection string for safe diagnostics and logging.
func RedactURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	cfg, err := pgxpool.ParseConfig(rawURL)
	if err != nil {
		return "[malformed database url]"
	}
	host := cfg.ConnConfig.Host
	if host == "" {
		host = "localhost"
	}
	port := cfg.ConnConfig.Port
	if port == 0 {
		port = 5432
	}
	dbName := cfg.ConnConfig.Database
	user := cfg.ConnConfig.User
	if user != "" {
		return fmt.Sprintf("postgres://%s:***@%s:%d/%s", user, host, port, dbName)
	}
	return fmt.Sprintf("postgres://%s:%d/%s", host, port, dbName)
}

// ConfigurePoolConfig applies socket-level keep-alives, connection timeouts, PgBouncer compatibility
// modes, and statement cache tuning to a pgxpool.Config.
func ConfigurePoolConfig(cfg *pgxpool.Config, opt PoolOptions) {
	if opt.MaxConns <= 0 {
		opt.MaxConns = 25
	}
	cfg.MaxConns = int32(opt.MaxConns)

	if opt.MinConns < 0 {
		opt.MinConns = opt.MaxConns / 5
		if opt.MinConns < 2 {
			opt.MinConns = 2
		}
	}
	cfg.MinConns = int32(opt.MinConns)

	if opt.MaxConnLifetime <= 0 {
		opt.MaxConnLifetime = time.Hour
	}
	cfg.MaxConnLifetime = opt.MaxConnLifetime

	if opt.MaxConnIdleTime <= 0 {
		opt.MaxConnIdleTime = 15 * time.Minute
	}
	cfg.MaxConnIdleTime = opt.MaxConnIdleTime

	if opt.HealthCheckPeriod <= 0 {
		opt.HealthCheckPeriod = time.Minute
	}
	cfg.HealthCheckPeriod = opt.HealthCheckPeriod

	// Network socket tuning: custom dialer with keepalive
	connectTimeout := opt.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	tcpKeepAlive := opt.TCPKeepAlive
	if tcpKeepAlive <= 0 {
		tcpKeepAlive = 30 * time.Second
	}

	dialer := &net.Dialer{
		Timeout:   connectTimeout,
		KeepAlive: tcpKeepAlive,
	}
	cfg.ConnConfig.DialFunc = dialer.DialContext
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = make(map[string]string)
	}
	appName := opt.AppName
	if appName == "" {
		appName = "pg-go"
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName

	statementTimeout := opt.StatementTimeout
	if statementTimeout <= 0 {
		statementTimeout = 30 * time.Second
	}
	idleInTxTimeout := opt.IdleInTransactionTimeout
	if idleInTxTimeout <= 0 {
		idleInTxTimeout = 60 * time.Second
	}
	lockTimeout := opt.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = 10 * time.Second
	}

	cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(statementTimeout.Milliseconds(), 10)
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = strconv.FormatInt(idleInTxTimeout.Milliseconds(), 10)
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = strconv.FormatInt(lockTimeout.Milliseconds(), 10)

	// PgBouncer transaction-pooling compatibility:
	// In PgBouncer transaction mode, prepared statements cannot be cached across transactions.
	// Setting QueryExecModeSimpleProtocol and zeroing cache capacity guarantees zero prepared-statement collision errors.
	if opt.PgBouncer {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
		cfg.ConnConfig.StatementCacheCapacity = 0
		cfg.ConnConfig.DescriptionCacheCapacity = 0
	} else {
		// Direct connection: statement cache gives ultra-low latency query planning.
		cfg.ConnConfig.StatementCacheCapacity = 512
		cfg.ConnConfig.DescriptionCacheCapacity = 512
	}

	if opt.Tracer != nil {
		cfg.ConnConfig.Tracer = opt.Tracer
	}
}

// NewCluster initializes a DBCluster with a primary connection pool and optional read-replica pools.
func NewCluster(ctx context.Context, ccfg ClusterConfig) (*DBCluster, error) {
	if ccfg.PrimaryURL == "" {
		return nil, fmt.Errorf("primary database url is required")
	}
	logger := ccfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tracer := NewQueryPerfTracer(ccfg.SlowQueryThreshold, logger)

	// Build default pool options
	baseOpt := PoolOptions{
		MaxConns:                 ccfg.MaxConns,
		MinConns:                 ccfg.MinConns,
		MaxConnLifetime:          ccfg.MaxConnLifetime,
		MaxConnIdleTime:          ccfg.MaxConnIdleTime,
		HealthCheckPeriod:        ccfg.HealthCheckPeriod,
		ConnectTimeout:           ccfg.ConnectTimeout,
		TCPKeepAlive:             ccfg.TCPKeepAlive,
		PgBouncer:                ccfg.PgBouncer,
		StatementTimeout:         ccfg.StatementTimeout,
		IdleInTransactionTimeout: ccfg.IdleInTransactionTimeout,
		LockTimeout:              ccfg.LockTimeout,
		AppName:                  "pg-go-primary",
		Tracer:                   tracer,
	}

	primaryCfg, err := pgxpool.ParseConfig(ccfg.PrimaryURL)
	if err != nil {
		return nil, fmt.Errorf("parse primary database url: %w", err)
	}
	ConfigurePoolConfig(primaryCfg, baseOpt)

	primaryPool, err := pgxpool.NewWithConfig(ctx, primaryCfg)
	if err != nil {
		return nil, fmt.Errorf("connect primary database: %w", err)
	}
	if err := primaryPool.Ping(ctx); err != nil {
		primaryPool.Close()
		return nil, fmt.Errorf("ping primary database: %w", err)
	}

	cluster := &DBCluster{
		primaryPool: primaryPool,
		tracer:      tracer,
		logger:      logger,
	}

	// Initialize read replicas if configured
	for i, replicaURL := range ccfg.ReplicaURLs {
		replicaURL = strings.TrimSpace(replicaURL)
		if replicaURL == "" {
			continue
		}
		replicaCfg, err := pgxpool.ParseConfig(replicaURL)
		if err != nil {
			logger.Warn("skipping invalid replica database url", "index", i, "err", err)
			continue
		}
		repOpt := baseOpt
		repOpt.AppName = fmt.Sprintf("pg-go-replica-%d", i+1)
		ConfigurePoolConfig(replicaCfg, repOpt)

		repPool, err := pgxpool.NewWithConfig(ctx, replicaCfg)
		if err != nil {
			logger.Warn("failed to initialize replica pool, skipping", "index", i, "err", err)
			continue
		}
		if err := repPool.Ping(ctx); err != nil {
			logger.Warn("replica ping failed at startup, skipping", "index", i, "err", err)
			repPool.Close()
			continue
		}
		cluster.replicaPools = append(cluster.replicaPools, repPool)
	}

	logger.Info("db cluster initialized",
		slog.String("primary", RedactURL(ccfg.PrimaryURL)),
		slog.Int("replicas_count", len(cluster.replicaPools)),
		slog.Bool("pgbouncer", ccfg.PgBouncer),
	)

	return cluster, nil
}

// NewClusterFromEnv initializes a DBCluster using primaryURL and any comma-separated replica URLs
// specified in DATABASE_REPLICA_URLS or REPLICA_DATABASE_URLS environment variables.
// Connection pool options, timeouts, keep-alives, and PgBouncer modes are derived from environment variables.
func NewClusterFromEnv(ctx context.Context, primaryURL string) (*DBCluster, error) {
	if primaryURL == "" {
		primaryURL = os.Getenv("DATABASE_URL")
	}
	if primaryURL == "" {
		return nil, fmt.Errorf("primary database url is required (DATABASE_URL unset)")
	}

	opt := defaultPoolOptions("pg-go-primary")

	var replicas []string
	replicaEnv := os.Getenv("DATABASE_REPLICA_URLS")
	if replicaEnv == "" {
		replicaEnv = os.Getenv("REPLICA_DATABASE_URLS")
	}
	if replicaEnv != "" {
		for _, part := range strings.Split(replicaEnv, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				replicas = append(replicas, part)
			}
		}
	}

	slowQueryThreshold := 100 * time.Millisecond
	if s := os.Getenv("DATABASE_SLOW_QUERY_THRESHOLD"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			slowQueryThreshold = d
		}
	}

	ccfg := ClusterConfig{
		PrimaryURL:         primaryURL,
		ReplicaURLs:        replicas,
		MaxConns:           opt.MaxConns,
		MinConns:           opt.MinConns,
		MaxConnLifetime:    opt.MaxConnLifetime,
		MaxConnIdleTime:    opt.MaxConnIdleTime,
		HealthCheckPeriod:  opt.HealthCheckPeriod,
		ConnectTimeout:     opt.ConnectTimeout,
		TCPKeepAlive:       opt.TCPKeepAlive,
		PgBouncer:          opt.PgBouncer,
		SlowQueryThreshold: slowQueryThreshold,
	}

	return NewCluster(ctx, ccfg)
}

// Primary returns the primary read/write connection pool.
// All writes, row-level locks, and transactions MUST run on Primary.
func (c *DBCluster) Primary() *pgxpool.Pool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.primaryPool
}

// Replica returns a read-replica pool selected via round-robin.
// If no replicas are configured or available, it transparently returns Primary.
func (c *DBCluster) Replica() *pgxpool.Pool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	n := len(c.replicaPools)
	if n == 0 {
		return c.primaryPool
	}

	// Round-robin selection
	idx := c.rrCounter.Add(1) - 1
	pool := c.replicaPools[idx%uint64(n)]
	if pool != nil {
		return pool
	}
	return c.primaryPool
}

// Reader returns a DBTX connection suitable for read-only queries (using a replica if available).
func (c *DBCluster) Reader() DBTX {
	return c.Replica()
}

// Writer returns a DBTX connection suitable for mutating queries (always Primary).
func (c *DBCluster) Writer() DBTX {
	return c.Primary()
}

// WithinTx executes fn inside a transaction strictly on the Primary pool.
func (c *DBCluster) WithinTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return WithinTx(ctx, c.Primary(), fn)
}

// Tracer returns the active QueryPerfTracer.
func (c *DBCluster) Tracer() *QueryPerfTracer {
	return c.tracer
}

// Stats returns snapshot statistics of connection pools and query performance.
func (c *DBCluster) Stats() ClusterStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var pTotal, pIdle int32
	if c.primaryPool != nil {
		stat := c.primaryPool.Stat()
		pTotal = stat.TotalConns()
		pIdle = stat.IdleConns()
	}

	var perf QueryPerfStats
	if c.tracer != nil {
		perf = c.tracer.Stats()
	}

	return ClusterStats{
		PrimaryTotalConns: pTotal,
		PrimaryIdleConns:  pIdle,
		ReplicaPoolsCount: len(c.replicaPools),
		QueryPerf:         perf,
	}
}

// Health checks the ping latency of Primary and all Read-Replicas.
//
// CORRECTNESS: The read lock is released before each Ping() call to prevent
// blocking Close() (which requires the write lock) for the full TCP connect
// timeout. Pool pointer snapshots are taken under the lock; their use after
// release is safe because pgxpool.Pool.Ping() and .Stat() are goroutine-safe
// and tolerate concurrent closure.
func (c *DBCluster) Health(ctx context.Context) ClusterHealthReport {
	// Snapshot pool pointers under read lock, then release before I/O.
	c.mu.RLock()
	primary := c.primaryPool
	replicas := make([]*pgxpool.Pool, len(c.replicaPools))
	copy(replicas, c.replicaPools)
	c.mu.RUnlock()

	report := ClusterHealthReport{
		ReplicasTotal: len(replicas),
	}

	// Primary health check (lock not held during network I/O)
	start := time.Now()
	err := primary.Ping(ctx)
	pStat := primary.Stat()
	pHealth := ClusterPoolHealth{
		Role:       "primary",
		Healthy:    err == nil,
		Latency:    time.Since(start),
		TotalConns: pStat.TotalConns(),
		IdleConns:  pStat.IdleConns(),
	}
	if err != nil {
		pHealth.Error = sanitizeHealthErr(err)
	} else {
		report.PrimaryHealthy = true
	}
	report.Pools = append(report.Pools, pHealth)

	// Replicas health check (lock not held during network I/O)
	for i, rep := range replicas {
		rStart := time.Now()
		rErr := rep.Ping(ctx)
		rStat := rep.Stat()
		rHealth := ClusterPoolHealth{
			Role:       fmt.Sprintf("replica-%d", i+1),
			Healthy:    rErr == nil,
			Latency:    time.Since(rStart),
			TotalConns: rStat.TotalConns(),
			IdleConns:  rStat.IdleConns(),
		}
		if rErr != nil {
			rHealth.Error = sanitizeHealthErr(rErr)
		} else {
			report.ReplicasHealthy++
		}
		report.Pools = append(report.Pools, rHealth)
	}

	if report.PrimaryHealthy && primary != nil {
		report.ReplicationLag = c.checkReplicationLag(ctx, primary)
	}

	return report
}

func (c *DBCluster) checkReplicationLag(ctx context.Context, primary *pgxpool.Pool) []ReplicaLagInfo {
	qCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	rows, err := primary.Query(qCtx, `
		SELECT coalesce(client_addr::text, ''), coalesce(application_name, ''), coalesce(state, ''),
		       coalesce(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn), 0)::bigint
		FROM pg_stat_replication`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var lags []ReplicaLagInfo
	for rows.Next() {
		var info ReplicaLagInfo
		if err := rows.Scan(&info.ClientAddr, &info.AppName, &info.State, &info.LagBytes); err == nil {
			if info.LagBytes > 100*1024*1024 { // 100MB threshold
				if c.logger != nil {
					c.logger.Warn("high replica lag detected",
						slog.String("replica", info.AppName),
						slog.String("addr", info.ClientAddr),
						slog.Int64("lag_bytes", info.LagBytes),
					)
				}
			}
			lags = append(lags, info)
		}
	}
	return lags
}

var dbCredsRegex = regexp.MustCompile(`postgres://([^:\s]+):([^@\s]+)@`)

func sanitizeHealthErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "postgres://") {
		msg = dbCredsRegex.ReplaceAllString(msg, "postgres://$1:***@")
	}
	return msg
}

// IsClosed reports whether the cluster has been shut down via Close.
func (c *DBCluster) IsClosed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.closed
}

// Close gracefully closes all pools in the cluster.
func (c *DBCluster) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.primaryPool != nil {
		c.primaryPool.Close()
	}
	for _, rep := range c.replicaPools {
		if rep != nil {
			rep.Close()
		}
	}
}
