package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/crypto"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"golang.org/x/sync/errgroup"
)

// PayoutBatchDispatcher defines the worker interface for dispatching approved payout batches to Cashfree.
type PayoutBatchDispatcher interface {
	DispatchBatch(ctx context.Context, batchID uuid.UUID) error
}

// PayoutDispatcher handles the Cashfree Transfers V2 batch transfer lifecycle.
type PayoutDispatcher struct {
	repo          *postgres.PayoutRepo
	client        *cashfree.PayoutClient
	fundsourceID  string
	encryptionKey       []byte
	keyRing             *crypto.KeyRing
	approvalSigningKeys map[string][]byte
}

// NewPayoutDispatcher creates a new PayoutDispatcher.
func NewPayoutDispatcher(repo *postgres.PayoutRepo, client *cashfree.PayoutClient, fundsourceID string, encryptionKey ...[]byte) *PayoutDispatcher {
	var key []byte
	if len(encryptionKey) > 0 {
		key = encryptionKey[0]
	}
	return &PayoutDispatcher{
		repo:          repo,
		client:        client,
		fundsourceID:  fundsourceID,
		encryptionKey: key,
	}
}

// SetKeyRing sets the versioned key ring for decrypting payee account numbers.
func (d *PayoutDispatcher) SetKeyRing(kr *crypto.KeyRing) {
	d.keyRing = kr
}

// SetApprovalSigningKeys sets the dedicated HMAC signing keys for verifying approved batch snapshots.
func (d *PayoutDispatcher) SetApprovalSigningKeys(keys map[string][]byte) {
	d.approvalSigningKeys = keys
}

