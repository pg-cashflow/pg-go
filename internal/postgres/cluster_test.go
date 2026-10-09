package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "postgres://postgres:supersecret@localhost:5432/pg_dev",
			expected: "postgres://postgres:***@localhost:5432/pg_dev",
		},
		{
			input:    "postgres://admin:password123@db.example.com:6432/production?sslmode=require",
			expected: "postgres://admin:***@db.example.com:6432/production",
		},
		{
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		got := RedactURL(tt.input)
		if got != tt.expected {
			t.Errorf("RedactURL(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestConfigurePoolConfig_DirectMode(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://postgres:password@localhost:5432/pg_dev")
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}

	opt := PoolOptions{
		MaxConns:       50,
		MinConns:       10,
		ConnectTimeout: 5 * time.Second,
		TCPKeepAlive:   15 * time.Second,
		PgBouncer:      false,
		AppName:        "test-direct",
	}

	ConfigurePoolConfig(cfg, opt)

	if cfg.MaxConns != 50 {
		t.Errorf("expected MaxConns=50, got %d", cfg.MaxConns)
	}
	if cfg.MinConns != 10 {
		t.Errorf("expected MinConns=10, got %d", cfg.MinConns)
	}
	if cfg.ConnConfig.ConnectTimeout != 5*time.Second {
		t.Errorf("expected ConnectTimeout=5s, got %v", cfg.ConnConfig.ConnectTimeout)
	}
	if cfg.ConnConfig.RuntimeParams["application_name"] != "test-direct" {
		t.Errorf("expected appName test-direct, got %s", cfg.ConnConfig.RuntimeParams["application_name"])
	}
	if cfg.ConnConfig.StatementCacheCapacity != 512 {
		t.Errorf("expected direct mode StatementCacheCapacity=512, got %d", cfg.ConnConfig.StatementCacheCapacity)
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement && cfg.ConnConfig.DefaultQueryExecMode != 0 {
		t.Errorf("expected default cache statement exec mode for direct, got %v", cfg.ConnConfig.DefaultQueryExecMode)
	}
}

func TestConfigurePoolConfig_PgBouncerMode(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://postgres:password@localhost:6432/pg_dev")
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}

	opt := PoolOptions{
		PgBouncer: true,
	}

	ConfigurePoolConfig(cfg, opt)

	if cfg.ConnConfig.StatementCacheCapacity != 0 {
		t.Errorf("expected PgBouncer mode StatementCacheCapacity=0, got %d", cfg.ConnConfig.StatementCacheCapacity)
	}
	if cfg.ConnConfig.DescriptionCacheCapacity != 0 {
		t.Errorf("expected PgBouncer mode DescriptionCacheCapacity=0, got %d", cfg.ConnConfig.DescriptionCacheCapacity)
	}
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Errorf("expected PgBouncer mode QueryExecModeSimpleProtocol, got %v", cfg.ConnConfig.DefaultQueryExecMode)
	}
}

func TestDBCluster_RoutingAndFallback(t *testing.T) {
	primary := &pgxpool.Pool{}
	rep1 := &pgxpool.Pool{}
	rep2 := &pgxpool.Pool{}

	cluster := &DBCluster{
		primaryPool:  primary,
		replicaPools: []*pgxpool.Pool{rep1, rep2},
	}

	// Primary should always return primary
	if cluster.Primary() != primary {
		t.Errorf("expected Primary() to return primary")
	}
	if cluster.Writer() != primary {
		t.Errorf("expected Writer() to return primary")
	}

	// Replicas should alternate in round-robin
	got1 := cluster.Replica()
	got2 := cluster.Replica()
	got3 := cluster.Replica()

	if got1 != rep1 || got2 != rep2 || got3 != rep1 {
		t.Errorf("expected round robin between rep1 and rep2, got: %v, %v, %v", got1, got2, got3)
	}

	// Test fallback when no replicas configured
	clusterNoRep := &DBCluster{
		primaryPool: primary,
	}
	if clusterNoRep.Replica() != primary {
		t.Errorf("expected fallback to primary when no replicas configured")
	}
	if clusterNoRep.Reader() != primary {
		t.Errorf("expected Reader() fallback to primary")
	}
}

func TestDBCluster_Stats(t *testing.T) {
	cluster := &DBCluster{
		tracer: NewQueryPerfTracer(100*time.Millisecond, nil),
	}

	stats := cluster.Stats()
	if stats.ReplicaPoolsCount != 0 {
		t.Errorf("expected 0 replicas, got %d", stats.ReplicaPoolsCount)
	}

	// Trigger a dummy query on tracer
	ctx := cluster.Tracer().TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	cluster.Tracer().TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

	stats = cluster.Stats()
	if stats.QueryPerf.TotalQueries != 1 {
		t.Errorf("expected 1 tracked query, got %d", stats.QueryPerf.TotalQueries)
	}
}

func TestDBCluster_CloseAndHealthSanitization(t *testing.T) {
	cluster := &DBCluster{}
	if cluster.IsClosed() {
		t.Errorf("new cluster should not be closed")
	}

	cluster.Close()
	if !cluster.IsClosed() {
		t.Errorf("cluster should be marked closed after Close()")
	}

	// Double close should be idempotent
	cluster.Close()
	if !cluster.IsClosed() {
		t.Errorf("cluster should still be marked closed after second Close()")
	}

	// Test error sanitization
	rawErr := &mockCustomError{msg: "connection to postgres://user:secretpass@10.0.0.1:5432/mydb failed"}
	sanitized := sanitizeHealthErr(rawErr)
	if strings.Contains(sanitized, "secretpass") {
		t.Errorf("password leaked in sanitized health error: %s", sanitized)
	}
	if !strings.Contains(sanitized, "postgres://user:***@10.0.0.1:5432/mydb") {
		t.Errorf("expected redacted URL in error, got: %s", sanitized)
	}
}

type mockCustomError struct {
	msg string
}

func (m *mockCustomError) Error() string {
	return m.msg
}

func TestNewClusterFromEnv_MissingURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	_, err := NewClusterFromEnv(context.Background(), "")
	if err == nil {
		t.Fatalf("expected error when DATABASE_URL is unset")
	}
}

func TestNewClusterFromEnv_LiveDB(t *testing.T) {
	_ = godotenv.Load("../../.env")
	dbURL := testutil.RequireDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cluster, err := NewClusterFromEnv(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to initialize cluster from env: %v", err)
	}
	defer cluster.Close()

	if cluster.Primary() == nil {
		t.Fatalf("expected non-nil Primary pool")
	}
	if cluster.Replica() == nil {
		t.Fatalf("expected non-nil Replica pool (should fallback to Primary)")
	}
	if cluster.Reader() == nil {
		t.Fatalf("expected non-nil Reader DBTX")
	}
	if cluster.Writer() == nil {
		t.Fatalf("expected non-nil Writer DBTX")
	}

	// Verify WithinTx works
	err = cluster.WithinTx(ctx, func(tx pgx.Tx) error {
		var one int
		return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
	})
	if err != nil {
		t.Fatalf("WithinTx failed: %v", err)
	}

	// Verify Health check
	h := cluster.Health(ctx)
	if !h.PrimaryHealthy {
		t.Fatalf("expected primary to be healthy in health report: %+v", h)
	}

	// Verify Stats
	stats := cluster.Stats()
	if stats.PrimaryTotalConns <= 0 {
		t.Fatalf("expected positive PrimaryTotalConns, got %d", stats.PrimaryTotalConns)
	}
}
