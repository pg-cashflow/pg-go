package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrBankAccountNotFound = errors.New("bank account not found")
)

type BankAccountRepo struct {
	pool *pgxpool.Pool
}

func NewBankAccountRepo(pool *pgxpool.Pool) *BankAccountRepo {
	return &BankAccountRepo{pool: pool}
}

// Create inserts a new bank account profile for a property.
func (r *BankAccountRepo) Create(ctx context.Context, acct *domain.BankAccount) error {
	if acct.ID == uuid.Nil {
		acct.ID = uuid.New()
	}
	if acct.AccountType == "" {
		acct.AccountType = domain.BankAccountTypeSavings
	}
	if acct.StatementProfile == "" {
		acct.StatementProfile = "generic"
	}
	acct.IsActive = true

	query := `
		INSERT INTO bank_accounts (
			id, property_id, bank_name, account_type, account_number_last4,
			label, statement_profile, is_active, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, NOW(), NOW()
		)
		RETURNING created_at, updated_at;
	`
	err := r.pool.QueryRow(ctx, query,
		acct.ID, acct.PropertyID, acct.BankName, string(acct.AccountType), acct.AccountNumberLast4,
		acct.Label, acct.StatementProfile, acct.IsActive,
	).Scan(&acct.CreatedAt, &acct.UpdatedAt)
	if err != nil {
		return fmt.Errorf("postgres: create bank_account: %w", err)
	}
	return nil
}

// GetByID retrieves a bank account by ID.
func (r *BankAccountRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.BankAccount, error) {
	query := `
		SELECT
			id, property_id, bank_name, account_type, account_number_last4,
			label, statement_profile, is_active, created_at, updated_at
		FROM bank_accounts
		WHERE id = $1
	`
	var acct domain.BankAccount
	var acctType string
	err := r.pool.QueryRow(ctx, query, id).Scan(
		&acct.ID, &acct.PropertyID, &acct.BankName, &acctType, &acct.AccountNumberLast4,
		&acct.Label, &acct.StatementProfile, &acct.IsActive, &acct.CreatedAt, &acct.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBankAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get bank_account: %w", err)
	}
	acct.AccountType = domain.BankAccountType(acctType)
	return &acct, nil
}

// ListByProperty lists all active bank accounts for a property.
func (r *BankAccountRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]*domain.BankAccount, error) {
	query := `
		SELECT
			id, property_id, bank_name, account_type, account_number_last4,
			label, statement_profile, is_active, created_at, updated_at
		FROM bank_accounts
		WHERE property_id = $1 AND is_active = TRUE
		ORDER BY created_at ASC
	`
	rows, err := r.pool.Query(ctx, query, propertyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list bank_accounts: %w", err)
	}
	defer rows.Close()

	var out []*domain.BankAccount
	for rows.Next() {
		var acct domain.BankAccount
		var acctType string
		if err := rows.Scan(
			&acct.ID, &acct.PropertyID, &acct.BankName, &acctType, &acct.AccountNumberLast4,
			&acct.Label, &acct.StatementProfile, &acct.IsActive, &acct.CreatedAt, &acct.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("postgres: scan bank_account: %w", err)
		}
		acct.AccountType = domain.BankAccountType(acctType)
		out = append(out, &acct)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: scan bank_accounts rows: %w", err)
	}
	return out, nil
}

// Deactivate soft-deletes a bank account profile.
func (r *BankAccountRepo) Deactivate(ctx context.Context, id uuid.UUID) error {
	query := `
		UPDATE bank_accounts
		SET is_active = FALSE, updated_at = NOW()
		WHERE id = $1
	`
	ct, err := r.pool.Exec(ctx, query, id)
	if err != nil {
		return fmt.Errorf("postgres: deactivate bank_account: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrBankAccountNotFound
	}
	return nil
}