// DispatchBatch processes an outbox-triggered batch transfer against Cashfree Transfers V2.
func (d *PayoutDispatcher) DispatchBatch(ctx context.Context, batchID uuid.UUID) error {
	if d.client == nil {
		return fmt.Errorf("payout client not configured")
	}

	if d.repo != nil {
		acquired, unlock, lockErr := d.repo.TryLockBatchDispatch(ctx, batchID)
		if lockErr != nil {
			return fmt.Errorf("payout dispatcher: lock batch %s: %w", batchID, lockErr)
		}
		if !acquired {
			slog.Info("payout dispatcher: batch already being dispatched by another worker, skipping", "batch_id", batchID)
			return nil
		}
		defer unlock()
	}

	batch, err := d.repo.GetBatchByID(ctx, batchID)
	if err != nil {
		return fmt.Errorf("payout dispatcher: get batch %s: %w", batchID, err)
	}
	if batch == nil {
		return fmt.Errorf("payout dispatcher: batch %s not found", batchID)
	}

	// Idempotency: If batch already reached terminal status, safe no-op
	switch batch.Status {
	case domain.BatchCompleted, domain.BatchPartiallyFailed, domain.BatchFailed, domain.BatchCancelled:
		return nil
	}

	items, err := d.repo.ListPayoutItemsByBatch(ctx, batchID)
	if err != nil {
		return fmt.Errorf("payout dispatcher: list items for batch %s: %w", batchID, err)
	}
	if len(items) == 0 {
		return fmt.Errorf("payout dispatcher: batch %s has no items", batchID)
	}

	batchTransferID := fmt.Sprintf("pgo_%s", strings.ReplaceAll(batch.ID.String(), "-", ""))

	// Verification Gate: Verify immutable approval snapshot and HMAC signature before outbound dispatch
	if len(d.approvalSigningKeys) == 0 {
		return errors.New("payout dispatcher: approval signing keys not configured; refusing to dispatch unverified batch")
	}
	snapshot, err := d.repo.VerifyBatchApproval(ctx, batchID, d.approvalSigningKeys)
	if err != nil {
		return fmt.Errorf("payout dispatcher: approval integrity failed: %w", err)
	}
	if len(snapshot.Items) != len(items) {
		return fmt.Errorf("payout dispatcher: approved snapshot has %d items, batch has %d items", len(snapshot.Items), len(items))
	}
	approvedItemsByID := make(map[uuid.UUID]domain.ApprovedItemSnapshot, len(snapshot.Items))
	for _, appItem := range snapshot.Items {
		approvedItemsByID[appItem.ItemID] = appItem
	}

	// Step 1: Deduplicate and verify all payees locally before any provider interaction
	uniquePayees := make(map[uuid.UUID]*domain.PayoutPayee)
	for _, it := range items {
		if _, exists := uniquePayees[it.PayeeID]; exists {
			continue
		}
		payee, err := d.repo.GetPayeeByID(ctx, it.PayeeID)
		if err != nil {
			return fmt.Errorf("payout dispatcher: get payee %s for item %s: %w", it.PayeeID, it.ID, err)
		}
		if !payee.IsVerified {
			return fmt.Errorf("payout dispatcher: payee %s for item %s is not verified", it.PayeeID, it.ID)
		}
		uniquePayees[it.PayeeID] = payee
	}

	// Full TOCTOU Protection: Ensure current payee records and item instructions have not been altered since owner approval
	if approvedItemsByID != nil {
		for _, it := range items {
			appItem, ok := approvedItemsByID[it.ID]
			if !ok {
				return fmt.Errorf("payout dispatcher: item %s not present in approved snapshot", it.ID)
			}
			if it.PayeeID != appItem.PayeeID {
				return fmt.Errorf("payout dispatcher: TOCTOU detected: item %s payee mismatch (approved: %s, current: %s)",
					it.ID, appItem.PayeeID, it.PayeeID)
			}
			if it.AmountPaise != appItem.AmountPaise {
				return fmt.Errorf("payout dispatcher: TOCTOU detected: item %s amount mismatch (approved: %d, current: %d)",
					it.ID, appItem.AmountPaise, it.AmountPaise)
			}
			if it.ReferenceNumber != appItem.ReferenceNumber {
				return fmt.Errorf("payout dispatcher: TOCTOU detected: item %s reference mismatch (approved: %q, current: %q)",
					it.ID, appItem.ReferenceNumber, it.ReferenceNumber)
			}

			payee := uniquePayees[it.PayeeID]
			if payee.AccountNumberHash != appItem.AccountNumberHash {
				return fmt.Errorf("payout dispatcher: TOCTOU detected: payee %s account hash changed since approval", payee.ID)
			}
			currentIFSC := ""
			if payee.IFSC != nil {
				currentIFSC = strings.ToUpper(strings.TrimSpace(*payee.IFSC))
			}
			if currentIFSC != appItem.IFSC {
				return fmt.Errorf("payout dispatcher: TOCTOU detected: payee %s IFSC changed since approval (approved: %s, current: %s)",
					payee.ID, appItem.IFSC, currentIFSC)
			}
			currentVPA := ""
			if payee.UPIVPA != nil {
				currentVPA = strings.ToLower(strings.TrimSpace(*payee.UPIVPA))
			}
			if currentVPA != appItem.UPIVPA {
				return fmt.Errorf("payout dispatcher: TOCTOU detected: payee %s UPI VPA changed since approval (approved: %s, current: %s)",
					payee.ID, appItem.UPIVPA, currentVPA)
			}
		}
	}

	// Step 2: Ambiguity Resolver — if batch is in dispatch_unknown or processing, query Cashfree status first!
	if batch.Status == domain.BatchDispatchUnknown || batch.Status == domain.BatchProcessing {
		statusResp, err := d.client.GetBatchTransferStatus(ctx, batchTransferID)
		if err == nil && statusResp != nil && statusResp.CFBatchTransferID != "" {
			slog.Info("cashfree batch transfer recovered via status poll",
				"batch_id", batch.ID,
				"cf_batch_id", statusResp.CFBatchTransferID,
				"status", statusResp.Status,
			)
			// Successfully queried status: reconcile items and return
			return d.reconcileBatchStatus(ctx, batch, items, statusResp)
		}

		// If the batch had an ambiguous prior dispatch (dispatch_unknown), we can only proceed
		// if the provider definitively confirms the batch does not exist (ErrBatchNotFound).
		// Any other error (network timeout, 5xx) leaves prior execution unresolved.
		if batch.Status == domain.BatchDispatchUnknown {
			if !errors.Is(err, cashfree.ErrBatchNotFound) {
				return fmt.Errorf("payout dispatcher: batch %s has unresolved prior dispatch; reconciliation required: %w", batch.ID, err)
			}
		}

		// For batches currently in processing (e.g. freshly initiated in local DB), a 404/ErrBatchNotFound
		// is expected since the batch has not yet been dispatched to Cashfree. If the status lookup failed
		// with a non-404 error (e.g. client auth/network transport failure), fail closed before attempting dispatch.
		if batch.Status == domain.BatchProcessing && err != nil && !errors.Is(err, cashfree.ErrBatchNotFound) {
			return fmt.Errorf("payout dispatcher: cannot verify batch status before dispatch: %w", err)
		}
	}

	// Step 3: Register / verify beneficiaries with Cashfree Transfers V2
	// Fail closed: ALL beneficiaries must be verified before a single transfer is submitted.
	var g errgroup.Group
	g.SetLimit(8)
	for _, payee := range uniquePayees {
		p := payee
		beneID := cashfree.DeriveBeneficiaryID(p.ID, p.AccountNumberHash)
		g.Go(func() error {
			if err := d.ensureBeneficiary(ctx, p, beneID); err != nil {
				return fmt.Errorf("register beneficiary %s: %w", beneID, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("payout dispatcher: refusing batch dispatch: %w", err)
	}

	transferEntries := make([]cashfree.BatchTransferEntry, 0, len(items))
	for _, it := range items {
		payee := uniquePayees[it.PayeeID]
		accountHash := payee.AccountNumberHash
		amountPaise := it.AmountPaise
		mode := "banktransfer"
		if payee.UPIVPA != nil && strings.TrimSpace(*payee.UPIVPA) != "" {
			mode = "upi"
		}

		// Bind transfer parameters strictly to immutable approved snapshot details
		if approvedItemsByID != nil {
			appItem := approvedItemsByID[it.ID]
			accountHash = appItem.AccountNumberHash
			amountPaise = appItem.AmountPaise
			if appItem.UPIVPA != "" {
				mode = "upi"
			} else {
				mode = "banktransfer"
			}
		}

		beneID := cashfree.DeriveBeneficiaryID(it.PayeeID, accountHash)
		remarks := strings.TrimSpace(fmt.Sprintf("%s %s", it.Purpose, it.ReferenceNumber))
		if len(remarks) > 70 {
			remarks = remarks[:70]
		}

		transferEntries = append(transferEntries, cashfree.BatchTransferEntry{
			TransferID:      fmt.Sprintf("pgo_%s", strings.ReplaceAll(it.ID.String(), "-", "")),
			TransferAmount:  json.Number(cashfree.FormatPaiseToRupees(amountPaise)),
			TransferMode:    mode,
			FundsourceID:    d.fundsourceID,
			TransferRemarks: remarks,
			BeneficiaryDetails: cashfree.TransferBeneficiaryDetails{
				BeneficiaryID: beneID,
			},
		})
	}

	req := cashfree.BatchTransferRequest{
		BatchTransferID: batchTransferID,
		Transfers:       transferEntries,
	}

	// Step 3: Dispatch batch to Cashfree Transfers V2
	resp, err := d.client.RequestBatchTransfer(ctx, req)
	if err != nil {
		if errors.Is(err, cashfree.ErrPayoutAuthRejected) {
			slog.Error("CASHFREE BATCH TRANSFER AUTHENTICATION REJECTED - CONFIGURATION ERROR (NON-RETRYABLE UNTIL CONFIG FIXED)",
				"batch_id", batch.ID,
				"err", err,
			)
			// Non-retryable configuration error: leave batch in approved/pending_dispatch, do not alter batch or ledger status
			return err
		}

		if errors.Is(err, cashfree.ErrDispatchUnknown) {
			slog.Error("CASHFREE BATCH TRANSFER DISPATCH AMBIGUOUS - TRANSITIONING TO dispatch_unknown",
				"batch_id", batch.ID,
				"err", err,
			)
			_ = d.repo.SetBatchDispatchUnknown(ctx, batch.ID, err.Error())
			return err
		}

		if errors.Is(err, cashfree.ErrBeneficiaryNotFound) {
			// One retry after re-registering beneficiaries with bounded concurrency
			var rg errgroup.Group
			rg.SetLimit(8)
			for _, payee := range uniquePayees {
				p := payee
				beneID := cashfree.DeriveBeneficiaryID(p.ID, p.AccountNumberHash)
				rg.Go(func() error {
					return d.ensureBeneficiary(ctx, p, beneID)
				})
			}
			if err := rg.Wait(); err != nil {
				return fmt.Errorf("payout dispatcher: refusing transfer retry: beneficiary validation failed: %w", err)
			}

			retryResp, retryErr := d.client.RequestBatchTransfer(ctx, req)
			if retryErr != nil {
				if errors.Is(retryErr, cashfree.ErrDispatchUnknown) {
					_ = d.repo.SetBatchDispatchUnknown(ctx, batch.ID, retryErr.Error())
				}
				return fmt.Errorf("payout dispatcher: retry batch transfer after bene register: %w", retryErr)
			}
			resp = retryResp
		} else {
			if strings.Contains(strings.ToLower(err.Error()), "already exists") || strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				if statusResp, sErr := d.client.GetBatchTransferStatus(ctx, batchTransferID); sErr == nil && statusResp != nil && statusResp.CFBatchTransferID != "" {
					return d.reconcileBatchStatus(ctx, batch, items, statusResp)
				}
			}
			return fmt.Errorf("payout dispatcher: request batch transfer: %w", err)
		}
	}

	slog.Info("cashfree batch transfer successfully received",
		"batch_id", batch.ID,
		"batch_transfer_id", batchTransferID,
		"cf_batch_id", resp.CFBatchTransferID,
		"status", resp.Status,
	)

	return nil
}

// StaleBatchLister lists in-flight payout batches that have not progressed recently.
type StaleBatchLister interface {
	ListStaleInFlightBatchIDs(ctx context.Context, olderThan time.Time, limit int) ([]uuid.UUID, error)
}

// PollBatch refreshes an in-flight batch from Cashfree's batch status endpoint and reconciles items.
// It NEVER creates a new transfer: on any lookup failure it returns an error and leaves state as is.
func (d *PayoutDispatcher) PollBatch(ctx context.Context, batchID uuid.UUID) error {
	if d.client == nil {
		return fmt.Errorf("payout client not configured")
	}
	acquired, unlock, err := d.repo.TryLockBatchDispatch(ctx, batchID)
	if err != nil {
		return fmt.Errorf("payout poll: lock batch %s: %w", batchID, err)
	}
	if !acquired {
		return nil
	}
	defer unlock()

	batch, err := d.repo.GetBatchByID(ctx, batchID)
	if err != nil || batch == nil {
		return fmt.Errorf("payout poll: get batch %s: %w", batchID, err)
	}
	if batch.Status != domain.BatchProcessing && batch.Status != domain.BatchDispatchUnknown {
		return nil
	}
	items, err := d.repo.ListPayoutItemsByBatch(ctx, batchID)
	if err != nil {
		return fmt.Errorf("payout poll: list items for batch %s: %w", batchID, err)
	}
	statusResp, err := d.client.GetBatchTransferStatus(ctx, fmt.Sprintf("pgo_%s", strings.ReplaceAll(batch.ID.String(), "-", "")))
	if err != nil {
		return fmt.Errorf("payout poll: batch status %s: %w", batchID, err)
	}
	if statusResp == nil || statusResp.CFBatchTransferID == "" {
		return fmt.Errorf("payout poll: batch %s unknown at gateway; manual review required", batchID)
	}
	return d.reconcileBatchStatus(ctx, batch, items, statusResp)
}

// PollStaleBatches polls every in-flight batch older than staleAfter. Failures on one batch do not
// stop the others; the first error is returned after all are attempted.
func (d *PayoutDispatcher) PollStaleBatches(ctx context.Context, lister StaleBatchLister, staleAfter time.Duration, limit int) (int, error) {
	ids, err := lister.ListStaleInFlightBatchIDs(ctx, time.Now().UTC().Add(-staleAfter), limit)
	if err != nil {
		return 0, err
	}
	var firstErr error
	polled := 0
	for _, id := range ids {
		if err := d.PollBatch(ctx, id); err != nil {
			slog.Error("payout poll failed", "batch_id", id, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		polled++
	}
	return polled, firstErr
}

func (d *PayoutDispatcher) ensureBeneficiary(ctx context.Context, payee *domain.PayoutPayee, beneID string) error {
	req := cashfree.CreateBeneficiaryRequest{
		BeneficiaryID:   beneID,
		BeneficiaryName: payee.Name,
	}
	if payee.Phone != nil {
		req.BeneficiaryContact = &cashfree.BeneficiaryContactDetails{
			BeneficiaryPhone: *payee.Phone,
		}
	}
	if payee.UPIVPA != nil && strings.TrimSpace(*payee.UPIVPA) != "" {
		req.BeneficiaryInstrument = cashfree.BeneficiaryInstrumentDetails{
			VPA: strings.TrimSpace(*payee.UPIVPA),
		}
	} else if len(payee.AccountNumberEncrypted) > 0 && payee.IFSC != nil {
		accountNum := string(payee.AccountNumberEncrypted)
		if d.keyRing != nil {
			decrypted, _, err := crypto.DecryptWithKeyRing(d.keyRing, payee.AccountNumberEncrypted)
			if err != nil {
				return fmt.Errorf("payee %s: decrypt account number failed: %w", payee.ID, err)
			}
			accountNum = string(decrypted)
		} else if len(d.encryptionKey) == 32 {
			decrypted, _, err := crypto.Decrypt(d.encryptionKey, payee.AccountNumberEncrypted)
			if err != nil {
				return fmt.Errorf("payee %s: decrypt account number failed: %w", payee.ID, err)
			}
			accountNum = string(decrypted)
		}
		req.BeneficiaryInstrument = cashfree.BeneficiaryInstrumentDetails{
			BankAccountNumber: accountNum,
			BankIFSC:          strings.TrimSpace(*payee.IFSC),
		}
	} else {
		return fmt.Errorf("payee %s has neither account number nor upi vpa", payee.ID)
	}

	_, err := d.client.CreateBeneficiary(ctx, req)
	return err
}

func (d *PayoutDispatcher) reconcileBatchStatus(
	ctx context.Context,
	batch *domain.PayoutBatch,
	items []domain.PayoutItem,
	statusResp *cashfree.BatchTransferStatusResponse,
) error {
	itemMap := make(map[string]domain.PayoutItem, len(items))
	for _, it := range items {
		itemMap[fmt.Sprintf("pgo_%s", strings.ReplaceAll(it.ID.String(), "-", ""))] = it
	}

	// Update items from Cashfree status
	for _, st := range statusResp.Transfers {
		it, ok := itemMap[st.TransferID]
		if !ok {
			continue
		}
		cfID := st.CFTransferID
		switch st.Status {
		case "SUCCESS":
			// Cashfree Transfers V2: Transfer is settled only when status is SUCCESS and status_code is COMPLETED.
			if !strings.EqualFold(st.StatusCode, "COMPLETED") {
				slog.Warn("payout batch reconcile: status is SUCCESS but status_code is not COMPLETED; keeping pending",
					"transfer_id", st.TransferID,
					"status", st.Status,
					"status_code", st.StatusCode,
				)
				continue
			}
			utr := ""
			if st.UTR != nil {
				utr = *st.UTR
			}
			if err := d.repo.ReconcilePayoutItemWithOutbox(ctx, batch.PropertyID, it.ID, it.AmountPaise, domain.PayoutSucceeded, &cfID, &utr, nil, nil); err != nil {
				return fmt.Errorf("reconcile payout item succeeded status: %w", err)
			}
		case "FAILED":
			reason := st.StatusDescription
			if reason == "" {
				reason = st.StatusCode
			}
			targetStatus := domain.PayoutFailed
			if st.StatusCode == "WAIT_TIME_EXCEEDED" {
				targetStatus = domain.PayoutRetriableFailed
			}
			if err := d.repo.UpdatePayoutItemStatusTx(ctx, nil, it.ID, targetStatus, &cfID, nil, nil, &reason); err != nil {
				return fmt.Errorf("update payout item failed status: %w", err)
			}
		case "APPROVAL_PENDING":
			if err := d.repo.UpdatePayoutItemStatusTx(ctx, nil, it.ID, domain.PayoutCashfreeApprovalPending, &cfID, nil, nil, nil); err != nil {
				return fmt.Errorf("update payout item approval pending status: %w", err)
			}
		case "REVERSED":
			reason := "transfer reversed by bank"
			if err := d.repo.ReconcilePayoutItemWithOutbox(ctx, batch.PropertyID, it.ID, it.AmountPaise, domain.PayoutReversed, &cfID, nil, nil, &reason); err != nil {
				return fmt.Errorf("reconcile payout item reversed status: %w", err)
			}
		}
	}

	// Recalculate batch status
	_, err := d.repo.UpdateBatchStatusFromItemsTx(ctx, nil, batch.ID)
	return err
}

// UnmarshalPayoutBatchPayload extracts batch_id from the ledger outbox payload.
func UnmarshalPayoutBatchPayload(payload []byte) (uuid.UUID, error) {
	var p struct {
		BatchID uuid.UUID `json:"batch_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return uuid.Nil, err
	}
	return p.BatchID, nil
}
