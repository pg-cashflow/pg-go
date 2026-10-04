package postgres

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestQueryPerfTracer_FastQuery(t *testing.T) {
	threshold := 50 * time.Millisecond
	tracer := NewQueryPerfTracer(threshold, slog.Default())

	ctx := context.Background()
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL:  "SELECT 1",
		Args: []any{123},
	})

	time.Sleep(5 * time.Millisecond) // below threshold

	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		CommandTag: pgconn.NewCommandTag("SELECT 1"),
		Err:        nil,
	})

	stats := tracer.Stats()
	if stats.TotalQueries != 1 {
		t.Fatalf("expected 1 total query, got %d", stats.TotalQueries)
	}
	if stats.SlowQueries != 0 {
		t.Fatalf("expected 0 slow queries, got %d", stats.SlowQueries)
	}
	if stats.ErrorQueries != 0 {
		t.Fatalf("expected 0 error queries, got %d", stats.ErrorQueries)
	}
	if stats.TotalDuration <= 0 {
		t.Fatalf("expected positive total duration, got %v", stats.TotalDuration)
	}
}

func TestQueryPerfTracer_SlowQueryAndError(t *testing.T) {
	threshold := 10 * time.Millisecond
	tracer := NewQueryPerfTracer(threshold, slog.Default())

	ctx := context.Background()
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL:  "SELECT pg_sleep(0.02)",
		Args: nil,
	})

	time.Sleep(15 * time.Millisecond) // exceeds threshold

	testErr := errors.New("timeout error")
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		CommandTag: pgconn.CommandTag{},
		Err:        testErr,
	})

	stats := tracer.Stats()
	if stats.TotalQueries != 1 {
		t.Fatalf("expected 1 total query, got %d", stats.TotalQueries)
	}
	if stats.SlowQueries != 1 {
		t.Fatalf("expected 1 slow query, got %d", stats.SlowQueries)
	}
	if stats.ErrorQueries != 1 {
		t.Fatalf("expected 1 error query, got %d", stats.ErrorQueries)
	}
	if stats.MaxDuration < threshold {
		t.Fatalf("expected max duration >= threshold, got %v", stats.MaxDuration)
	}
}

func TestQueryPerfTracer_IgnoreErrNoRows(t *testing.T) {
	tracer := NewQueryPerfTracer(100*time.Millisecond, slog.Default())

	ctx := context.Background()
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL: "SELECT id FROM users WHERE id = $1",
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		Err: pgx.ErrNoRows,
	})

	stats := tracer.Stats()
	if stats.ErrorQueries != 0 {
		t.Fatalf("expected ErrNoRows not counted as error query, got %d", stats.ErrorQueries)
	}
}
