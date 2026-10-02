package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrDailyBalanceNotFound = errors.New("daily settlement balance record not found")
)

type SettlementBalancerRepo struct {
	pool *pgxpool.Pool
}

func NewSettlementBalancerRepo(pool *pgxpool.Pool) *SettlementBalancerRepo {
	return &SettlementBalancerRepo{pool: pool}
}

// UpsertDailyBalance writes or updates an EOD settlement snapshot for a property and date.
func (r *SettlementBalancerRepo) UpsertDailyBalance(ctx context.Context, bal *domain.DailySettlementBalance) error {
	if bal.ID == uuid.Nil {
		bal.ID = uuid.New()
	}
	discrepanciesJSON, err := json.Marshal(bal.Discrepancies)
	if err != nil {
		discrepanciesJSON = []byte("[]")
	}
	metadataJSON, err := json.Marshal(bal.Metadata)
	if err != nil {
		metadataJSON = []byte("{}")
	}

	query := `
		INSERT INTO daily_settlement_balances (
			id, property_id, recon_date,
			gateway_gross_paise, gateway_net_settled_paise, gateway_fees_paise,
			gateway_tax_paise, gateway_adjustment_paise, gateway_in_transit_paise,
			bank_credits_paise, bank_debits_paise, unapplied_quarantine_paise,
			ledger_bank_dr_paise, ledger_bank_cr_paise, is_balanced,
			discrepancy_paise, discrepancies, metadata, created_at, updated_at
		) VALUES (
			$1, $2, $3,
			$4, $5, $6,
			$7, $8, $9,
			$10, $11, $12,
			$13, $14, $15,
			$16, $17, $18, NOW(), NOW()
		)
		ON CONFLICT (property_id, recon_date)
		DO UPDATE SET
			gateway_gross_paise = EXCLUDED.gateway_gross_paise,
			gateway_net_settled_paise = EXCLUDED.gateway_net_settled_paise,
			gateway_fees_paise = EXCLUDED.gateway_fees_paise,
			gateway_tax_paise = EXCLUDED.gateway_tax_paise,
			gateway_adjustment_paise = EXCLUDED.gateway_adjustment_paise,
			gateway_in_transit_paise = EXCLUDED.gateway_in_transit_paise,
			bank_credits_paise = EXCLUDED.bank_credits_paise,
			bank_debits_paise = EXCLUDED.bank_debits_paise,
			unapplied_quarantine_paise = EXCLUDED.unapplied_quarantine_paise,
			ledger_bank_dr_paise = EXCLUDED.ledger_bank_dr_paise,
			ledger_bank_cr_paise = EXCLUDED.ledger_bank_cr_paise,
			is_balanced = EXCLUDED.is_balanced,
			discrepancy_paise = EXCLUDED.discrepancy_paise,
			discrepancies = EXCLUDED.discrepancies,
			metadata = EXCLUDED.metadata,
			updated_at = NOW()
		RETURNING id, created_at, updated_at
	`
	return r.pool.QueryRow(ctx, query,
		bal.ID, bal.PropertyID, bal.ReconDate,
		bal.GatewayGrossPaise, bal.GatewayNetSettledPaise, bal.GatewayFeesPaise,
		bal.GatewayTaxPaise, bal.GatewayAdjustmentPaise, bal.GatewayInTransitPaise,
		bal.BankCreditsPaise, bal.BankDebitsPaise, bal.UnappliedQuarantinePaise,
		bal.LedgerBankDrPaise, bal.LedgerBankCrPaise, bal.IsBalanced,
		bal.DiscrepancyPaise, discrepanciesJSON, metadataJSON,
	).Scan(&bal.ID, &bal.CreatedAt, &bal.UpdatedAt)
}

