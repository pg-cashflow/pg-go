package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrSettlementNotFound = errors.New("settlement record not found")
)

type SettlementRepo struct {
	pool *pgxpool.Pool
}

func NewSettlementRepo(pool *pgxpool.Pool) *SettlementRepo {
	return &SettlementRepo{pool: pool}
}

// UpsertSettlement inserts or updates a gateway settlement record based on the unique index.
func (r *SettlementRepo) UpsertSettlement(ctx context.Context, s *domain.GatewaySettlement) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.Currency == "" {
		s.Currency = "INR"
	}
	if len(s.RawPayload) == 0 {
		s.RawPayload = json.RawMessage("{}")
	}

	if s.PropertyID == nil {
		if s.OrderID != nil && *s.OrderID != "" {
			var pid uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT property_id FROM payments WHERE order_id = $1 LIMIT 1`, *s.OrderID).Scan(&pid); err == nil {
				s.PropertyID = &pid
			}
		}
		if s.PropertyID == nil && s.CFSettlementID != "" {
			var pid uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT property_id FROM gateway_settlements WHERE cf_settlement_id = $1 AND property_id IS NOT NULL LIMIT 1`, s.CFSettlementID).Scan(&pid); err == nil {
				s.PropertyID = &pid
			}
		}
		if s.PropertyID == nil {
			var pid uuid.UUID
			var totalProps int
			if err := r.pool.QueryRow(ctx, `SELECT id, (SELECT COUNT(*) FROM properties) FROM properties ORDER BY created_at ASC LIMIT 1`).Scan(&pid, &totalProps); err == nil && totalProps == 1 {
				s.PropertyID = &pid
			}
		}
	}

	query := `
		INSERT INTO gateway_settlements (
			id, property_id, cf_settlement_id, order_id, cf_payment_id,
			payment_intent_id, payment_id, ingestion_source, utr, currency,
			gross_amount_paise, service_charge_paise, service_tax_paise,
			adjustment_paise, net_amount_paise, settlement_status,
			settled_on, settlement_initiated_on, transfer_time,
			reconciliation_status, discrepancy_reason, journal_entry_id,
			resolution_notes, resolved_by, resolved_at, raw_payload,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10,
			$11, $12, $13,
			$14, $15, $16,
			$17, $18, $19,
			$20, $21, $22,
			$23, $24, $25, $26,
			NOW(), NOW()
		)
		ON CONFLICT (cf_settlement_id, COALESCE(order_id, ''), COALESCE(cf_payment_id, ''))
		DO UPDATE SET
			property_id = COALESCE(EXCLUDED.property_id, gateway_settlements.property_id),
			payment_intent_id = COALESCE(EXCLUDED.payment_intent_id, gateway_settlements.payment_intent_id),
			payment_id = COALESCE(EXCLUDED.payment_id, gateway_settlements.payment_id),
			ingestion_source = EXCLUDED.ingestion_source,
			utr = CASE WHEN EXCLUDED.utr != '' THEN EXCLUDED.utr ELSE gateway_settlements.utr END,
			gross_amount_paise = EXCLUDED.gross_amount_paise,
			service_charge_paise = EXCLUDED.service_charge_paise,
			service_tax_paise = EXCLUDED.service_tax_paise,
			adjustment_paise = EXCLUDED.adjustment_paise,
			net_amount_paise = EXCLUDED.net_amount_paise,
			settlement_status = CASE
				-- Terminal reversal cannot be overwritten by any state
				WHEN gateway_settlements.settlement_status = 'REVERSED' THEN gateway_settlements.settlement_status
				-- Legitimate reversal wins over everything else (e.g. chargeback after SUCCESS)
				WHEN EXCLUDED.settlement_status = 'REVERSED' THEN 'REVERSED'
				-- Terminal SUCCESS cannot regress to PENDING or FAILED
				WHEN gateway_settlements.settlement_status = 'SUCCESS' AND EXCLUDED.settlement_status IN ('PENDING', 'FAILED') THEN gateway_settlements.settlement_status
				-- Terminal FAILED cannot regress to PENDING
				WHEN gateway_settlements.settlement_status = 'FAILED' AND EXCLUDED.settlement_status = 'PENDING' THEN gateway_settlements.settlement_status
				ELSE EXCLUDED.settlement_status
			END,
			settled_on = COALESCE(EXCLUDED.settled_on, gateway_settlements.settled_on),
			settlement_initiated_on = COALESCE(EXCLUDED.settlement_initiated_on, gateway_settlements.settlement_initiated_on),
			transfer_time = COALESCE(EXCLUDED.transfer_time, gateway_settlements.transfer_time),
			reconciliation_status = CASE
				WHEN gateway_settlements.reconciliation_status = 'manually_reconciled' THEN gateway_settlements.reconciliation_status
				ELSE EXCLUDED.reconciliation_status
			END,
			discrepancy_reason = CASE
				WHEN gateway_settlements.reconciliation_status = 'manually_reconciled' THEN gateway_settlements.discrepancy_reason
				ELSE EXCLUDED.discrepancy_reason
			END,
			resolution_notes = COALESCE(EXCLUDED.resolution_notes, gateway_settlements.resolution_notes),
			resolved_by = COALESCE(EXCLUDED.resolved_by, gateway_settlements.resolved_by),
			resolved_at = COALESCE(EXCLUDED.resolved_at, gateway_settlements.resolved_at),
			journal_entry_id = COALESCE(EXCLUDED.journal_entry_id, gateway_settlements.journal_entry_id),
			raw_payload = EXCLUDED.raw_payload,
			updated_at = NOW()
		RETURNING id, created_at, updated_at;
	`

	return r.pool.QueryRow(ctx, query,
		s.ID, s.PropertyID, s.CFSettlementID, s.OrderID, s.CFPaymentID,
		s.PaymentIntentID, s.PaymentID, s.IngestionSource, s.UTR, s.Currency,
		s.GrossAmountPaise, s.ServiceChargePaise, s.ServiceTaxPaise,
		s.AdjustmentPaise, s.NetAmountPaise, s.SettlementStatus,
		s.SettledOn, s.SettlementInitiatedOn, s.TransferTime,
		s.ReconciliationStatus, s.DiscrepancyReason, s.JournalEntryID,
		s.ResolutionNotes, s.ResolvedBy, s.ResolvedAt, s.RawPayload,
	).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt)
}

