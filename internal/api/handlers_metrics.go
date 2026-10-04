package api

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/gin-gonic/gin"
)

// MetricsResponse holds operational database, runtime, and performance telemetry.
type MetricsResponse struct {
	Status    string           `json:"status"`
	Runtime   RuntimeMetrics   `json:"runtime"`
	Database  DatabaseMetrics  `json:"database"`
	QueryPerf QueryPerfSummary `json:"query_performance"`
	Outbox    *OutboxMetrics   `json:"outbox,omitempty"`
}

type OutboxMetrics struct {
	PendingCount        int64   `json:"pending_count"`
	DeadLetterCount     int64   `json:"dead_letter_count"`
	RetryingCount       int64   `json:"retrying_count"`
	OldestPendingAgeSec float64 `json:"oldest_pending_age_sec"`
}

type RuntimeMetrics struct {
	NumGoroutine   int    `json:"num_goroutines"`
	AllocBytes     uint64 `json:"alloc_bytes"`
	TotalAlloc     uint64 `json:"total_alloc_bytes"`
	SysBytes       uint64 `json:"sys_bytes"`
	NumGC          uint32 `json:"num_gc"`
	GoVersion      string `json:"go_version"`
}

type DatabaseMetrics struct {
	TotalConns    int32 `json:"total_conns"`
	AcquiredConns int32 `json:"acquired_conns"`
	IdleConns     int32 `json:"idle_conns"`
	MaxConns      int32 `json:"max_conns"`
}

