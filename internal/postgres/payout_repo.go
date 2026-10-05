package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type DepartureMirrorer interface {
	MirrorDepartureSettlement(
		ctx context.Context,
		propertyID, departureID uuid.UUID,
		depositPaise, unusedRentReversal, damagesPaise, netRefundPaise, outstandingDuesNettedPaise, receivableBalancePaise int64,
		at time.Time,
	) error
}

type PayoutRepo struct {
	pool     *pgxpool.Pool
	mirrorer DepartureMirrorer
	outbox   *LedgerOutboxRepo
}

func NewPayoutRepo(pool *pgxpool.Pool, mirrorer ...DepartureMirrorer) *PayoutRepo {
	var m DepartureMirrorer
	if len(mirrorer) > 0 {
		m = mirrorer[0]
	}
	return &PayoutRepo{pool: pool, mirrorer: m, outbox: NewLedgerOutboxRepo(pool)}
}

func (r *PayoutRepo) SetMirrorer(m DepartureMirrorer) {
	r.mirrorer = m
}

// ---------------------------------------------------------
// Payees
// ---------------------------------------------------------

const payeeCols = `id, property_id, payee_type, name, phone, account_number_encrypted,
	account_number_last4, account_number_hash, ifsc, bank_name, upi_vpa,
	key_version, is_verified, verified_by, verified_at, created_at, updated_at`

