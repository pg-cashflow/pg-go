package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// ApproveBatchSigned atomically validates batch instructions, locks payee instruments,
// creates an immutable PayoutApprovalSnapshot, signs it using HMAC-SHA256 with the dedicated key,
// records the snapshot in payout_approval_snapshots, and transitions the batch status to 'approved'.
//
// Invariant: If signing, payee instrument lookup, or snapshot insertion fails, the entire transaction
// rolls back and the batch remains in 'draft'.
// Preserves property-scoped isolation (RLS) across pool transactions and scoped DB handles.
func (r *PayoutRepo) ApproveBatchSigned(
	ctx context.Context,
	propertyID, batchID, approverID uuid.UUID,
	signingKey []byte,
	keyID string,
) (*domain.PayoutBatch, *domain.PayoutApprovalSnapshot, error) {
	if len(signingKey) < domain.MinApprovalSigningKeyLength {
		return nil, nil, domain.ErrInvalidSigningKey
	}
	if strings.TrimSpace(keyID) == "" {
		keyID = "v1"
	}

	if propertyID == uuid.Nil {
		if pid, ok := requestscope.PropertyIDFromContext(ctx); ok {
			propertyID = pid
		}
	}

	var tx pgx.Tx
	var ownsTx bool

	if t, ok := r.db.(pgx.Tx); ok {
		tx = t
	} else if t, ok := TxFromContext(ctx); ok {
		tx = t
	} else if r.pool != nil {
		t, err := r.pool.Begin(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("begin approval tx: %w", err)
		}
		tx = t
		ownsTx = true
		defer func() { _ = tx.Rollback(ctx) }()
	} else {
		return nil, nil, errors.New("database pool unavailable")
	}

	// 1. Establish property-level RLS boundary within the transaction if propertyID is known
	if propertyID != uuid.Nil {
		if _, err := tx.Exec(ctx, "SELECT set_config('role', 'pgapp_app', true), set_config('app.current_property_id', $1, true)", propertyID.String()); err != nil {
			return nil, nil, fmt.Errorf("set local app.current_property_id: %w", err)
		}
	}

	// 2. Lock the draft batch
	var batch domain.PayoutBatch
	err := tx.QueryRow(ctx, `
		SELECT id, property_id, batch_number, format_type, status, total_amount_paise,
		       item_count, created_by, approved_by, approved_at, file_checksum, notes, created_at, updated_at
		FROM payout_batches
		WHERE id = $1 AND ($2 = '00000000-0000-0000-0000-000000000000'::uuid OR property_id = $2) AND status = 'draft'
		FOR UPDATE`, batchID, propertyID,
	).Scan(
		&batch.ID, &batch.PropertyID, &batch.BatchNumber, &batch.FormatType, &batch.Status,
		&batch.TotalAmountPaise, &batch.ItemCount, &batch.CreatedBy, &batch.ApprovedBy,
		&batch.ApprovedAt, &batch.FileChecksum, &batch.Notes, &batch.CreatedAt, &batch.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, domain.ErrNotFound
		}
		return nil, nil, fmt.Errorf("lock payout batch: %w", err)
	}

	// If propertyID was not initially supplied, bind it now and enforce RLS
	if propertyID == uuid.Nil {
		propertyID = batch.PropertyID
		if _, err := tx.Exec(ctx, "SELECT set_config('role', 'pgapp_app', true), set_config('app.current_property_id', $1, true)", propertyID.String()); err != nil {
			return nil, nil, fmt.Errorf("set local app.current_property_id: %w", err)
		}
	}

	// 3. Lock all payout items belonging to this batch
	rows, err := tx.Query(ctx, `
		SELECT id, batch_id, payee_id, departure_id, reference_number,
		       amount_paise, purpose, period_label, status, utr, settled_at,
		       failure_reason, cf_transfer_id, retry_of, created_at, updated_at
		FROM payout_items
		WHERE batch_id = $1 AND status = 'pending'
		ORDER BY id ASC
		FOR UPDATE`, batchID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("lock payout items: %w", err)
	}
	defer rows.Close()

	var items []domain.PayoutItem
	var calculatedTotal int64
	payeeIDMap := make(map[uuid.UUID]struct{})
	for rows.Next() {
		it, err := scanPayoutItem(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan payout item: %w", err)
		}
		items = append(items, *it)
		calculatedTotal += it.AmountPaise
		payeeIDMap[it.PayeeID] = struct{}{}
	}
	rows.Close()

	if len(items) == 0 {
		return nil, nil, fmt.Errorf("payout batch %s has no pending items", batchID)
	}
	if len(items) != batch.ItemCount || calculatedTotal != batch.TotalAmountPaise {
		return nil, nil, fmt.Errorf("batch item count or amount mismatch: expected %d items totaling %d paise, got %d items totaling %d paise",
			batch.ItemCount, batch.TotalAmountPaise, len(items), calculatedTotal)
	}

	// 4. Lock and resolve all destination instruments from payout_payees
	payeeIDs := make([]uuid.UUID, 0, len(payeeIDMap))
	for pid := range payeeIDMap {
		payeeIDs = append(payeeIDs, pid)
	}

	payeeRows, err := tx.Query(ctx, `
		SELECT id, property_id, payee_type, name, phone, account_number_encrypted,
		       account_number_last4, account_number_hash, ifsc, bank_name, upi_vpa,
		       key_version, is_verified, verified_by, verified_at, created_at, updated_at
		FROM payout_payees
		WHERE id = ANY($1) AND property_id = $2
		FOR UPDATE`, payeeIDs, batch.PropertyID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("lock payout payees: %w", err)
	}
	defer payeeRows.Close()

	payeeMap := make(map[uuid.UUID]*domain.PayoutPayee)
	for payeeRows.Next() {
		p, err := scanPayee(payeeRows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan payee: %w", err)
		}
		if !p.IsVerified {
			return nil, nil, fmt.Errorf("payee %s (%s) is not verified; disbursement blocked", p.ID, p.Name)
		}
		if strings.TrimSpace(p.AccountNumberHash) == "" {
			return nil, nil, fmt.Errorf("payee %s (%s) has no account number hash fingerprint", p.ID, p.Name)
		}
		payeeMap[p.ID] = p
	}
	payeeRows.Close()

	// 5. Build immutable ApprovedItemSnapshots binding destination details
	approvedItems := make([]domain.ApprovedItemSnapshot, 0, len(items))
	for _, it := range items {
		p, ok := payeeMap[it.PayeeID]
		if !ok {
			return nil, nil, fmt.Errorf("missing payee record for item %s", it.ID)
		}
		ifscStr := ""
		if p.IFSC != nil {
			ifscStr = strings.ToUpper(strings.TrimSpace(*p.IFSC))
		}
		vpaStr := ""
		if p.UPIVPA != nil {
			vpaStr = strings.ToLower(strings.TrimSpace(*p.UPIVPA))
		}

		approvedItems = append(approvedItems, domain.ApprovedItemSnapshot{
			ItemID:            it.ID,
			PayeeID:           it.PayeeID,
			ReferenceNumber:   it.ReferenceNumber,
			AmountPaise:       it.AmountPaise,
			Purpose:           it.Purpose,
			PeriodLabel:       it.PeriodLabel,
			AccountNumberHash: p.AccountNumberHash,
			IFSC:              ifscStr,
			UPIVPA:            vpaStr,
		})
	}

	now := time.Now().UTC()
	snapshot := &domain.PayoutApprovalSnapshot{
		SchemaVersion: domain.CurrentSnapshotSchemaVersion,
		BatchID:       batch.ID,
		PropertyID:    batch.PropertyID,
		ApprovedBy:    approverID,
		ApprovedAt:    now,
		TotalAmount:   calculatedTotal,
		ItemCount:     len(items),
		Items:         approvedItems,
	}

	// 6. Sign snapshot with dedicated approval secret
	signature, err := domain.SignApprovalSnapshot(signingKey, snapshot)
	if err != nil {
		return nil, nil, fmt.Errorf("sign approval snapshot: %w", err)
	}

	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal approval snapshot: %w", err)
	}

	// 7. Persist to payout_approval_snapshots (RLS protected)
	_, err = tx.Exec(ctx, `
		INSERT INTO payout_approval_snapshots (
			batch_id, property_id, schema_version, approved_by, approved_at,
			total_amount_paise, item_count, snapshot_data, signature, key_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		batch.ID, batch.PropertyID, snapshot.SchemaVersion, approverID, now,
		snapshot.TotalAmount, snapshot.ItemCount, snapshotJSON, signature, keyID,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("insert approval snapshot: %w", err)
	}

	// 8. Transition batch status to 'approved'
	var approvedBatch domain.PayoutBatch
	err = tx.QueryRow(ctx, `
		UPDATE payout_batches
		SET status = 'approved', approved_by = $2, approved_at = $3, updated_at = $3
		WHERE id = $1 AND status = 'draft'
		RETURNING `+batchCols,
		batchID, approverID, now,
	).Scan(
		&approvedBatch.ID, &approvedBatch.PropertyID, &approvedBatch.BatchNumber,
		&approvedBatch.FormatType, &approvedBatch.Status, &approvedBatch.TotalAmountPaise,
		&approvedBatch.ItemCount, &approvedBatch.CreatedBy, &approvedBatch.ApprovedBy,
		&approvedBatch.ApprovedAt, &approvedBatch.FileChecksum, &approvedBatch.Notes,
		&approvedBatch.CreatedAt, &approvedBatch.UpdatedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("update batch status to approved: %w", err)
	}

	if ownsTx {
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, fmt.Errorf("commit approval tx: %w", err)
		}
	}

	return &approvedBatch, snapshot, nil
}

// GetApprovalSnapshotByBatchID queries the immutable approval snapshot and HMAC signature for a batch.
// If propertyID is provided, it establishes the property-level RLS boundary on the connection.
func (r *PayoutRepo) GetApprovalSnapshotByBatchID(
	ctx context.Context,
	batchID uuid.UUID,
	propertyID ...uuid.UUID,
) (*domain.PayoutApprovalSnapshot, string, string, error) {
	var propID uuid.UUID
	if len(propertyID) > 0 && propertyID[0] != uuid.Nil {
		propID = propertyID[0]
	} else if pid, ok := requestscope.PropertyIDFromContext(ctx); ok {
		propID = pid
	}
	if propID != uuid.Nil {
		_, _ = r.db.Exec(ctx, "SELECT set_config('role', 'pgapp_app', true), set_config('app.current_property_id', $1, true)", propID.String())
	}

	var rawJSON []byte
	var signature string
	var keyID string

	err := r.db.QueryRow(ctx, `
		SELECT snapshot_data, signature, key_id
		FROM payout_approval_snapshots
		WHERE batch_id = $1`, batchID,
	).Scan(&rawJSON, &signature, &keyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", "", domain.ErrNotFound
		}
		return nil, "", "", fmt.Errorf("query approval snapshot: %w", err)
	}

	var snapshot domain.PayoutApprovalSnapshot
	if err := json.Unmarshal(rawJSON, &snapshot); err != nil {
		return nil, "", "", fmt.Errorf("unmarshal approval snapshot: %w", err)
	}

	return &snapshot, signature, keyID, nil
}

// VerifyBatchApproval verifies that the batch has an immutable, cryptographically valid approval snapshot
// signed by an authorized key in approvalKeys, matching the batch's total amount, count, and property scope.
func (r *PayoutRepo) VerifyBatchApproval(
	ctx context.Context,
	batchID uuid.UUID,
	approvalKeys map[string][]byte,
) (*domain.PayoutApprovalSnapshot, error) {
	if len(approvalKeys) == 0 {
		return nil, errors.New("no approval signing keys provided for verification")
	}

	// 1. Fetch batch to verify status, property, and totals
	var batch domain.PayoutBatch
	err := r.db.QueryRow(ctx, `
		SELECT id, property_id, status, total_amount_paise, item_count
		FROM payout_batches
		WHERE id = $1`, batchID,
	).Scan(&batch.ID, &batch.PropertyID, &batch.Status, &batch.TotalAmountPaise, &batch.ItemCount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("verify batch approval: query batch: %w", err)
	}

	// 2. Query approval snapshot with property context for RLS
	snapshot, signature, keyID, err := r.GetApprovalSnapshotByBatchID(ctx, batchID, batch.PropertyID)
	if err != nil {
		return nil, fmt.Errorf("verify batch approval: %w", err)
	}

	// 3. Find signing key
	key, ok := approvalKeys[keyID]
	if !ok || len(key) < domain.MinApprovalSigningKeyLength {
		return nil, fmt.Errorf("unknown or invalid approval signing key_id %q", keyID)
	}

	// 4. Verify cryptographic signature
	if err := domain.VerifyApprovalSnapshot(key, snapshot, signature); err != nil {
		return nil, fmt.Errorf("approval snapshot signature verification failed: %w", err)
	}

	// 5. Verify batch affirmative invariants against signed snapshot
	if snapshot.BatchID != batch.ID {
		return nil, fmt.Errorf("approval snapshot batch_id mismatch: snapshot=%s, batch=%s", snapshot.BatchID, batch.ID)
	}
	if snapshot.PropertyID != batch.PropertyID {
		return nil, fmt.Errorf("approval snapshot property_id mismatch: snapshot=%s, batch=%s", snapshot.PropertyID, batch.PropertyID)
	}
	if snapshot.ItemCount != batch.ItemCount {
		return nil, fmt.Errorf("approval snapshot item count mismatch: snapshot=%d, batch=%d", snapshot.ItemCount, batch.ItemCount)
	}
	if snapshot.TotalAmount != batch.TotalAmountPaise {
		return nil, fmt.Errorf("approval snapshot total amount mismatch: snapshot=%d, batch=%d", snapshot.TotalAmount, batch.TotalAmountPaise)
	}

	return snapshot, nil
}