// GetDailyBalance retrieves a saved EOD balance record for a property and date.
func (r *SettlementBalancerRepo) GetDailyBalance(ctx context.Context, propertyID uuid.UUID, reconDate time.Time) (*domain.DailySettlementBalance, error) {
	query := `
		SELECT
			id, property_id, recon_date,
			gateway_gross_paise, gateway_net_settled_paise, gateway_fees_paise,
			gateway_tax_paise, gateway_adjustment_paise, gateway_in_transit_paise,
			bank_credits_paise, bank_debits_paise, unapplied_quarantine_paise,
			ledger_bank_dr_paise, ledger_bank_cr_paise, is_balanced,
			discrepancy_paise, discrepancies, metadata, created_at, updated_at
		FROM daily_settlement_balances
		WHERE property_id = $1 AND recon_date = $2
	`
	var bal domain.DailySettlementBalance
	var discrepanciesJSON, metadataJSON []byte

	err := r.pool.QueryRow(ctx, query, propertyID, reconDate).Scan(
		&bal.ID, &bal.PropertyID, &bal.ReconDate,
		&bal.GatewayGrossPaise, &bal.GatewayNetSettledPaise, &bal.GatewayFeesPaise,
		&bal.GatewayTaxPaise, &bal.GatewayAdjustmentPaise, &bal.GatewayInTransitPaise,
		&bal.BankCreditsPaise, &bal.BankDebitsPaise, &bal.UnappliedQuarantinePaise,
		&bal.LedgerBankDrPaise, &bal.LedgerBankCrPaise, &bal.IsBalanced,
		&bal.DiscrepancyPaise, &discrepanciesJSON, &metadataJSON,
		&bal.CreatedAt, &bal.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDailyBalanceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get daily balance: %w", err)
	}

	_ = json.Unmarshal(discrepanciesJSON, &bal.Discrepancies)
	_ = json.Unmarshal(metadataJSON, &bal.Metadata)
	return &bal, nil
}

// ListDailyBalances lists recent EOD balance snapshots for a property.
func (r *SettlementBalancerRepo) ListDailyBalances(ctx context.Context, propertyID uuid.UUID, limit, offset int) ([]*domain.DailySettlementBalance, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}

	query := `
		SELECT
			id, property_id, recon_date,
			gateway_gross_paise, gateway_net_settled_paise, gateway_fees_paise,
			gateway_tax_paise, gateway_adjustment_paise, gateway_in_transit_paise,
			bank_credits_paise, bank_debits_paise, unapplied_quarantine_paise,
			ledger_bank_dr_paise, ledger_bank_cr_paise, is_balanced,
			discrepancy_paise, discrepancies, metadata, created_at, updated_at
		FROM daily_settlement_balances
		WHERE property_id = $1
		ORDER BY recon_date DESC
		LIMIT $2 OFFSET $3
	`
	rows, err := r.pool.Query(ctx, query, propertyID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list daily balances: %w", err)
	}
	defer rows.Close()

	var list []*domain.DailySettlementBalance
	for rows.Next() {
		var bal domain.DailySettlementBalance
		var discrepanciesJSON, metadataJSON []byte

		if err := rows.Scan(
			&bal.ID, &bal.PropertyID, &bal.ReconDate,
			&bal.GatewayGrossPaise, &bal.GatewayNetSettledPaise, &bal.GatewayFeesPaise,
			&bal.GatewayTaxPaise, &bal.GatewayAdjustmentPaise, &bal.GatewayInTransitPaise,
			&bal.BankCreditsPaise, &bal.BankDebitsPaise, &bal.UnappliedQuarantinePaise,
			&bal.LedgerBankDrPaise, &bal.LedgerBankCrPaise, &bal.IsBalanced,
			&bal.DiscrepancyPaise, &discrepanciesJSON, &metadataJSON,
			&bal.CreatedAt, &bal.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan daily balance: %w", err)
		}
		_ = json.Unmarshal(discrepanciesJSON, &bal.Discrepancies)
		_ = json.Unmarshal(metadataJSON, &bal.Metadata)
		list = append(list, &bal)
	}
	return list, nil
}