func scanPayee(row pgx.Row) (*domain.PayoutPayee, error) {
	var p domain.PayoutPayee
	err := row.Scan(
		&p.ID, &p.PropertyID, &p.PayeeType, &p.Name, &p.Phone, &p.AccountNumberEncrypted,
		&p.AccountNumberLast4, &p.AccountNumberHash, &p.IFSC, &p.BankName, &p.UPIVPA,
		&p.KeyVersion, &p.IsVerified, &p.VerifiedBy, &p.VerifiedAt, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PayoutRepo) CreatePayee(ctx context.Context, p *domain.PayoutPayee) error {
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now
	if p.KeyVersion == 0 {
		p.KeyVersion = 1
	}
	return r.pool.QueryRow(ctx, `
		INSERT INTO payout_payees (
			property_id, payee_type, name, phone, account_number_encrypted,
			account_number_last4, account_number_hash, ifsc, bank_name, upi_vpa,
			key_version, is_verified, verified_by, verified_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING id`,
		p.PropertyID, p.PayeeType, p.Name, p.Phone, p.AccountNumberEncrypted,
		p.AccountNumberLast4, p.AccountNumberHash, p.IFSC, p.BankName, p.UPIVPA,
		p.KeyVersion, p.IsVerified, p.VerifiedBy, p.VerifiedAt, p.CreatedAt, p.UpdatedAt,
	).Scan(&p.ID)
}

func (r *PayoutRepo) GetPayeeByID(ctx context.Context, id uuid.UUID) (*domain.PayoutPayee, error) {
	return scanPayee(r.pool.QueryRow(ctx, `SELECT `+payeeCols+` FROM payout_payees WHERE id=$1`, id))
}

func (r *PayoutRepo) GetPayeeByHash(ctx context.Context, propertyID uuid.UUID, hash string, payeeType domain.PayeeType) (*domain.PayoutPayee, error) {
	return scanPayee(r.pool.QueryRow(ctx, `
		SELECT `+payeeCols+` FROM payout_payees
		WHERE property_id=$1 AND account_number_hash=$2 AND payee_type=$3`,
		propertyID, hash, payeeType,
	))
}

func (r *PayoutRepo) ListPayeesByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.PayoutPayee, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+payeeCols+` FROM payout_payees WHERE property_id=$1 ORDER BY name ASC, id ASC LIMIT 50`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PayoutPayee
	for rows.Next() {
		p, err := scanPayee(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------
// Departures & Deductions
// ---------------------------------------------------------

const departureCols = `id, tenant_id, property_id, notice_given_at, planned_vacate_date,
	actual_vacate_date, inspected_at, sla_deadline_at, deposit_amount_paise,
	unused_rent_refund_paise, prorated_rent_owed_paise, outstanding_dues_netted_paise,
	deductions_paise, net_refund_paise, receivable_balance_paise, status, notes,
	created_at, updated_at`

func scanDeparture(row pgx.Row) (*domain.TenantDeparture, error) {
	var d domain.TenantDeparture
	err := row.Scan(
		&d.ID, &d.TenantID, &d.PropertyID, &d.NoticeGivenAt, &d.PlannedVacateDate,
		&d.ActualVacateDate, &d.InspectedAt, &d.SLADeadlineAt, &d.DepositAmountPaise,
		&d.UnusedRentRefundPaise, &d.ProratedRentOwedPaise, &d.OutstandingDuesNettedPaise,
		&d.DeductionsPaise, &d.NetRefundPaise, &d.ReceivableBalancePaise, &d.Status, &d.Notes,
		&d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *PayoutRepo) CreateDeparture(ctx context.Context, d *domain.TenantDeparture) error {
	now := time.Now().UTC()
	d.CreatedAt = now
	d.UpdatedAt = now
	if d.Status == "" {
		d.Status = domain.DeparturePending
	}
	return r.pool.QueryRow(ctx, `
		INSERT INTO tenant_departures (
			tenant_id, property_id, notice_given_at, planned_vacate_date,
			actual_vacate_date, inspected_at, sla_deadline_at, deposit_amount_paise,
			unused_rent_refund_paise, prorated_rent_owed_paise, outstanding_dues_netted_paise,
			deductions_paise, net_refund_paise, receivable_balance_paise, status, notes,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		RETURNING id`,
		d.TenantID, d.PropertyID, d.NoticeGivenAt, d.PlannedVacateDate,
		d.ActualVacateDate, d.InspectedAt, d.SLADeadlineAt, d.DepositAmountPaise,
		d.UnusedRentRefundPaise, d.ProratedRentOwedPaise, d.OutstandingDuesNettedPaise,
		d.DeductionsPaise, d.NetRefundPaise, d.ReceivableBalancePaise, d.Status, d.Notes,
		d.CreatedAt, d.UpdatedAt,
	).Scan(&d.ID)
}

func (r *PayoutRepo) GetDepartureByID(ctx context.Context, id uuid.UUID) (*domain.TenantDeparture, error) {
	return scanDeparture(r.pool.QueryRow(ctx, `SELECT `+departureCols+` FROM tenant_departures WHERE id=$1`, id))
}

func (r *PayoutRepo) GetActiveDepartureByTenant(ctx context.Context, tenantID uuid.UUID) (*domain.TenantDeparture, error) {
	return scanDeparture(r.pool.QueryRow(ctx, `
		SELECT `+departureCols+` FROM tenant_departures
		WHERE tenant_id=$1 AND status IN ('pending', 'inspected', 'approved')
		ORDER BY created_at DESC LIMIT 1`, tenantID,
	))
}

func (r *PayoutRepo) UpdateDepartureInspection(ctx context.Context, id uuid.UUID, inspectedAt time.Time, notes *string) error {
	slaDeadline := inspectedAt.Add(24 * time.Hour)
	_, err := r.pool.Exec(ctx, `
		UPDATE tenant_departures
		SET inspected_at=$2, sla_deadline_at=$3, status='inspected', notes=COALESCE($4, notes), updated_at=NOW()
		WHERE id=$1 AND status='pending'`,
		id, inspectedAt, slaDeadline, notes,
	)
	return err
}

func (r *PayoutRepo) AddDeduction(ctx context.Context, ded *domain.DepartureDeduction) error {
	now := time.Now().UTC()
	ded.CreatedAt = now
	if ded.Status == "" {
		ded.Status = domain.DeductionAgreed
	}
	return r.pool.QueryRow(ctx, `
		INSERT INTO departure_deductions (
			departure_id, description, amount_paise, evidence_photo_key, status, tenant_acknowledged_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		ded.DepartureID, ded.Description, ded.AmountPaise, ded.EvidencePhotoKey, ded.Status, ded.TenantAcknowledgedAt, ded.CreatedAt,
	).Scan(&ded.ID)
}

func (r *PayoutRepo) ListDeductions(ctx context.Context, departureID uuid.UUID) ([]domain.DepartureDeduction, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, departure_id, description, amount_paise, evidence_photo_key, status, tenant_acknowledged_at, created_at
		FROM departure_deductions WHERE departure_id=$1 ORDER BY created_at ASC`, departureID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DepartureDeduction
	for rows.Next() {
		var d domain.DepartureDeduction
		if err := rows.Scan(&d.ID, &d.DepartureID, &d.Description, &d.AmountPaise, &d.EvidencePhotoKey, &d.Status, &d.TenantAcknowledgedAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *PayoutRepo) AddDueAdjustment(ctx context.Context, adj *domain.DepartureDueAdjustment) error {
	now := time.Now().UTC()
	adj.CreatedAt = now
	if adj.AdjustmentType == "" {
		adj.AdjustmentType = "unused_rent_reversal"
	}
	return r.pool.QueryRow(ctx, `
		INSERT INTO departure_due_adjustments (departure_id, due_id, amount_paise, adjustment_type, created_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		adj.DepartureID, adj.DueID, adj.AmountPaise, adj.AdjustmentType, adj.CreatedAt,
	).Scan(&adj.ID)
}

func (r *PayoutRepo) ListDueAdjustments(ctx context.Context, departureID uuid.UUID) ([]domain.DepartureDueAdjustment, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, departure_id, due_id, amount_paise, adjustment_type, created_at
		FROM departure_due_adjustments WHERE departure_id=$1 ORDER BY created_at ASC`, departureID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DepartureDueAdjustment
	for rows.Next() {
		var a domain.DepartureDueAdjustment
		if err := rows.Scan(&a.ID, &a.DepartureID, &a.DueID, &a.AmountPaise, &a.AdjustmentType, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------
// Payout Items & Batches
// ---------------------------------------------------------

const itemCols = `id, batch_id, payee_id, departure_id, reference_number,
	amount_paise, purpose, period_label, status, utr, settled_at, failure_reason,
	cf_transfer_id, retry_of, created_at, updated_at`

func scanPayoutItem(row pgx.Row) (*domain.PayoutItem, error) {
	var it domain.PayoutItem
	err := row.Scan(
		&it.ID, &it.BatchID, &it.PayeeID, &it.DepartureID, &it.ReferenceNumber,
		&it.AmountPaise, &it.Purpose, &it.PeriodLabel, &it.Status, &it.UTR, &it.SettledAt,
		&it.FailureReason, &it.CFTransferID, &it.RetryOf, &it.CreatedAt, &it.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func (r *PayoutRepo) CreatePayoutItem(ctx context.Context, it *domain.PayoutItem) error {
	return r.CreatePayoutItemTx(ctx, nil, it)
}

func (r *PayoutRepo) CreatePayoutItemTx(ctx context.Context, tx pgx.Tx, it *domain.PayoutItem) error {
	now := time.Now().UTC()
	it.CreatedAt = now
	it.UpdatedAt = now
	if it.Status == "" {
		it.Status = domain.PayoutPending
	}
	var runner DBTX = r.pool
	if tx != nil {
		runner = tx
	}
	return runner.QueryRow(ctx, `
		INSERT INTO payout_items (
			batch_id, payee_id, departure_id, reference_number,
			amount_paise, purpose, period_label, status, utr, settled_at, failure_reason,
			cf_transfer_id, retry_of, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id`,
		it.BatchID, it.PayeeID, it.DepartureID, it.ReferenceNumber,
		it.AmountPaise, it.Purpose, it.PeriodLabel, it.Status, it.UTR, it.SettledAt, it.FailureReason,
		it.CFTransferID, it.RetryOf, it.CreatedAt, it.UpdatedAt,
	).Scan(&it.ID)
}

func (r *PayoutRepo) GetPayoutItemByID(ctx context.Context, id uuid.UUID) (*domain.PayoutItem, error) {
	return scanPayoutItem(r.pool.QueryRow(ctx, `SELECT `+itemCols+` FROM payout_items WHERE id=$1`, id))
}

func (r *PayoutRepo) ListUnbatchedPendingPayoutItems(ctx context.Context, propertyID uuid.UUID) ([]domain.PayoutItem, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT pi.id, pi.batch_id, pi.payee_id, pi.departure_id, pi.reference_number,
		       pi.amount_paise, pi.purpose, pi.period_label, pi.status, pi.utr, pi.settled_at,
		       pi.failure_reason, pi.cf_transfer_id, pi.retry_of, pi.created_at, pi.updated_at
		FROM payout_items pi
		JOIN payout_payees pp ON pp.id = pi.payee_id
		WHERE pp.property_id=$1 AND pi.batch_id IS NULL AND pi.status='pending'
		ORDER BY pi.created_at ASC, pi.id ASC`, propertyID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PayoutItem
	for rows.Next() {
		it, err := scanPayoutItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

func (r *PayoutRepo) ListPayoutItemsByBatch(ctx context.Context, batchID uuid.UUID) ([]domain.PayoutItem, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+itemCols+` FROM payout_items WHERE batch_id=$1 ORDER BY id ASC`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PayoutItem
	for rows.Next() {
		it, err := scanPayoutItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

const batchCols = `id, property_id, batch_number, format_type, status,
	total_amount_paise, item_count, created_by, approved_by, approved_at,
	file_checksum, notes, created_at, updated_at`

func scanBatch(row pgx.Row) (*domain.PayoutBatch, error) {
	var b domain.PayoutBatch
	err := row.Scan(
		&b.ID, &b.PropertyID, &b.BatchNumber, &b.FormatType, &b.Status,
		&b.TotalAmountPaise, &b.ItemCount, &b.CreatedBy, &b.ApprovedBy, &b.ApprovedAt,
		&b.FileChecksum, &b.Notes, &b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (r *PayoutRepo) GetBatchByID(ctx context.Context, id uuid.UUID) (*domain.PayoutBatch, error) {
	return scanBatch(r.pool.QueryRow(ctx, `SELECT `+batchCols+` FROM payout_batches WHERE id=$1`, id))
}

func (r *PayoutRepo) ListBatchesByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.PayoutBatch, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+batchCols+` FROM payout_batches WHERE property_id=$1 ORDER BY created_at DESC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PayoutBatch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// CreateBatchFromUnbatchedItems bundles unbatched pending payout items into an approved PayoutBatch with HMAC checksum.
func (r *PayoutRepo) CreateBatchFromUnbatchedItems(
	ctx context.Context,
	propertyID, ownerID uuid.UUID,
	batchNumber string,
	checksumSecret []byte,
	notes *string,
) (*domain.PayoutBatch, []domain.PayoutItem, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	// Lock all pending unbatched items for this property
	rows, err := tx.Query(ctx, `
		SELECT pi.id, pi.batch_id, pi.payee_id, pi.departure_id, pi.reference_number,
		       pi.amount_paise, pi.purpose, pi.period_label, pi.status, pi.utr, pi.settled_at,
		       pi.failure_reason, pi.cf_transfer_id, pi.retry_of, pi.created_at, pi.updated_at
		FROM payout_items pi
		JOIN payout_payees pp ON pp.id = pi.payee_id
		WHERE pp.property_id=$1 AND pi.batch_id IS NULL AND pi.status='pending'
		ORDER BY pi.id ASC FOR UPDATE OF pi`, propertyID,
	)
	if err != nil {
		return nil, nil, err
	}
	var items []domain.PayoutItem
	for rows.Next() {
		it, err := scanPayoutItem(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		items = append(items, *it)
	}
	rows.Close()

	if len(items) == 0 {
		return nil, nil, errors.New("no unbatched pending payout items found")
	}

	var totalPaise int64
	for _, it := range items {
		totalPaise += it.AmountPaise
	}

	checksum := domain.ComputeBatchChecksum(checksumSecret, items)
	now := time.Now().UTC()

	batch := &domain.PayoutBatch{
		PropertyID:       propertyID,
		BatchNumber:      batchNumber,
		FormatType:       "instruction_sheet",
		Status:           domain.BatchDraft,
		TotalAmountPaise: totalPaise,
		ItemCount:        len(items),
		CreatedBy:        ownerID,
		ApprovedBy:       nil,
		ApprovedAt:       nil,
		FileChecksum:     &checksum,
		Notes:            notes,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO payout_batches (
			property_id, batch_number, format_type, status,
			total_amount_paise, item_count, created_by, approved_by, approved_at,
			file_checksum, notes, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id`,
		batch.PropertyID, batch.BatchNumber, batch.FormatType, batch.Status,
		batch.TotalAmountPaise, batch.ItemCount, batch.CreatedBy, batch.ApprovedBy, batch.ApprovedAt,
		batch.FileChecksum, batch.Notes, batch.CreatedAt, batch.UpdatedAt,
	).Scan(&batch.ID)
	if err != nil {
		return nil, nil, err
	}

	// Assign batch_id to all items
	for i := range items {
		items[i].BatchID = &batch.ID
	}
	itemIDs := make([]uuid.UUID, len(items))
	for i, it := range items {
		itemIDs[i] = it.ID
	}
	_, err = tx.Exec(ctx, `
		UPDATE payout_items
		SET batch_id=$2, updated_at=NOW()
		WHERE id = ANY($1)`, itemIDs, batch.ID,
	)
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return batch, items, nil
}

// ApproveBatch transitions a draft batch to approved with the specified approver.
func (r *PayoutRepo) ApproveBatch(ctx context.Context, batchID, approverID uuid.UUID) (*domain.PayoutBatch, error) {
	now := time.Now().UTC()
	row := r.pool.QueryRow(ctx, `
		UPDATE payout_batches
		SET status = 'approved', approved_by = $2, approved_at = $3, updated_at = $3
		WHERE id = $1 AND status = 'draft'
		RETURNING `+batchCols,
		batchID, approverID, now,
	)
	b, err := scanBatch(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return b, nil
}

// InitiateBatchTransferTx atomically transitions an approved batch and its pending items to 'processing'
// and enqueues a durable outbox event for asynchronous Cashfree dispatch.
func (r *PayoutRepo) InitiateBatchTransferTx(ctx context.Context, batchID uuid.UUID) (*domain.PayoutBatch, []domain.PayoutItem, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("begin batch transfer tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Lock batch FOR UPDATE
	row := tx.QueryRow(ctx, `
		SELECT `+batchCols+`
		FROM payout_batches
		WHERE id = $1
		FOR UPDATE`, batchID,
	)
	batch, err := scanBatch(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, domain.ErrNotFound
		}
		return nil, nil, fmt.Errorf("lock batch: %w", err)
	}

	if batch.Status != domain.BatchApproved {
		return nil, nil, fmt.Errorf("cannot initiate transfer: batch status is '%s', must be 'approved'", batch.Status)
	}

	// 2. Lock items FOR UPDATE
	rows, err := tx.Query(ctx, `
		SELECT `+itemCols+`
		FROM payout_items
		WHERE batch_id = $1
		FOR UPDATE`, batchID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("lock batch items: %w", err)
	}
	defer rows.Close()

	var items []domain.PayoutItem
	for rows.Next() {
		it, err := scanPayoutItem(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan item: %w", err)
		}
		if it.Status != domain.PayoutPending && it.Status != domain.PayoutProcessing {
			return nil, nil, fmt.Errorf("item %s has invalid status '%s' for batch transfer", it.ID, it.Status)
		}
		items = append(items, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return nil, nil, errors.New("no items found in batch")
	}

	now := time.Now().UTC()

	// 3. Flip batch -> processing
	_, err = tx.Exec(ctx, `
		UPDATE payout_batches
		SET status = 'processing', updated_at = $2
		WHERE id = $1`, batchID, now,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update batch to processing: %w", err)
	}
	batch.Status = domain.BatchProcessing
	batch.UpdatedAt = now

	// 4. Flip items -> processing
	_, err = tx.Exec(ctx, `
		UPDATE payout_items
		SET status = 'processing', updated_at = $2
		WHERE batch_id = $1`, batchID, now,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update items to processing: %w", err)
	}
	for i := range items {
		items[i].Status = domain.PayoutProcessing
		items[i].UpdatedAt = now
	}

	// 5. Transactional outbox event enqueue
	payload, _ := json.Marshal(map[string]any{
		"batch_id": batch.ID,
	})
	outboxEvt := &domain.LedgerOutboxEvent{
		EventType:      "payout_batch_transfer",
		PropertyID:     batch.PropertyID,
		SourceID:       batch.ID,
		Payload:        payload,
		IdempotencyKey: fmt.Sprintf("payout_batch_transfer:%s", batch.ID),
		MaxAttempts:    5,
	}
	if r.outbox != nil {
		if err := r.outbox.InsertLedgerOutboxEventTx(ctx, tx, outboxEvt); err != nil {
			return nil, nil, fmt.Errorf("enqueue payout outbox event: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit batch transfer: %w", err)
	}

	return batch, items, nil
}

// SetBatchDispatchUnknown sets batch status to dispatch_unknown on ambiguous 5xx or transport timeouts.
func (r *PayoutRepo) SetBatchDispatchUnknown(ctx context.Context, batchID uuid.UUID, reason string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE payout_batches
		SET status = 'dispatch_unknown',
		    notes = CASE WHEN notes IS NULL OR notes = '' THEN $2 ELSE notes || '; ' || $2 END,
		    updated_at = NOW()
		WHERE id = $1`, batchID, reason,
	)
	return err
}

// UpdatePayoutItemStatusTx updates status, UTR, settled_at, and failure reason on a payout item.
func (r *PayoutRepo) UpdatePayoutItemStatusTx(
	ctx context.Context,
	tx pgx.Tx,
	itemID uuid.UUID,
	status domain.PayoutItemStatus,
	cfTransferID *string,
	utr *string,
	settledAt *time.Time,
	failureReason *string,
) error {
	var runner DBTX = r.pool
	if tx != nil {
		runner = tx
	}
	_, err := runner.Exec(ctx, `
		UPDATE payout_items
		SET status = $2,
		    cf_transfer_id = COALESCE($3, cf_transfer_id),
		    utr = COALESCE($4, utr),
		    settled_at = COALESCE($5, settled_at),
		    failure_reason = COALESCE($6, failure_reason),
		    updated_at = NOW()
		WHERE id = $1`,
		itemID, status, cfTransferID, utr, settledAt, failureReason,
	)
	return err
}

// UpdateBatchStatusFromItemsTx evaluates all items in a batch and transitions the batch status.
//
// Rules:
// - All items succeeded -> 'completed'
// - All items failed/rejected -> 'failed'
// - Mixed terminal outcomes -> 'partially_failed'
// - Any item in-flight (pending, processing, cashfree_approval_pending, retriable_failed) -> 'processing'
func (r *PayoutRepo) UpdateBatchStatusFromItemsTx(ctx context.Context, tx pgx.Tx, batchID uuid.UUID) (*domain.PayoutBatch, error) {
	var runner DBTX = r.pool
	if tx != nil {
		runner = tx
	}

	rows, err := runner.Query(ctx, `SELECT status FROM payout_items WHERE batch_id = $1`, batchID)
	if err != nil {
		return nil, fmt.Errorf("query item statuses: %w", err)
	}
	defer rows.Close()

	var (
		totalCount     int
		succeededCount int
		failedCount    int
		inFlightCount  int
	)

	for rows.Next() {
		var st domain.PayoutItemStatus
		if err := rows.Scan(&st); err != nil {
			return nil, err
		}
		totalCount++
		switch st {
		case domain.PayoutSucceeded:
			succeededCount++
		case domain.PayoutFailed, domain.PayoutRejected:
			failedCount++
		default:
			inFlightCount++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if totalCount == 0 {
		return nil, errors.New("batch has no items")
	}

	var newStatus domain.PayoutBatchStatus
	if inFlightCount > 0 {
		newStatus = domain.BatchProcessing
	} else if succeededCount == totalCount {
		newStatus = domain.BatchCompleted
	} else if failedCount == totalCount {
		newStatus = domain.BatchFailed
	} else {
		newStatus = domain.BatchPartiallyFailed
	}

	row := runner.QueryRow(ctx, `
		UPDATE payout_batches
		SET status = $2, updated_at = NOW()
		WHERE id = $1
		RETURNING `+batchCols,
		batchID, newStatus,
	)
	return scanBatch(row)
}

type SettleDepartureParams struct {
	DepartureID       uuid.UUID
	ActualVacateDate  time.Time
	ProratedRentPaise int64      // Contractual prorated rent for departure cycle
	PayeeID           *uuid.UUID // Required if net_refund_paise > 0
	Notes             *string
}

type SettleDepartureResult struct {
	Departure                  *domain.TenantDeparture
	Due                        *domain.Due
	PayoutItem                 *domain.PayoutItem
	DueAdjustments             []domain.DepartureDueAdjustment
	InternalPaymentID          *uuid.UUID
	OutstandingDuesNettedPaise int64
	UnusedRentRefundPaise      int64
	ProratedRentOwedPaise      int64
	DeductionsPaise            int64
	NetRefundPaise             int64
	ReceivableBalancePaise     int64
}

// SettleDepartureUnderLock settles a tenant departure respecting the 15-step topological lock order:
// 1. tenants
// 3. dues
// 6. payments (if internal deposit offset)
// 7. payment_allocations (via trigger)
// 10. tenant_departures
// 11. departure_deductions
// 12. departure_due_adjustments
// 13. payout_payees
// 15. payout_items
func (r *PayoutRepo) SettleDepartureUnderLock(ctx context.Context, params SettleDepartureParams) (*SettleDepartureResult, error) {
	// Unlocked read to resolve tenant_id and property_id
	var tenantID, propertyID uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT tenant_id, property_id FROM tenant_departures WHERE id = $1`, params.DepartureID).Scan(&tenantID, &propertyID)
	if err != nil {
		return nil, fmt.Errorf("lookup departure: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Step 1: Lock tenant
	var tStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&tStatus)
	if err != nil {
		return nil, fmt.Errorf("lock tenant: %w", err)
	}
	if tStatus == "vacated" {
		return nil, errors.New("tenant is already vacated")
	}

	// Step 3: Lock all dues for this tenant
	rows, err := tx.Query(ctx, `
		SELECT id, due_code, tenant_id, property_id, kind, amount, original_amount,
		       contractual_ceiling_paise, period_start, period_end, due_date, status,
		       paid_at, created_at, updated_at
		FROM dues
		WHERE tenant_id = $1
		ORDER BY due_date ASC, id ASC FOR UPDATE`, tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("lock dues: %w", err)
	}
	var lockedDues []domain.Due
	for rows.Next() {
		var d domain.Due
		if err := rows.Scan(
			&d.ID, &d.DueCode, &d.TenantID, &d.PropertyID, &d.Kind, &d.Amount, &d.OriginalAmount,
			&d.ContractualCeilingPaise, &d.PeriodStart, &d.PeriodEnd, &d.DueDate, &d.Status,
			&d.PaidAt, &d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			rows.Close()
			return nil, err
		}
		lockedDues = append(lockedDues, d)
	}
	rows.Close()

	// Step 10: Lock departure
	var dep domain.TenantDeparture
	err = tx.QueryRow(ctx, `
		SELECT `+departureCols+`
		FROM tenant_departures
		WHERE id = $1 FOR UPDATE`, params.DepartureID,
	).Scan(
		&dep.ID, &dep.TenantID, &dep.PropertyID, &dep.NoticeGivenAt, &dep.PlannedVacateDate,
		&dep.ActualVacateDate, &dep.InspectedAt, &dep.SLADeadlineAt, &dep.DepositAmountPaise,
		&dep.UnusedRentRefundPaise, &dep.ProratedRentOwedPaise, &dep.OutstandingDuesNettedPaise,
		&dep.DeductionsPaise, &dep.NetRefundPaise, &dep.ReceivableBalancePaise, &dep.Status,
		&dep.Notes, &dep.CreatedAt, &dep.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("lock departure: %w", err)
	}
	if dep.Status != domain.DeparturePending && dep.Status != domain.DepartureInspected {
		return nil, fmt.Errorf("departure in status '%s' cannot be settled", dep.Status)
	}

	// Step 11: Lock departure deductions
	dedRows, err := tx.Query(ctx, `
		SELECT id, departure_id, description, amount_paise, evidence_photo_key, status, tenant_acknowledged_at, created_at
		FROM departure_deductions
		WHERE departure_id = $1 FOR UPDATE`, params.DepartureID,
	)
	if err != nil {
		return nil, fmt.Errorf("lock deductions: %w", err)
	}
	var totalDeductions int64
	for dedRows.Next() {
		var d domain.DepartureDeduction
		if err := dedRows.Scan(&d.ID, &d.DepartureID, &d.Description, &d.AmountPaise, &d.EvidencePhotoKey, &d.Status, &d.TenantAcknowledgedAt, &d.CreatedAt); err != nil {
			dedRows.Close()
			return nil, err
		}
		if d.Status != domain.DeductionWaived {
			totalDeductions += d.AmountPaise
		}
	}
	dedRows.Close()

	// Identify departure cycle rent due
	var cycleDue *domain.Due
	for i := range lockedDues {
		d := &lockedDues[i]
		if d.Kind == domain.DueKindRent {
			if (!d.PeriodStart.After(params.ActualVacateDate)) && (!d.PeriodEnd.Before(params.ActualVacateDate)) {
				cycleDue = d
				break
			}
		}
	}
	if cycleDue == nil {
		for i := len(lockedDues) - 1; i >= 0; i-- {
			if lockedDues[i].Kind == domain.DueKindRent {
				cycleDue = &lockedDues[i]
				break
			}
		}
	}

	// Calculate net paid on cycleDue
	var netPaid int64
	if cycleDue != nil {
		err = tx.QueryRow(ctx, `
			SELECT COALESCE((SELECT SUM(amount_paise) FROM payment_allocations WHERE due_id = $1), 0)
			     - COALESCE((SELECT SUM(ra.amount_paise) FROM refund_allocations ra 
			                 JOIN gateway_refunds gr ON gr.id = ra.refund_id 
			                 WHERE ra.due_id = $1 AND gr.status = 'succeeded'), 0)
			     - COALESCE((SELECT SUM(amount_paise) FROM departure_due_adjustments WHERE due_id = $1), 0)
		`, cycleDue.ID).Scan(&netPaid)
		if err != nil {
			return nil, fmt.Errorf("get net paid for due: %w", err)
		}
	}

	proratedRentPaise := params.ProratedRentPaise

	var (
		unusedRentRefundPaise      int64
		proratedRentOwedPaise      int64
		outstandingDuesNettedPaise int64
		internalPaymentID          *uuid.UUID
		dueAdjustments             []domain.DepartureDueAdjustment
	)

	now := time.Now().UTC()

	if cycleDue != nil {
		ceiling := proratedRentPaise
		cycleDue.ContractualCeilingPaise = &ceiling

		if netPaid < proratedRentPaise {
			// Scenario 1 & 3b: Tenant paid less than prorated rent
			shortfall := proratedRentPaise - netPaid
			proratedRentOwedPaise = shortfall
			unusedRentRefundPaise = 0
			outstandingDuesNettedPaise += shortfall

			// Step 6 & 7: Insert internal deposit offset payment
			var pid uuid.UUID
			err = tx.QueryRow(ctx, `
				INSERT INTO payments (
					tenant_id, property_id, due_id, amount, matched_by, provider, raw_note, is_unapplied, payer_type, matched_at, created_at
				) VALUES ($1, $2, $3, $4, 'deposit_netting', 'internal', 'Settled via departure deposit deduction', false, 'tenant', $5, $5)
				RETURNING id`,
				tenantID, cycleDue.PropertyID, cycleDue.ID, shortfall, now,
			).Scan(&pid)
			if err != nil {
				return nil, fmt.Errorf("insert internal payment: %w", err)
			}
			internalPaymentID = &pid

			// SSoT status derivation
			newNetPaid := netPaid + shortfall
			newStatus, newAmount := domain.RecomputeDueStatusMath(newNetPaid, ceiling)
			cycleDue.Status = newStatus
			cycleDue.Amount = newAmount
			cycleDue.PaidAt = &now

			_, err = tx.Exec(ctx, `
				UPDATE dues
				SET contractual_ceiling_paise = $2,
					status = $3,
					amount = $4,
					paid_at = $5,
					updated_at = $5
				WHERE id = $1`,
				cycleDue.ID, cycleDue.ContractualCeilingPaise, cycleDue.Status, cycleDue.Amount, now,
			)
			if err != nil {
				return nil, fmt.Errorf("update due: %w", err)
			}

		} else if netPaid > proratedRentPaise {
			// Scenario 2 & 3: Tenant prepaid more than prorated rent
			excess := netPaid - proratedRentPaise
			unusedRentRefundPaise = excess
			proratedRentOwedPaise = 0

			// Step 12: Insert into departure_due_adjustments
			adj := domain.DepartureDueAdjustment{
				DepartureID:    params.DepartureID,
				DueID:          cycleDue.ID,
				AmountPaise:    excess,
				AdjustmentType: "unused_rent_reversal",
				CreatedAt:      now,
			}
			err = tx.QueryRow(ctx, `
				INSERT INTO departure_due_adjustments (departure_id, due_id, amount_paise, adjustment_type, created_at)
				VALUES ($1, $2, $3, $4, $5)
				RETURNING id`,
				adj.DepartureID, adj.DueID, adj.AmountPaise, adj.AdjustmentType, adj.CreatedAt,
			).Scan(&adj.ID)
			if err != nil {
				return nil, fmt.Errorf("insert due adjustment: %w", err)
			}
			dueAdjustments = append(dueAdjustments, adj)

			// SSoT status derivation
			newNetPaid := netPaid - excess
			newStatus, newAmount := domain.RecomputeDueStatusMath(newNetPaid, ceiling)
			cycleDue.Status = newStatus
			cycleDue.Amount = newAmount

			_, err = tx.Exec(ctx, `
				UPDATE dues
				SET contractual_ceiling_paise = $2,
					status = $3,
					amount = $4,
					updated_at = $5
				WHERE id = $1`,
				cycleDue.ID, cycleDue.ContractualCeilingPaise, cycleDue.Status, cycleDue.Amount, now,
			)
			if err != nil {
				return nil, fmt.Errorf("update due: %w", err)
			}

		} else {
			// Exact match
			proratedRentOwedPaise = 0
			unusedRentRefundPaise = 0

			newStatus, newAmount := domain.RecomputeDueStatusMath(netPaid, ceiling)
			cycleDue.Status = newStatus
			cycleDue.Amount = newAmount

			_, err = tx.Exec(ctx, `
				UPDATE dues
				SET contractual_ceiling_paise = $2,
					status = $3,
					amount = $4,
					updated_at = $5
				WHERE id = $1`,
				cycleDue.ID, cycleDue.ContractualCeilingPaise, cycleDue.Status, cycleDue.Amount, now,
			)
			if err != nil {
				return nil, fmt.Errorf("update due: %w", err)
			}
		}
	}

	// Net any other open dues from deposit
	for i := range lockedDues {
		d := &lockedDues[i]
		if cycleDue != nil && d.ID == cycleDue.ID {
			continue
		}
		if d.Status == domain.DueStatusPending || d.Status == domain.DueStatusPartial {
			unpaid := int64(d.Amount)
			if unpaid > 0 {
				outstandingDuesNettedPaise += unpaid
				var pid uuid.UUID
				err = tx.QueryRow(ctx, `
					INSERT INTO payments (
						tenant_id, property_id, due_id, amount, matched_by, provider, raw_note, is_unapplied, payer_type, matched_at, created_at
					) VALUES ($1, $2, $3, $4, 'deposit_netting', 'internal', 'Settled via departure deposit deduction', false, 'tenant', $5, $5)
					RETURNING id`,
					tenantID, d.PropertyID, d.ID, unpaid, now,
				).Scan(&pid)
				if err != nil {
					return nil, fmt.Errorf("insert internal payment for open due %s: %w", d.ID, err)
				}
				d.Status = domain.DueStatusPaid
				d.Amount = 0
				d.PaidAt = &now
				_, err = tx.Exec(ctx, `
					UPDATE dues SET status = 'paid', amount = 0, paid_at = $2, updated_at = $2 WHERE id = $1`,
					d.ID, now,
				)
				if err != nil {
					return nil, fmt.Errorf("update open due %s: %w", d.ID, err)
				}
			}
		}
	}

	// Net refund calculation
	totalCredits := dep.DepositAmountPaise + unusedRentRefundPaise
	totalDebits := outstandingDuesNettedPaise + totalDeductions

	var netRefundPaise int64
	var receivableBalancePaise int64
	if totalCredits >= totalDebits {
		netRefundPaise = totalCredits - totalDebits
		receivableBalancePaise = 0
	} else {
		netRefundPaise = 0
		receivableBalancePaise = totalDebits - totalCredits
	}

	// Step 13 & 15: payout_payees & payout_items
	var payoutItem *domain.PayoutItem
	if netRefundPaise > 0 {
		var targetPayeeID uuid.UUID
		if params.PayeeID != nil {
			targetPayeeID = *params.PayeeID
		} else {
			err = tx.QueryRow(ctx, `
				SELECT id FROM payout_payees
				WHERE property_id = $1 AND payee_type IN ('tenant_deposit', 'guardian_deposit')
				ORDER BY created_at DESC LIMIT 1`, propertyID,
			).Scan(&targetPayeeID)
			if err != nil {
				return nil, errors.New("payee_id is required for deposit refund payout")
			}
		}

		// Step 13: Lock payout_payee
		var payeeExists bool
		err = tx.QueryRow(ctx, `SELECT true FROM payout_payees WHERE id = $1 FOR UPDATE`, targetPayeeID).Scan(&payeeExists)
		if err != nil {
			return nil, fmt.Errorf("lock payee %s: %w", targetPayeeID, err)
		}

		// Step 15: Insert unbatched payout item
		refNum := fmt.Sprintf("dep_%s", strings.ReplaceAll(dep.ID.String(), "-", ""))
		periodLabel := fmt.Sprintf("dep_%s", params.ActualVacateDate.Format("2006_01"))
		it := domain.PayoutItem{
			PayeeID:         targetPayeeID,
			DepartureID:     &dep.ID,
			ReferenceNumber: refNum,
			AmountPaise:     netRefundPaise,
			Purpose:         "tenant_deposit_refund",
			PeriodLabel:     periodLabel,
			Status:          domain.PayoutPending,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO payout_items (
				batch_id, payee_id, departure_id, reference_number,
				amount_paise, purpose, period_label, status, created_at, updated_at
			) VALUES (
				NULL, $1, $2, $3,
				$4, $5, $6, $7, $8, $9
			) RETURNING id`,
			it.PayeeID, it.DepartureID, it.ReferenceNumber,
			it.AmountPaise, it.Purpose, it.PeriodLabel, it.Status, it.CreatedAt, it.UpdatedAt,
		).Scan(&it.ID)
		if err != nil {
			return nil, fmt.Errorf("insert payout item: %w", err)
		}
		payoutItem = &it
	}

	// Update departure
	dep.ActualVacateDate = &params.ActualVacateDate
	dep.UnusedRentRefundPaise = unusedRentRefundPaise
	dep.ProratedRentOwedPaise = proratedRentOwedPaise
	dep.OutstandingDuesNettedPaise = outstandingDuesNettedPaise
	dep.DeductionsPaise = totalDeductions
	dep.NetRefundPaise = netRefundPaise
	dep.ReceivableBalancePaise = receivableBalancePaise
	dep.Status = domain.DepartureApproved
	if params.Notes != nil {
		dep.Notes = params.Notes
	}
	dep.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE tenant_departures
		SET actual_vacate_date = $2,
			unused_rent_refund_paise = $3,
			prorated_rent_owed_paise = $4,
			outstanding_dues_netted_paise = $5,
			deductions_paise = $6,
			net_refund_paise = $7,
			receivable_balance_paise = $8,
			status = $9,
			notes = COALESCE($10, notes),
			updated_at = $11
		WHERE id = $1`,
		dep.ID, dep.ActualVacateDate, dep.UnusedRentRefundPaise, dep.ProratedRentOwedPaise,
		dep.OutstandingDuesNettedPaise, dep.DeductionsPaise, dep.NetRefundPaise,
		dep.ReceivableBalancePaise, dep.Status, dep.Notes, now,
	)
	if err != nil {
		return nil, fmt.Errorf("update departure: %w", err)
	}

	// Update tenant to vacated
	_, err = tx.Exec(ctx, `
		UPDATE tenants
		SET status = 'vacated', updated_at = $2
		WHERE id = $1`, tenantID, now,
	)
	if err != nil {
		return nil, fmt.Errorf("update tenant to vacated: %w", err)
	}

	// Step 16: Transactional Ledger Outbox Enqueue (before commit)
	mirrorPayload := domain.DepartureSettlementMirrorPayload{
		PropertyID:                 dep.PropertyID,
		DepartureID:                dep.ID,
		DepositAmountPaise:         dep.DepositAmountPaise,
		UnusedRentRefundPaise:      unusedRentRefundPaise,
		TotalDeductions:            totalDeductions,
		NetRefundPaise:             netRefundPaise,
		OutstandingDuesNettedPaise: outstandingDuesNettedPaise,
		ReceivableBalancePaise:     receivableBalancePaise,
		OccurredAt:                 now,
	}
	payloadBytes, _ := json.Marshal(mirrorPayload)
	outboxEvt := &domain.LedgerOutboxEvent{
		EventType:      "departure_settlement_mirror",
		PropertyID:     dep.PropertyID,
		SourceID:       dep.ID,
		Payload:        payloadBytes,
		IdempotencyKey: fmt.Sprintf("departure_settlement:%s", dep.ID),
		MaxAttempts:    5,
	}
	if r.outbox != nil {
		if err := r.outbox.InsertLedgerOutboxEventTx(ctx, tx, outboxEvt); err != nil {
			return nil, fmt.Errorf("enqueue ledger outbox event: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit departure settlement: %w", err)
	}

	// Post balanced financial mirror journals (inline fast path)
	if r.mirrorer != nil {
		if err := r.mirrorer.MirrorDepartureSettlement(
			ctx,
			dep.PropertyID, dep.ID,
			dep.DepositAmountPaise,
			unusedRentRefundPaise,
			totalDeductions,
			netRefundPaise,
			outstandingDuesNettedPaise,
			receivableBalancePaise,
			now,
		); err != nil {
			slog.Warn("inline departure settlement mirror deferred to ledger outbox processor",
				"departure_id", dep.ID,
				"err", err,
			)
		} else if r.outbox != nil {
			_ = r.outbox.MarkProcessedByIdempotencyKey(ctx, outboxEvt.IdempotencyKey)
		}
	}

	return &SettleDepartureResult{
		Departure:                  &dep,
		Due:                        cycleDue,
		PayoutItem:                 payoutItem,
		DueAdjustments:             dueAdjustments,
		InternalPaymentID:          internalPaymentID,
		OutstandingDuesNettedPaise: outstandingDuesNettedPaise,
		UnusedRentRefundPaise:      unusedRentRefundPaise,
		ProratedRentOwedPaise:      proratedRentOwedPaise,
		DeductionsPaise:            totalDeductions,
		NetRefundPaise:             netRefundPaise,
		ReceivableBalancePaise:     receivableBalancePaise,
	}, nil
}