type QueryPerfSummary struct {
	TotalQueries  uint64  `json:"total_queries"`
	SlowQueries   uint64  `json:"slow_queries"`
	ErrorQueries  uint64  `json:"error_queries"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	P50DurationMs float64 `json:"p50_duration_ms"`
	P95DurationMs float64 `json:"p95_duration_ms"`
	P99DurationMs float64 `json:"p99_duration_ms"`
	MaxDurationMs float64 `json:"max_duration_ms"`
}

// Metrics handles GET /metrics and GET /api/metrics.
// Supports application/json format or Prometheus text format based on Accept header.
func (h *Handlers) Metrics(c *gin.Context) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	rtMetrics := RuntimeMetrics{
		NumGoroutine: runtime.NumGoroutine(),
		AllocBytes:   m.Alloc,
		TotalAlloc:   m.TotalAlloc,
		SysBytes:     m.Sys,
		NumGC:        m.NumGC,
		GoVersion:    runtime.Version(),
	}

	var dbMetrics DatabaseMetrics
	if h.Pool != nil {
		stat := h.Pool.Stat()
		dbMetrics = DatabaseMetrics{
			TotalConns:    stat.TotalConns(),
			AcquiredConns: stat.AcquiredConns(),
			IdleConns:     stat.IdleConns(),
			MaxConns:      stat.MaxConns(),
		}
	}

	var perfSummary QueryPerfSummary
	if h.DBCluster != nil {
		stats := h.DBCluster.Stats()
		perfSummary = QueryPerfSummary{
			TotalQueries:  stats.QueryPerf.TotalQueries,
			SlowQueries:   stats.QueryPerf.SlowQueries,
			ErrorQueries:  stats.QueryPerf.ErrorQueries,
			AvgDurationMs: float64(stats.QueryPerf.AvgDuration.Microseconds()) / 1000.0,
			P50DurationMs: float64(stats.QueryPerf.P50Duration.Microseconds()) / 1000.0,
			P95DurationMs: float64(stats.QueryPerf.P95Duration.Microseconds()) / 1000.0,
			P99DurationMs: float64(stats.QueryPerf.P99Duration.Microseconds()) / 1000.0,
			MaxDurationMs: float64(stats.QueryPerf.MaxDuration.Microseconds()) / 1000.0,
		}
	}

	var outboxMetrics *OutboxMetrics
	if h.LedgerOutboxRepo != nil {
		stats, err := h.LedgerOutboxRepo.GetOutboxQueueStats(c.Request.Context())
		if err == nil {
			outboxMetrics = &OutboxMetrics{
				PendingCount:        stats.PendingCount,
				DeadLetterCount:     stats.DeadLetterCount,
				RetryingCount:       stats.RetryingCount,
				OldestPendingAgeSec: stats.OldestPendingAgeSec,
			}
		}
	}

	// Prometheus text format support
	accept := c.GetHeader("Accept")
	format := c.Query("format")
	if strings.Contains(accept, "text/plain") || format == "prometheus" {
		promOutput := fmt.Sprintf(`# HELP go_goroutines Number of active goroutines.
# TYPE go_goroutines gauge
go_goroutines %d

# HELP go_memstats_alloc_bytes Number of bytes allocated and still in use.
# TYPE go_memstats_alloc_bytes gauge
go_memstats_alloc_bytes %d

# HELP pg_pool_total_connections Total connections in pgx pool.
# TYPE pg_pool_total_connections gauge
pg_pool_total_connections %d

# HELP pg_pool_idle_connections Idle connections in pgx pool.
# TYPE pg_pool_idle_connections gauge
pg_pool_idle_connections %d

# HELP pg_pool_acquired_connections In-use connections in pgx pool.
# TYPE pg_pool_acquired_connections gauge
pg_pool_acquired_connections %d

# HELP pg_queries_total Total executed database queries.
# TYPE pg_queries_total counter
pg_queries_total %d

# HELP pg_queries_slow_total Total queries exceeding slow query threshold.
# TYPE pg_queries_slow_total counter
pg_queries_slow_total %d

# HELP pg_query_duration_p50_ms 50th percentile query latency in milliseconds.
# TYPE pg_query_duration_p50_ms gauge
pg_query_duration_p50_ms %.3f

# HELP pg_query_duration_p95_ms 95th percentile query latency in milliseconds.
# TYPE pg_query_duration_p95_ms gauge
pg_query_duration_p95_ms %.3f

# HELP pg_query_duration_p99_ms 99th percentile query latency in milliseconds.
# TYPE pg_query_duration_p99_ms gauge
pg_query_duration_p99_ms %.3f
`,
			rtMetrics.NumGoroutine,
			rtMetrics.AllocBytes,
			dbMetrics.TotalConns,
			dbMetrics.IdleConns,
			dbMetrics.AcquiredConns,
			perfSummary.TotalQueries,
			perfSummary.SlowQueries,
			perfSummary.P50DurationMs,
			perfSummary.P95DurationMs,
			perfSummary.P99DurationMs,
		)

		if outboxMetrics != nil {
			promOutput += fmt.Sprintf(`
# HELP ledger_outbox_pending_events Current number of pending ledger outbox events.
# TYPE ledger_outbox_pending_events gauge
ledger_outbox_pending_events %d

# HELP ledger_outbox_dead_letter_events Total events in permanent failure / dead letter state.
# TYPE ledger_outbox_dead_letter_events gauge
ledger_outbox_dead_letter_events %d

# HELP ledger_outbox_retrying_events Events currently retrying with attempt_count > 0.
# TYPE ledger_outbox_retrying_events gauge
ledger_outbox_retrying_events %d

# HELP ledger_outbox_oldest_age_seconds Age of the oldest pending ledger outbox event in seconds.
# TYPE ledger_outbox_oldest_age_seconds gauge
ledger_outbox_oldest_age_seconds %.3f
`,
				outboxMetrics.PendingCount,
				outboxMetrics.DeadLetterCount,
				outboxMetrics.RetryingCount,
				outboxMetrics.OldestPendingAgeSec,
			)
		}

		c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(promOutput))
		return
	}

	c.JSON(http.StatusOK, MetricsResponse{
		Status:    "ok",
		Runtime:   rtMetrics,
		Database:  dbMetrics,
		QueryPerf: perfSummary,
		Outbox:    outboxMetrics,
	})
}