// GetSettlement retrieves a settlement record by its internal UUID.
func (r *SettlementRepo) GetSettlement(ctx context.Context, id uuid.UUID) (*domain.GatewaySettlement, error) {
	query := `
		SELECT
			id, property_id, cf_settlement_id, order_id, cf_payment_id,
			payment_intent_id, payment_id, ingestion_source, utr, currency,
			gross_amount_paise, service_charge_paise, service_tax_paise,
			adjustment_paise, net_amount_paise, settlement_status,
			settled_on, settlement_initiated_on, transfer_time,
			reconciliation_status, discrepancy_reason, journal_entry_id,
			resolution_notes, resolved_by, resolved_at, raw_payload,
			created_at, updated_at
		FROM gateway_settlements
		WHERE id = $1
	`
	row := r.pool.QueryRow(ctx, query, id)
	return scanSettlement(row)
}

// GetSettlementByCFID retrieves a settlement record by Cashfree identifiers.
func (r *SettlementRepo) GetSettlementByCFID(ctx context.Context, cfSettlementID string, orderID string, cfPaymentID string) (*domain.GatewaySettlement, error) {
	query := `
		SELECT
			id, property_id, cf_settlement_id, order_id, cf_payment_id,
			payment_intent_id, payment_id, ingestion_source, utr, currency,
			gross_amount_paise, service_charge_paise, service_tax_paise,
			adjustment_paise, net_amount_paise, settlement_status,
			settled_on, settlement_initiated_on, transfer_time,
			reconciliation_status, discrepancy_reason, journal_entry_id,
			resolution_notes, resolved_by, resolved_at, raw_payload,
			created_at, updated_at
		FROM gateway_settlements
		WHERE cf_settlement_id = $1
		  AND COALESCE(order_id, '') = COALESCE($2, '')
		  AND COALESCE(cf_payment_id, '') = COALESCE($3, '')
	`
	row := r.pool.QueryRow(ctx, query, cfSettlementID, orderID, cfPaymentID)
	return scanSettlement(row)
}

