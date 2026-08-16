package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReconciliationSummary is the Tier-1 collections digest (Rev 6).
type ReconciliationSummary struct {
	Period             string           `json:"period"` // "2026-08" or "2026"
	RentCollected      int64            `json:"rent_collected_paise"`
	CollectedByChannel map[string]int64 `json:"by_channel"`              // due_code | amount_date_window | cash | manual
	OutstandingRent    int64            `json:"outstanding_paise"`       // pending + partial rent dues
	CreditsHeld        int64            `json:"credits_held_paise"`      // Σ tenant.credit_balance_paise
	DepositsHeld       int64            `json:"deposits_held_paise"`     // paid, unsettled deposits
	DepositsRefunded   int64            `json:"deposits_refunded_paise"` // this period
}

// RowQuerier is satisfied by *pgxpool.Pool for the reconciliation SQL.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SQLSummaryRepository runs one SUM/GROUP BY style aggregate query.
type SQLSummaryRepository struct {
	DB RowQuerier
}

func NewSQLSummaryRepository(db RowQuerier) *SQLSummaryRepository {
	return &SQLSummaryRepository{DB: db}
}

func (r *SQLSummaryRepository) QueryReconciliation(ctx context.Context, propertyID uuid.UUID, from, to time.Time) (*ReconciliationSummary, error) {
	var (
		rentCollected    int64
		byChannelJSON    []byte
		outstanding      int64
		creditsHeld      int64
		depositsHeld     int64
		depositsRefunded int64
	)
	// One round-trip: scalar aggregates + channel GROUP BY via json_object_agg.
	err := r.DB.QueryRow(ctx, `
		SELECT
			COALESCE((
				SELECT SUM(p.amount)::bigint
				FROM payments p
				JOIN dues d ON d.id = p.due_id
				WHERE d.property_id = $1 AND d.kind = 'rent'
				  AND p.matched_at >= $2 AND p.matched_at < $3
			), 0),
			COALESCE((
				SELECT json_object_agg(matched_by, total)
				FROM (
					SELECT p.matched_by::text AS matched_by, SUM(p.amount)::bigint AS total
					FROM payments p
					JOIN dues d ON d.id = p.due_id
					WHERE d.property_id = $1 AND d.kind = 'rent'
					  AND p.matched_at >= $2 AND p.matched_at < $3
					GROUP BY p.matched_by
				) s
			), '{}'::json),
			COALESCE((
				SELECT SUM(amount)::bigint FROM dues
				WHERE property_id = $1 AND kind = 'rent' AND status IN ('pending', 'partial')
			), 0),
			COALESCE((
				SELECT SUM(credit_balance_paise)::bigint FROM tenants WHERE property_id = $1
			), 0),
			COALESCE((
				SELECT SUM(d.original_amount)::bigint FROM dues d
				WHERE d.property_id = $1 AND d.kind = 'deposit' AND d.status = 'paid'
				  AND NOT EXISTS (
					SELECT 1 FROM events e
					WHERE e.due_id = d.id AND e.event_type = 'DepositSettled'
				  )
			), 0),
			COALESCE((
				SELECT SUM((payload->>'refunded_amount_paise')::bigint)
				FROM events
				WHERE property_id = $1 AND event_type = 'DepositSettled'
				  AND occurred_at >= $2 AND occurred_at < $3
			), 0)
	`, propertyID, from, to).Scan(
		&rentCollected, &byChannelJSON, &outstanding, &creditsHeld, &depositsHeld, &depositsRefunded,
	)
	if err != nil {
		return nil, err
	}
	channels := map[string]int64{}
	if len(byChannelJSON) > 0 {
		_ = json.Unmarshal(byChannelJSON, &channels)
	}
	return &ReconciliationSummary{
		RentCollected:      rentCollected,
		CollectedByChannel: channels,
		OutstandingRent:    outstanding,
		CreditsHeld:        creditsHeld,
		DepositsHeld:       depositsHeld,
		DepositsRefunded:   depositsRefunded,
	}, nil
}

// BuildSummary aggregates collections for propertyID over period "YYYY-MM" or "YYYY".
func (s *Service) BuildSummary(ctx context.Context, propertyID uuid.UUID, period string) (*ReconciliationSummary, error) {
	if s.summaries == nil {
		return nil, fmt.Errorf("payment: summary repository not configured")
	}
	from, to, err := parsePeriodBounds(period)
	if err != nil {
		return nil, err
	}
	sum, err := s.summaries.QueryReconciliation(ctx, propertyID, from, to)
	if err != nil {
		return nil, err
	}
	sum.Period = period
	if sum.CollectedByChannel == nil {
		sum.CollectedByChannel = map[string]int64{}
	}
	return sum, nil
}

func parsePeriodBounds(period string) (from, to time.Time, err error) {
	loc, locErr := time.LoadLocation("Asia/Kolkata")
	if locErr != nil {
		loc = time.FixedZone("IST", 5*3600+30*60)
	}

	var y int
	switch len(period) {
	case 4: // YYYY
		if _, err = fmt.Sscanf(period, "%d", &y); err != nil || y < 2000 {
			return time.Time{}, time.Time{}, fmt.Errorf("payment: invalid period %q", period)
		}
		fromLocal := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
		toLocal := time.Date(y+1, 1, 1, 0, 0, 0, 0, loc)
		return fromLocal.UTC(), toLocal.UTC(), nil
	case 7: // YYYY-MM
		var mi int
		if _, err = fmt.Sscanf(period, "%d-%d", &y, &mi); err != nil || y < 2000 || mi < 1 || mi > 12 {
			return time.Time{}, time.Time{}, fmt.Errorf("payment: invalid period %q", period)
		}
		fromLocal := time.Date(y, time.Month(mi), 1, 0, 0, 0, 0, loc)
		toLocal := fromLocal.AddDate(0, 1, 0)
		return fromLocal.UTC(), toLocal.UTC(), nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("payment: invalid period %q (want YYYY or YYYY-MM)", period)
	}
}
