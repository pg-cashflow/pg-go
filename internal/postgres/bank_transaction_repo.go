package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrBankTransactionNotFound = errors.New("bank transaction record not found")
)

type BankTransactionRepo struct {
	pool *pgxpool.Pool
}

func NewBankTransactionRepo(pool *pgxpool.Pool) *BankTransactionRepo {
	return &BankTransactionRepo{pool: pool}
}

// InsertTransaction inserts a new bank transaction. If a row with the same (property_id, dedup_hash)
// already exists, it is safely ignored and inserted returns false.
func (r *BankTransactionRepo) InsertTransaction(ctx context.Context, tx pgx.Tx, txn *domain.BankTransaction) (bool, error) {
	if txn.ID == uuid.Nil {
		txn.ID = uuid.New()
	}
	if txn.Status == "" {
		txn.Status = domain.BankTxnUnmatched
	}
	if txn.RowType == "" {
		txn.RowType = "credit"
	}
	if txn.OccurrenceIndex <= 0 {
		txn.OccurrenceIndex = 1
	}
	if txn.DedupHash == "" {
		txn.DedupHash = domain.ComputeBankTxnDedupHash(
			txn.PropertyID,
			txn.TxnDate,
			txn.AmountPaise,
			txn.RowType,
			txn.TxnID,
			txn.ClosingBalancePaise,
			txn.OccurrenceIndex,
		)
	}

	query := `
		INSERT INTO bank_transactions (
			id, property_id, txn_id, amount_paise, row_type, txn_date,
			narration, closing_balance_paise, occurrence_index, dedup_hash,
			status, matched_due_id, suggested_due_id, confidence_score,
			matched_at, matched_by, journal_entry_id, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14,
			$15, $16, $17, NOW(), NOW()
		)
		ON CONFLICT (property_id, dedup_hash) DO NOTHING
		RETURNING id, created_at, updated_at;
	`

	var row pgx.Row
	if tx != nil {
		row = tx.QueryRow(ctx, query,
			txn.ID, txn.PropertyID, txn.TxnID, txn.AmountPaise, txn.RowType, txn.TxnDate,
			txn.Narration, txn.ClosingBalancePaise, txn.OccurrenceIndex, txn.DedupHash,
			string(txn.Status), txn.MatchedDueID, txn.SuggestedDueID, txn.ConfidenceScore,
			txn.MatchedAt, txn.MatchedBy, txn.JournalEntryID,
		)
	} else {
		row = r.pool.QueryRow(ctx, query,
			txn.ID, txn.PropertyID, txn.TxnID, txn.AmountPaise, txn.RowType, txn.TxnDate,
			txn.Narration, txn.ClosingBalancePaise, txn.OccurrenceIndex, txn.DedupHash,
			string(txn.Status), txn.MatchedDueID, txn.SuggestedDueID, txn.ConfidenceScore,
			txn.MatchedAt, txn.MatchedBy, txn.JournalEntryID,
		)
	}

	err := row.Scan(&txn.ID, &txn.CreatedAt, &txn.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Existing duplicate detected — safely skipped
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: insert bank_transaction: %w", err)
	}
	return true, nil
}

// GetByID retrieves a bank transaction by its ID.
func (r *BankTransactionRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.BankTransaction, error) {
	query := `
		SELECT
			id, property_id, txn_id, amount_paise, row_type, txn_date,
			narration, closing_balance_paise, occurrence_index, dedup_hash,
			status, matched_due_id, suggested_due_id, confidence_score,
			matched_at, matched_by, journal_entry_id, created_at, updated_at
		FROM bank_transactions
		WHERE id = $1
	`
	row := r.pool.QueryRow(ctx, query, id)
	return scanBankTransaction(row)
}