// ComputeDayAggregates computes multi-way reconciliation aggregates for a given property and day.
func (r *SettlementBalancerRepo) ComputeDayAggregates(ctx context.Context, propertyID uuid.UUID, reconDate time.Time) (*domain.DailySettlementBalance, error) {
	startOfDay := time.Date(reconDate.Year(), reconDate.Month(), reconDate.Day(), 0, 0, 0, 0, time.UTC)
	endOfDay := startOfDay.Add(24*time.Hour - time.Nanosecond)

	bal := &domain.DailySettlementBalance{
		PropertyID: propertyID,
		ReconDate:  startOfDay,
		Metadata:   make(map[string]any),
	}

	// 1. Gateway Collections on this day
	queryGWCollected := `
		SELECT COALESCE(SUM(p.amount), 0)
		FROM payments p
		JOIN dues d ON p.due_id = d.id
		WHERE d.property_id = $1
		  AND p.matched_by = 'cashfree'
		  AND p.matched_at >= $2 AND p.matched_at <= $3
	`
	_ = r.pool.QueryRow(ctx, queryGWCollected, propertyID, startOfDay, endOfDay).Scan(&bal.GatewayGrossPaise)

	// 2. Gateway Settlements on this day
	queryGWSettled := `
		SELECT
			COALESCE(SUM(gross_amount_paise), 0),
			COALESCE(SUM(net_amount_paise), 0),
			COALESCE(SUM(service_charge_paise), 0),
			COALESCE(SUM(service_tax_paise), 0),
			COALESCE(SUM(adjustment_paise), 0)
		FROM gateway_settlements
		WHERE property_id = $1
		  AND settled_on >= $2 AND settled_on <= $3
		  AND settlement_status = 'SUCCESS'
	`
	_ = r.pool.QueryRow(ctx, queryGWSettled, propertyID, startOfDay, endOfDay).Scan(
		&bal.GatewayGrossPaise,
		&bal.GatewayNetSettledPaise,
		&bal.GatewayFeesPaise,
		&bal.GatewayTaxPaise,
		&bal.GatewayAdjustmentPaise,
	)

	// 3. Cumulative In-Transit Gateway Funds up to endOfDay
	queryInTransit := `
		SELECT
			COALESCE((
				SELECT SUM(p.amount)
				FROM payments p
				JOIN dues d ON p.due_id = d.id
				WHERE d.property_id = $1
				  AND p.matched_by = 'cashfree'
				  AND p.matched_at <= $2
			), 0)
			-
			COALESCE((
				SELECT SUM(gross_amount_paise)
				FROM gateway_settlements
				WHERE property_id = $1
				  AND settled_on <= $2
				  AND settlement_status = 'SUCCESS'
			), 0)
	`
	_ = r.pool.QueryRow(ctx, queryInTransit, propertyID, endOfDay).Scan(&bal.GatewayInTransitPaise)

	// 4. Bank Cleared Transactions on this day
	queryBankTxns := `
		SELECT
			COALESCE(SUM(CASE WHEN row_type = 'credit' THEN amount_paise ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN row_type = 'debit' THEN amount_paise ELSE 0 END), 0)
		FROM bank_transactions
		WHERE property_id = $1 AND txn_date = $2
	`
	_ = r.pool.QueryRow(ctx, queryBankTxns, propertyID, startOfDay).Scan(
		&bal.BankCreditsPaise,
		&bal.BankDebitsPaise,
	)

	// 5. Cumulative Unapplied Quarantine up to startOfDay
	queryUnapplied := `
		SELECT COALESCE(SUM(amount_paise), 0)
		FROM bank_transactions
		WHERE property_id = $1
		  AND status IN ('unmatched', 'suggested_match')
		  AND txn_date <= $2
	`
	_ = r.pool.QueryRow(ctx, queryUnapplied, propertyID, startOfDay).Scan(&bal.UnappliedQuarantinePaise)

	// 6. General Ledger Bank Account Net Movement on this day
	queryLedgerBank := `
		SELECT
			COALESCE(SUM(l.debit_paise), 0),
			COALESCE(SUM(l.credit_paise), 0)
		FROM financial_journal_lines l
		JOIN financial_journal_entries e ON l.entry_id = e.id
		WHERE e.property_id = $1
		  AND l.account_code = 'bank'
		  AND e.occurred_at >= $2 AND e.occurred_at <= $3
	`
	_ = r.pool.QueryRow(ctx, queryLedgerBank, propertyID, startOfDay, endOfDay).Scan(
		&bal.LedgerBankDrPaise,
		&bal.LedgerBankCrPaise,
	)

	// Evaluate balance and anomalies
	bal.EvaluateBalance()

	return bal, nil
}
