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
	ErrDepositAlreadySettled = errors.New("deposit is already settled")
	ErrInvalidRefundAmount   = errors.New("refunded paise and deductions exceed original deposit")
	ErrDueNotPaidDeposit     = errors.New("due is not a paid deposit")
)

type DepositSettlementRepo struct {
	pool *pgxpool.Pool
}

func NewDepositSettlementRepo(pool *pgxpool.Pool) *DepositSettlementRepo {
	return &DepositSettlementRepo{pool: pool}
}

type SettleDepositParams struct {
	PropertyID      uuid.UUID
	TenantID        uuid.UUID
	DepositDueID    uuid.UUID
	IdempotencyKey  string
	RefundedPaise   int64
	DeductionsPaise int64
	Reason          string
	SettledAt       time.Time
}

const depositSettlementCols = `id, property_id, tenant_id, deposit_due_id, idempotency_key, original_deposit_paise, refunded_paise, deductions_paise, status, reason, settled_at, created_at, updated_at`

func scanDepositSettlement(row pgx.Row) (*domain.DepositSettlement, error) {
	var s domain.DepositSettlement
	err := row.Scan(
		&s.ID, &s.PropertyID, &s.TenantID, &s.DepositDueID, &s.IdempotencyKey,
		&s.OriginalDepositPaise, &s.RefundedPaise, &s.DeductionsPaise,
		&s.Status, &s.Reason, &s.SettledAt, &s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *DepositSettlementRepo) SettleDepositAtomic(ctx context.Context, p SettleDepositParams) (*domain.DepositSettlement, error) {
	if p.RefundedPaise < 0 || p.DeductionsPaise < 0 {
		return nil, errors.New("refunded paise and deductions must be non-negative")
	}
	if p.SettledAt.IsZero() {
		p.SettledAt = time.Now().UTC()
	}

	var out *domain.DepositSettlement
	err := WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		// 1. Transaction-scoped advisory lock for the tenant's deposit settlements
		lockKey := fmt.Sprintf("deposit-settlement:%s", p.TenantID)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
			return fmt.Errorf("acquire advisory lock: %w", err)
		}

		// 2. Idempotency Check: if exact idempotency key exists, return it (replay safe)
		existing, err := scanDepositSettlement(tx.QueryRow(ctx, `
			SELECT `+depositSettlementCols+`
			FROM deposit_settlements
			WHERE idempotency_key = $1`, p.IdempotencyKey,
		))
		if err == nil && existing != nil {
			out = existing
			return nil
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check idempotency: %w", err)
		}

		// 3. Lock the deposit due row FOR UPDATE
		var dueTenantID, duePropertyID uuid.UUID
		var dueKind string
		var dueStatus string
		var dueOriginalAmount int64
		err = tx.QueryRow(ctx, `
			SELECT tenant_id, property_id, kind, status, original_amount
			FROM dues
			WHERE id = $1 FOR UPDATE`, p.DepositDueID,
		).Scan(&dueTenantID, &duePropertyID, &dueKind, &dueStatus, &dueOriginalAmount)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return fmt.Errorf("lock due: %w", err)
		}

		// Enforce property and tenant ownership boundary in SQL
		if duePropertyID != p.PropertyID || dueTenantID != p.TenantID {
			return domain.ErrForbidden
		}
		if dueKind != string(domain.DueKindDeposit) {
			return ErrDueNotPaidDeposit
		}
		if dueStatus != string(domain.DueStatusPaid) {
			return fmt.Errorf("%w: status is %s", ErrDueNotPaidDeposit, dueStatus)
		}

		// 4. Ensure due has not already been settled under a different idempotency key
		var existingSettlementID uuid.UUID
		err = tx.QueryRow(ctx, `
			SELECT id FROM deposit_settlements
			WHERE deposit_due_id = $1 AND status IN ('settling', 'settled') LIMIT 1`, p.DepositDueID,
		).Scan(&existingSettlementID)
		if err == nil {
			return ErrDepositAlreadySettled
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check prior settlement: %w", err)
		}

		// 5. Invariant: 0 <= refunded + deductions <= original_deposit
		if p.RefundedPaise+p.DeductionsPaise > dueOriginalAmount {
			return fmt.Errorf("%w: requested %d > deposit %d", ErrInvalidRefundAmount, p.RefundedPaise+p.DeductionsPaise, dueOriginalAmount)
		}

		// 6. Insert durable deposit_settlement row
		settlementID := uuid.New()
		settlement := &domain.DepositSettlement{
			ID:                   settlementID,
			PropertyID:           p.PropertyID,
			TenantID:             p.TenantID,
			DepositDueID:         p.DepositDueID,
			IdempotencyKey:       p.IdempotencyKey,
			OriginalDepositPaise: dueOriginalAmount,
			RefundedPaise:        p.RefundedPaise,
			DeductionsPaise:      p.DeductionsPaise,
			Status:               domain.DepositSettlementSettled,
			Reason:               p.Reason,
			SettledAt:            p.SettledAt,
			CreatedAt:            time.Now().UTC(),
			UpdatedAt:            time.Now().UTC(),
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO deposit_settlements (
				id, property_id, tenant_id, deposit_due_id, idempotency_key,
				original_deposit_paise, refunded_paise, deductions_paise,
				status, reason, settled_at, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			settlement.ID, settlement.PropertyID, settlement.TenantID, settlement.DepositDueID, settlement.IdempotencyKey,
			settlement.OriginalDepositPaise, settlement.RefundedPaise, settlement.DeductionsPaise,
			settlement.Status, settlement.Reason, settlement.SettledAt, settlement.CreatedAt, settlement.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert deposit settlement: %w", err)
		}

		// 7. Atomic in-transaction ledger outbox enqueue
		mirrorPayload := domain.DepositSettlementMirrorPayload{
			PropertyID:           p.PropertyID,
			SettlementID:         settlementID,
			TenantID:             p.TenantID,
			DepositDueID:         p.DepositDueID,
			OriginalDepositPaise: dueOriginalAmount,
			RefundedPaise:        p.RefundedPaise,
			DeductionsPaise:      p.DeductionsPaise,
			OccurredAt:           p.SettledAt,
		}
		rawPayload, err := json.Marshal(mirrorPayload)
		if err != nil {
			return fmt.Errorf("marshal outbox payload: %w", err)
		}

		outboxKey := fmt.Sprintf("deposit_settlement:%s", settlementID)
		_, err = tx.Exec(ctx, `
			INSERT INTO ledger_outbox_events (event_type, property_id, source_id, payload, idempotency_key, created_at)
			VALUES ('deposit_settlement_mirror', $1, $2, $3, $4, NOW())
			ON CONFLICT (idempotency_key) DO NOTHING`,
			p.PropertyID, settlementID, rawPayload, outboxKey,
		)
		if err != nil {
			return fmt.Errorf("enqueue outbox event: %w", err)
		}

		out = settlement
		return nil
	})

	return out, err
}