// ListSettlements returns a paginated list of settlements filtered by property and status.
func (r *SettlementRepo) ListSettlements(ctx context.Context, propertyID *uuid.UUID, filter domain.SettlementFilter) ([]*domain.GatewaySettlement, int, error) {
	whereClauses := []string{"1=1"}
	args := []any{}
	argIdx := 1

	if propertyID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("(property_id = $%d OR property_id IS NULL)", argIdx))
		args = append(args, *propertyID)
		argIdx++
	}

	if filter.Status != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("reconciliation_status = $%d", argIdx))
		args = append(args, string(*filter.Status))
		argIdx++
	}

	if filter.FromDate != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("created_at >= $%d", argIdx))
		args = append(args, *filter.FromDate)
		argIdx++
	}

	if filter.ToDate != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("created_at <= $%d", argIdx))
		args = append(args, *filter.ToDate)
		argIdx++
	}

	whereSQL := ""
	for i, c := range whereClauses {
		if i == 0 {
			whereSQL = "WHERE " + c
		} else {
			whereSQL += " AND " + c
		}
	}

	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM gateway_settlements %s", whereSQL)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count settlements: %w", err)
	}

	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	listQuery := fmt.Sprintf(`
		SELECT
			id, property_id, cf_settlement_id, order_id, cf_payment_id,
			payment_intent_id, payment_id, ingestion_source, utr, currency,
			gross_amount_paise, service_charge_paise, service_tax_paise,
			adjustment_paise, net_amount_paise, settlement_status,
			settled_on, settlement_initiated_on, transfer_time,
			reconciliation_status, discrepancy_reason, journal_entry_id,
			resolution_notes, resolved_by, resolved_at, raw_payload,
			created_at, updated_at
		FROM gateway_settlements
		%s
		ORDER BY created_at DESC
		LIMIT $%d OFFSET $%d
	`, whereSQL, argIdx, argIdx+1)

	args = append(args, limit, offset)

	rows, err := r.pool.Query(ctx, listQuery, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list settlements: %w", err)
	}
	defer rows.Close()

	var list []*domain.GatewaySettlement
	for rows.Next() {
		s, err := scanSettlement(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan settlement item: %w", err)
		}
		list = append(list, s)
	}
	return list, total, nil
}

// ResolveDiscrepancy updates a settlement record to manually_reconciled with operator notes.
func (r *SettlementRepo) ResolveDiscrepancy(ctx context.Context, id uuid.UUID, resolvedBy uuid.UUID, notes string) error {
	query := `
		UPDATE gateway_settlements
		SET reconciliation_status = 'manually_reconciled',
		    resolution_notes = $1,
		    resolved_by = $2,
		    resolved_at = NOW(),
		    updated_at = NOW()
		WHERE id = $3
	`
	cmd, err := r.pool.Exec(ctx, query, notes, resolvedBy, id)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrSettlementNotFound
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSettlement(s rowScanner) (*domain.GatewaySettlement, error) {
	var item domain.GatewaySettlement
	var rawPayload []byte
	var statusStr string
	var sourceStr string

	err := s.Scan(
		&item.ID, &item.PropertyID, &item.CFSettlementID, &item.OrderID, &item.CFPaymentID,
		&item.PaymentIntentID, &item.PaymentID, &sourceStr, &item.UTR, &item.Currency,
		&item.GrossAmountPaise, &item.ServiceChargePaise, &item.ServiceTaxPaise,
		&item.AdjustmentPaise, &item.NetAmountPaise, &item.SettlementStatus,
		&item.SettledOn, &item.SettlementInitiatedOn, &item.TransferTime,
		&statusStr, &item.DiscrepancyReason, &item.JournalEntryID,
		&item.ResolutionNotes, &item.ResolvedBy, &item.ResolvedAt, &rawPayload,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSettlementNotFound
	}
	if err != nil {
		return nil, err
	}

	item.IngestionSource = domain.IngestionSource(sourceStr)
	item.ReconciliationStatus = domain.SettlementReconStatus(statusStr)
	item.RawPayload = json.RawMessage(rawPayload)
	return &item, nil
}
