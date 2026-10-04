package postgres

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

type queryPerfCtxKey struct{}

type queryPerfStartInfo struct {
	startTime time.Time
	sql       string
	argsCount int
}

// QueryPerfStats contains aggregated execution metrics for DB operations.
type QueryPerfStats struct {
	TotalQueries  uint64        `json:"total_queries"`
	SlowQueries   uint64        `json:"slow_queries"`
	ErrorQueries  uint64        `json:"error_queries"`
	TotalDuration time.Duration `json:"total_duration"`
	AvgDuration   time.Duration `json:"avg_duration"`
	MaxDuration   time.Duration `json:"max_duration"`
}

// QueryPerfTracer implements pgx.QueryTracer to monitor query latency,
// log slow queries exceeding the configured threshold, and collect telemetry.
type QueryPerfTracer struct {
	slowThreshold time.Duration
	logger        *slog.Logger

	totalQueries atomic.Uint64
	slowQueries  atomic.Uint64
	errorQueries atomic.Uint64
	totalTimeNs  atomic.Int64
	maxTimeNs    atomic.Int64
}

// NewQueryPerfTracer creates a new QueryPerfTracer with the given slow query threshold.
// If slowThreshold is <= 0, a default of 100ms is used.
func NewQueryPerfTracer(slowThreshold time.Duration, logger *slog.Logger) *QueryPerfTracer {
	if slowThreshold <= 0 {
		slowThreshold = 100 * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &QueryPerfTracer{
		slowThreshold: slowThreshold,
		logger:        logger,
	}
}

// TraceQueryStart is called at the beginning of Query, QueryRow, and Exec calls.
func (t *QueryPerfTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	info := queryPerfStartInfo{
		startTime: time.Now(),
		sql:       data.SQL,
		argsCount: len(data.Args),
	}
	return context.WithValue(ctx, queryPerfCtxKey{}, info)
}

// TraceQueryEnd is called at the completion of Query, QueryRow, and Exec calls.
func (t *QueryPerfTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	val := ctx.Value(queryPerfCtxKey{})
	if val == nil {
		return
	}
	info, ok := val.(queryPerfStartInfo)
	if !ok {
		return
	}

	duration := time.Since(info.startTime)
	durationNs := duration.Nanoseconds()

	t.totalQueries.Add(1)
	t.totalTimeNs.Add(durationNs)

	// Update max duration atomically
	for {
		curMax := t.maxTimeNs.Load()
		if durationNs <= curMax {
			break
		}
		if t.maxTimeNs.CompareAndSwap(curMax, durationNs) {
			break
		}
	}

	if data.Err != nil && data.Err != pgx.ErrNoRows {
		t.errorQueries.Add(1)
	}

	if duration >= t.slowThreshold {
		t.slowQueries.Add(1)
		rows := int64(-1)
		if data.CommandTag.RowsAffected() >= 0 {
			rows = data.CommandTag.RowsAffected()
		}

		// Truncate query in logs if excessively long
		querySnippet := info.sql
		if len(querySnippet) > 300 {
			querySnippet = querySnippet[:300] + "..."
		}

		t.logger.Warn("slow database query detected",
			slog.Int64("duration_ms", duration.Milliseconds()),
			slog.Int64("threshold_ms", t.slowThreshold.Milliseconds()),
			slog.String("sql", querySnippet),
			slog.Int("args_count", info.argsCount),
			slog.Int64("rows_affected", rows),
			slog.Any("err", data.Err),
		)
	}
}

// Stats returns a snapshot of current query performance metrics.
func (t *QueryPerfTracer) Stats() QueryPerfStats {
	total := t.totalQueries.Load()
	slow := t.slowQueries.Load()
	errs := t.errorQueries.Load()
	totalNs := t.totalTimeNs.Load()
	maxNs := t.maxTimeNs.Load()

	var avg time.Duration
	if total > 0 {
		avg = time.Duration(totalNs / int64(total))
	}

	return QueryPerfStats{
		TotalQueries:  total,
		SlowQueries:   slow,
		ErrorQueries:  errs,
		TotalDuration: time.Duration(totalNs),
		AvgDuration:   avg,
		MaxDuration:   time.Duration(maxNs),
	}
}

// SlowThreshold returns the configured slow query threshold.
func (t *QueryPerfTracer) SlowThreshold() time.Duration {
	return t.slowThreshold
}
