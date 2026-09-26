package finance

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is satisfied by pgx.Tx, *pgxpool.Pool, or *pgxpool.Conn.
type DBTX interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// DepartureDesyncReport represents a departure that is settled operationally but has missing financial mirror journals.
type DepartureDesyncReport struct {
	DepartureID uuid.UUID `json:"departure_id"`
	PropertyID  uuid.UUID `json:"property_id"`
	Status      string    `json:"status"`
	SettledAt   time.Time `json:"settled_at"`
	AgeHours    float64   `json:"age_hours"`
}

// ReconcileDepartureSettlements identifies departures marked approved/refunded that have no matching
// journal entries in journal_lines older than the given ageThreshold (e.g. 1 hour).
// It flags anomalies loudly for operator investigation without silently mutating financial state.
func ReconcileDepartureSettlements(ctx context.Context, db DBTX, ageThreshold time.Duration) ([]DepartureDesyncReport, error) {
	if ageThreshold <= 0 {
		ageThreshold = 1 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-ageThreshold)

	rows, err := db.Query(ctx, `
		SELECT d.id, d.property_id, d.status, d.updated_at
		FROM tenant_departures d
		WHERE d.status IN ('approved', 'refunded')
		  AND d.updated_at <= $1
		  AND NOT EXISTS (
		      SELECT 1 FROM financial_journal_entries fje
		      WHERE fje.source_type = 'departure_settlement' AND fje.source_id = d.id
		  )
		ORDER BY d.updated_at ASC`, cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("query departure ledger reconciliation: %w", err)
	}
	defer rows.Close()

	var reports []DepartureDesyncReport
	now := time.Now().UTC()
	for rows.Next() {
		var r DepartureDesyncReport
		if err := rows.Scan(&r.DepartureID, &r.PropertyID, &r.Status, &r.SettledAt); err != nil {
			return nil, err
		}
		r.AgeHours = now.Sub(r.SettledAt).Hours()
		reports = append(reports, r)

		slog.Error("CRITICAL RECONCILIATION ANOMALY: settled departure missing double-entry financial mirror journal",
			"departure_id", r.DepartureID,
			"property_id", r.PropertyID,
			"status", r.Status,
			"settled_at", r.SettledAt,
			"age_hours", fmt.Sprintf("%.2f", r.AgeHours),
		)
	}
	return reports, rows.Err()
}