// GetByPropertyAndID retrieves a bank transaction scoped by property ID and transaction ID.
func (r *BankTransactionRepo) GetByPropertyAndID(ctx context.Context, propertyID, id uuid.UUID) (*domain.BankTransaction, error) {
	query := `
		SELECT
			id, property_id, txn_id, amount_paise, row_type, txn_date,
			narration, closing_balance_paise, occurrence_index, dedup_hash,
			status, matched_due_id, suggested_due_id, confidence_score,
			matched_at, matched_by, journal_entry_id, created_at, updated_at
		FROM bank_transactions
		WHERE property_id = $1 AND id = $2
	`
	row := r.pool.QueryRow(ctx, query, propertyID, id)
	return scanBankTransaction(row)
}

// ListByProperty lists bank transactions for a property filtered by status and date range.
func (r *BankTransactionRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID, filter domain.BankTransactionFilter) ([]*domain.BankTransaction, int, error) {
	whereClauses := []string{"property_id = $1"}
	args := []any{propertyID}
	argIdx := 2

	if filter.Status != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, string(*filter.Status))
		argIdx++
	}
	if filter.FromDate != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("txn_date >= $%d", argIdx))
		args = append(args, *filter.FromDate)
		argIdx++
	}
	if filter.ToDate != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("txn_date <= $%d", argIdx))
		args = append(args, *filter.ToDate)
		argIdx++
	}

	whereSQL := "WHERE " + whereClauses[0]
	for _, c := range whereClauses[1:] {
		whereSQL += " AND " + c
	}

	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM bank_transactions %s", whereSQL)
	var total int
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("postgres: count bank_transactions: %w", err)
	}

	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	query := fmt.Sprintf(`
		SELECT
			id, property_id, txn_id, amount_paise, row_type, txn_date,
			narration, closing_balance_paise, occurrence_index, dedup_hash,
			status, matched_due_id, suggested_due_id, confidence_score,
			matched_at, matched_by, journal_entry_id, created_at, updated_at
		FROM bank_transactions
		%s
		ORDER BY txn_date DESC, created_at DESC
		LIMIT $%d OFFSET $%d
	`, whereSQL, argIdx, argIdx+1)

	args = append(args, limit, offset)
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("postgres: list bank_transactions: %w", err)
	}
	defer rows.Close()

	var txns []*domain.BankTransaction
	for rows.Next() {
		txn, err := scanBankTransaction(rows)
		if err != nil {
			return nil, 0, err
		}
		txns = append(txns, txn)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("postgres: scan bank_transactions rows: %w", err)
	}

	return txns, total, nil
}

// UpdateStatus updates the status and matching fields of a bank transaction.
func (r *BankTransactionRepo) UpdateStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.BankTransactionStatus, matchedDueID *uuid.UUID, matchedBy *uuid.UUID, matchedAt *time.Time, journalEntryID *uuid.UUID) error {
	query := `
		UPDATE bank_transactions
		SET status = $2,
		    matched_due_id = $3,
		    matched_by = $4,
		    matched_at = $5,
		    journal_entry_id = COALESCE($6, journal_entry_id),
		    updated_at = NOW()
		WHERE id = $1
	`
	var err error
	if tx != nil {
		_, err = tx.Exec(ctx, query, id, string(status), matchedDueID, matchedBy, matchedAt, journalEntryID)
	} else {
		_, err = r.pool.Exec(ctx, query, id, string(status), matchedDueID, matchedBy, matchedAt, journalEntryID)
	}
	if err != nil {
		return fmt.Errorf("postgres: update bank_transaction status: %w", err)
	}
	return nil
}

func scanBankTransaction(row pgx.Row) (*domain.BankTransaction, error) {
	var txn domain.BankTransaction
	var statusStr string
	err := row.Scan(
		&txn.ID, &txn.PropertyID, &txn.TxnID, &txn.AmountPaise, &txn.RowType, &txn.TxnDate,
		&txn.Narration, &txn.ClosingBalancePaise, &txn.OccurrenceIndex, &txn.DedupHash,
		&statusStr, &txn.MatchedDueID, &txn.SuggestedDueID, &txn.ConfidenceScore,
		&txn.MatchedAt, &txn.MatchedBy, &txn.JournalEntryID, &txn.CreatedAt, &txn.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBankTransactionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan bank_transaction: %w", err)
	}
	txn.Status = domain.BankTransactionStatus(statusStr)
	return &txn, nil
}
