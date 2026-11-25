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
	repo            *postgres.PayoutRepo
	client          *cashfree.PayoutClient
	fundsourceID    string
	encryptionKey   []byte
	keyRing         *crypto.KeyRing
	approvalKeyRing *crypto.ApprovalKeyRing
	alerter         PayoutProtectAlerter
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

// SetApprovalKeyRing sets the unified versioned ApprovalKeyRing for verifying approved batch snapshots.
func (d *PayoutDispatcher) SetApprovalKeyRing(kr *crypto.ApprovalKeyRing) {
	d.approvalKeyRing = kr
}

// SetAlerter configures the notification alerter for stuck or pending Cashfree approvals.
func (d *PayoutDispatcher) SetAlerter(a PayoutProtectAlerter) {
	d.alerter = a
}

// WithRepo returns a copy of PayoutDispatcher configured with an alternative PayoutRepo (e.g. maintenance pool for background jobs).
func (d *PayoutDispatcher) WithRepo(repo *postgres.PayoutRepo) *PayoutDispatcher {
	return &PayoutDispatcher{
		repo:            repo,
		client:          d.client,
		fundsourceID:    d.fundsourceID,
		encryptionKey:   d.encryptionKey,
		keyRing:         d.keyRing,
		approvalKeyRing: d.approvalKeyRing,
		alerter:         d.alerter,
	}
}

// DispatchBatch processes an outbox-triggered batch transfer against Cashfree Transfers V2.
func (d *PayoutDispatcher) DispatchBatch(ctx context.Context, batchID uuid.UUID) error {
	if d.client == nil {
		return fmt.Errorf("payout client not configured")
	}
	if d.repo == nil {
		return fmt.Errorf("payout dispatcher: repository not configured")
	}

	lease, acquired, lockErr := d.repo.TryLockBatchDispatch(ctx, batchID)
	if lockErr != nil {
		return fmt.Errorf("payout dispatcher: lock batch %s: %w", batchID, lockErr)
	}
	if !acquired {
		slog.Info("payout dispatcher: batch already being dispatched by another worker, skipping", "batch_id", batchID)
		return nil
	}
	defer lease.Unlock()
	leaseCtx := lease.Ctx

	batch, err := d.repo.GetBatchByID(leaseCtx, batchID)
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
	if batch.ItemCount > domain.MaxPayoutBatchItems {
		return fmt.Errorf("%w: batch %s item_count=%d exceeds %d", domain.ErrBatchItemLimitExceeded, batch.ID, batch.ItemCount, domain.MaxPayoutBatchItems)
	}

	items, err := d.repo.ListPayoutItemsByBatch(leaseCtx, batchID)
	if err != nil {
		return fmt.Errorf("payout dispatcher: list items for batch %s: %w", batchID, err)
	}
	if len(items) == 0 {
		return fmt.Errorf("payout dispatcher: batch %s has no items", batchID)
	}
	if len(items) > domain.MaxPayoutBatchItems {
		return fmt.Errorf("%w: batch %s fetched %d items exceeding %d", domain.ErrBatchItemLimitExceeded, batch.ID, len(items), domain.MaxPayoutBatchItems)
	}

	batchTransferID := fmt.Sprintf("pgo_%s", strings.ReplaceAll(batch.ID.String(), "-", ""))

	// Verification Gate: Verify immutable approval snapshot and HMAC signature before outbound dispatch
	if d.approvalKeyRing == nil {
		return errors.New("payout dispatcher: approval signing keys not configured; refusing to dispatch unverified batch")
	}
	verificationKeys := d.approvalKeyRing.VerificationKeys()
	if len(verificationKeys) == 0 {
		return errors.New("payout dispatcher: approval signing keys not configured; refusing to dispatch unverified batch")
	}
	snapshot, err := d.repo.VerifyBatchApproval(leaseCtx, batchID, verificationKeys)
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
		payee, err := d.repo.GetPayeeByID(leaseCtx, it.PayeeID)
		if err != nil {
			return fmt.Errorf("payout dispatcher: get payee %s for item %s: %w", it.PayeeID, it.ID, err)
		}
		if !payee.IsVerified || payee.VerificationStatus != domain.PayeeVerificationProviderVerified {
			return fmt.Errorf("payout dispatcher: payee %s for item %s requires provider verification (status: %s)", it.PayeeID, it.ID, payee.VerificationStatus)
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
		statusResp, err := d.client.GetBatchTransferStatus(leaseCtx, batchTransferID)
		if err == nil {
			if statusResp == nil || strings.TrimSpace(statusResp.CFBatchTransferID) == "" || strings.TrimSpace(statusResp.BatchTransferID) == "" {
				return fmt.Errorf("payout dispatcher: malformed batch status response for %s: missing required identifiers", batch.ID)
			}
			slog.Info("cashfree batch transfer recovered via status poll",
				"batch_id", batch.ID,
				"cf_batch_id", statusResp.CFBatchTransferID,
				"status", statusResp.Status,
			)
			// Successfully queried status: reconcile items and return
			return d.reconcileBatchStatusFenced(leaseCtx, lease, batch, items, statusResp)
		}

		// If the batch had an ambiguous prior dispatch (dispatch_unknown), we can only proceed
		// if the provider definitively confirms the batch does not exist (ErrBatchNotFound).
		// Any other error (network timeout, 5xx, malformed response) leaves prior execution unresolved.
		if batch.Status == domain.BatchDispatchUnknown {
			if !errors.Is(err, cashfree.ErrBatchNotFound) {
				return fmt.Errorf("payout dispatcher: batch %s has unresolved prior dispatch; reconciliation required: %w", batch.ID, err)
			}
		}

		// For batches currently in processing (e.g. freshly initiated in local DB), a 404/ErrBatchNotFound
		// is expected since the batch has not yet been dispatched to Cashfree. If the status lookup failed
		// with a non-404 error (e.g. client auth/network transport failure), fail closed before attempting dispatch.
		if batch.Status == domain.BatchProcessing && !errors.Is(err, cashfree.ErrBatchNotFound) {
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
			if err := d.ensureBeneficiary(leaseCtx, p, beneID); err != nil {
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
		remarks := SanitizePayoutRemarks(it.Purpose, it.ReferenceNumber)

		transferID := DerivePayoutTransferID(it)

		transferEntries = append(transferEntries, cashfree.BatchTransferEntry{
			TransferID:      transferID,
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

	if err := leaseCtx.Err(); err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStaleDispatchLease, err)
	}

	// Step 3: Dispatch batch to Cashfree Transfers V2
	resp, err := d.client.RequestBatchTransfer(leaseCtx, req)
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
			if setErr := d.repo.SetBatchDispatchUnknownFenced(leaseCtx, lease, err.Error()); setErr != nil {
				slog.Error("failed to persist batch dispatch_unknown status", "batch_id", batch.ID, "err", setErr)
				return fmt.Errorf("payout dispatcher: persist dispatch_unknown failed: %w (dispatch error: %v)", setErr, err)
			}
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
					return d.ensureBeneficiary(leaseCtx, p, beneID)
				})
			}
			if err := rg.Wait(); err != nil {
				return fmt.Errorf("payout dispatcher: refusing transfer retry: beneficiary validation failed: %w", err)
			}

			if err := leaseCtx.Err(); err != nil {
				return fmt.Errorf("%w: %v", domain.ErrStaleDispatchLease, err)
			}

			retryResp, retryErr := d.client.RequestBatchTransfer(leaseCtx, req)
			if retryErr != nil {
				if errors.Is(retryErr, cashfree.ErrDispatchUnknown) {
					if setErr := d.repo.SetBatchDispatchUnknownFenced(leaseCtx, lease, retryErr.Error()); setErr != nil {
						slog.Error("failed to persist retry batch dispatch_unknown status", "batch_id", batch.ID, "err", setErr)
						return fmt.Errorf("payout dispatcher: persist dispatch_unknown on retry failed: %w (retry error: %v)", setErr, retryErr)
					}
				}
				return fmt.Errorf("payout dispatcher: retry batch transfer after bene register: %w", retryErr)
			}
			resp = retryResp
		} else {
			if statusResp, sErr := d.client.GetBatchTransferStatus(leaseCtx, batchTransferID); sErr == nil && statusResp != nil && statusResp.CFBatchTransferID != "" {
				slog.Info("payout dispatcher: batch already exists at gateway on error; reconciling existing batch status",
					"batch_id", batch.ID,
					"cf_batch_id", statusResp.CFBatchTransferID,
				)
				return d.reconcileBatchStatusFenced(leaseCtx, lease, batch, items, statusResp)
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
	lease, acquired, err := d.repo.TryLockBatchDispatch(ctx, batchID)
	if err != nil {
		return fmt.Errorf("payout poll: lock batch %s: %w", batchID, err)
	}
	if !acquired {
		return nil
	}
	defer lease.Unlock()
	leaseCtx := lease.Ctx

	batch, err := d.repo.GetBatchByID(leaseCtx, batchID)
	if err != nil || batch == nil {
		return fmt.Errorf("payout poll: get batch %s: %w", batchID, err)
	}
	if batch.Status != domain.BatchProcessing && batch.Status != domain.BatchDispatchUnknown {
		return nil
	}
	items, err := d.repo.ListPayoutItemsByBatch(leaseCtx, batchID)
	if err != nil {
		return fmt.Errorf("payout poll: list items for batch %s: %w", batchID, err)
	}
	statusResp, err := d.client.GetBatchTransferStatus(leaseCtx, fmt.Sprintf("pgo_%s", strings.ReplaceAll(batch.ID.String(), "-", "")))
	if err != nil {
		return fmt.Errorf("payout poll: batch status %s: %w", batchID, err)
	}
	if statusResp == nil || statusResp.CFBatchTransferID == "" {
		return fmt.Errorf("payout poll: batch %s unknown at gateway; manual review required", batchID)
	}
	return d.reconcileBatchStatusFenced(leaseCtx, lease, batch, items, statusResp)
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
		if cleanPhone := CleanBeneficiaryPhone(*payee.Phone); cleanPhone != "" {
			req.BeneficiaryContact = &cashfree.BeneficiaryContactDetails{
				BeneficiaryPhone: cleanPhone,
			}
		}
	}
	if payee.UPIVPA != nil && strings.TrimSpace(*payee.UPIVPA) != "" {
		req.BeneficiaryInstrument = cashfree.BeneficiaryInstrumentDetails{
			VPA: strings.TrimSpace(*payee.UPIVPA),
		}
	} else if len(payee.AccountNumberEncrypted) > 0 && payee.IFSC != nil {
		var decrypted []byte
		var err error
		if d.keyRing != nil {
			decrypted, _, err = crypto.DecryptWithKeyRing(d.keyRing, payee.AccountNumberEncrypted)
			if err != nil {
				return fmt.Errorf("payee %s: decrypt account number failed: %w", payee.ID, err)
			}
		} else if len(d.encryptionKey) == 32 {
			decrypted, _, err = crypto.Decrypt(d.encryptionKey, payee.AccountNumberEncrypted)
			if err != nil {
				return fmt.Errorf("payee %s: decrypt account number failed: %w", payee.ID, err)
			}
		} else {
			return fmt.Errorf("payee %s: encryption key or key ring unavailable to decrypt bank account", payee.ID)
		}
		accountNum := string(decrypted)
		req.BeneficiaryInstrument = cashfree.BeneficiaryInstrumentDetails{
			BankAccountNumber: accountNum,
			BankIFSC:          strings.TrimSpace(*payee.IFSC),
		}
	} else {
		return fmt.Errorf("payee %s has neither account number nor upi vpa", payee.ID)
	}

	beneResp, err := d.client.CreateBeneficiary(ctx, req)
	if err != nil {
		return err
	}
	return cashfree.ValidateBeneficiaryReady(req.BeneficiaryID, req.BeneficiaryInstrument, beneResp)
}

func (d *PayoutDispatcher) reconcileBatchStatus(
	ctx context.Context,
	batch *domain.PayoutBatch,
	items []domain.PayoutItem,
	statusResp *cashfree.BatchTransferStatusResponse,
) error {
	return d.reconcileBatchStatusFenced(ctx, nil, batch, items, statusResp)
}

func (d *PayoutDispatcher) reconcileBatchStatusFenced(
	ctx context.Context,
	lease *postgres.PayoutDispatchLease,
	batch *domain.PayoutBatch,
	items []domain.PayoutItem,
	statusResp *cashfree.BatchTransferStatusResponse,
) error {
	expectedTransferMap := make(map[string]domain.PayoutItem, len(items))
	for _, it := range items {
		expectedID := DerivePayoutTransferID(it)
		expectedTransferMap[expectedID] = it
	}

	// PASS 1: Whole-response validation. Count transfer IDs and detect incidents across the full batch payload.
	idCounts := make(map[string]int, len(statusResp.Transfers))
	var duplicateIDs []string
	var unknownIDs []string

	for _, st := range statusResp.Transfers {
		incomingID := st.TransferID
		if incomingID == "" {
			slog.Error("payout batch reconcile: empty transfer_id in gateway batch status",
				"batch_id", batch.ID, "status", st.Status)
			continue
		}
		idCounts[incomingID]++
		if idCounts[incomingID] == 2 {
			duplicateIDs = append(duplicateIDs, incomingID)
		}
		if _, ok := expectedTransferMap[incomingID]; !ok && idCounts[incomingID] == 1 {
			unknownIDs = append(unknownIDs, incomingID)
		}
	}

	// Persist durable reconciliation incidents before any item mutation
	if len(duplicateIDs) > 0 {
		dupMsg := fmt.Sprintf("duplicate transfer IDs in gateway batch status: %s", strings.Join(duplicateIDs, ", "))
		slog.ErrorContext(ctx, "payout batch reconcile: duplicate transfer IDs detected; refusing mutation of affected items",
			"batch_id", batch.ID,
			"duplicate_ids", duplicateIDs,
		)
		if d.repo != nil {
			if recErr := d.recordBatchIncident(ctx, lease, batch.ID, "DUPLICATE_TRANSFER_ID", dupMsg); recErr != nil {
				slog.ErrorContext(ctx, "payout batch reconcile: failed to persist duplicate transfer ID incident",
					"batch_id", batch.ID,
					"err", recErr,
				)
				return fmt.Errorf("payout batch reconcile: persist duplicate transfer ID incident: %w", recErr)
			}
		}
	}

	if len(unknownIDs) > 0 {
		unknownMsg := fmt.Sprintf("unrecognized transfer IDs in gateway batch status: %s", strings.Join(unknownIDs, ", "))
		slog.ErrorContext(ctx, "payout batch reconcile: unknown transfer IDs in provider response; refusing mutation",
			"batch_id", batch.ID,
			"unknown_ids", unknownIDs,
		)
		if d.repo != nil {
			if recErr := d.recordBatchIncident(ctx, lease, batch.ID, "UNKNOWN_TRANSFER_ID", unknownMsg); recErr != nil {
				slog.ErrorContext(ctx, "payout batch reconcile: failed to persist unknown transfer ID incident",
					"batch_id", batch.ID,
					"err", recErr,
				)
				return fmt.Errorf("payout batch reconcile: persist unknown transfer ID incident: %w", recErr)
			}
		}
	}

	// Build incident filter: items with duplicate or unknown IDs are blocked from state mutation
	incidentIDs := make(map[string]struct{}, len(duplicateIDs)+len(unknownIDs))
	for _, id := range duplicateIDs {
		incidentIDs[id] = struct{}{}
	}
	for _, id := range unknownIDs {
		incidentIDs[id] = struct{}{}
	}

	// PASS 2: Process ONLY unique, validated items whose IDs are free of incident conflicts
	seen := make(map[string]struct{}, len(expectedTransferMap))
	for _, st := range statusResp.Transfers {
		incomingID := st.TransferID
		if incomingID == "" {
			continue
		}
		if _, isIncident := incidentIDs[incomingID]; isIncident {
			continue
		}
		it, ok := expectedTransferMap[incomingID]
		if !ok {
			continue
		}

		if err := d.reconcileSingleTransferFenced(ctx, lease, batch, it, &st); err != nil {
			return err
		}
		seen[incomingID] = struct{}{}
	}

	// 2. Identify all locally unresolved transfers not present in batch response.
	// Query Cashfree using individual transfer IDs ONLY if batch status is explicitly PROCESSED.
	// Per Cashfree Payouts V2 contract, individual transfers in a batch must only be queried
	// after the batch status reaches PROCESSED (or terminal completion).
	// If the batch status is RECEIVED, PENDING, or unrecognised, keep unresolved items in-flight
	// and poll the batch endpoint again on subsequent intervals.
	batchStatus := strings.ToUpper(strings.TrimSpace(statusResp.Status))
	isBatchProcessed := batchStatus == "PROCESSED" || batchStatus == "SUCCESS" || batchStatus == "PARTIALLY_FAILED" || batchStatus == "FAILED"

	if isBatchProcessed {
		for expectedID, it := range expectedTransferMap {
			if _, ok := seen[expectedID]; ok {
				continue
			}
			if it.Status != domain.PayoutProcessing && it.Status != domain.PayoutPending {
				continue
			}
			if d.client == nil {
				continue
			}
			transferID := DerivePayoutTransferID(it)
			itemStatus, err := d.client.GetTransferStatus(ctx, transferID)
			if err == nil && itemStatus != nil {
				if err := d.reconcileSingleTransferFenced(ctx, lease, batch, it, itemStatus); err != nil {
					return err
				}
			} else {
				// Do NOT mark as failed on empty response or lookup failure.
				// Retain in reconciliation (in-flight) so background poller continues until authoritative.
				slog.Warn("payout batch reconcile: individual transfer status lookup inconclusive; retaining in-flight",
					"batch_id", batch.ID,
					"transfer_id", transferID,
					"item_id", it.ID,
					"err", err,
				)
			}
		}
	} else {
		slog.Info("payout batch reconcile: batch not yet processed; deferring individual transfer queries",
			"batch_id", batch.ID,
			"batch_status", statusResp.Status,
		)
	}

	// 3. Derive batch status after all available item-level results have been applied.
	if d.repo != nil {
		if lease != nil {
			_, err := d.repo.UpdateBatchStatusFromItemsFenced(ctx, lease)
			return err
		}
		_, err := d.repo.UpdateBatchStatusFromItemsTx(ctx, nil, batch.ID)
		return err
	}
	return nil
}

func (d *PayoutDispatcher) reconcileSingleTransfer(
	ctx context.Context,
	batch *domain.PayoutBatch,
	it domain.PayoutItem,
	st *cashfree.TransferStatusItem,
) error {
	return d.reconcileSingleTransferFenced(ctx, nil, batch, it, st)
}

func (d *PayoutDispatcher) reconcileSingleTransferFenced(
	ctx context.Context,
	lease *postgres.PayoutDispatchLease,
	batch *domain.PayoutBatch,
	it domain.PayoutItem,
	st *cashfree.TransferStatusItem,
) error {
	expectedTransferID := DerivePayoutTransferID(it)
	if st.TransferID != expectedTransferID {
		slog.Error("payout batch reconcile: missing or mismatched transfer ID; refusing state mutation",
			"expected", expectedTransferID,
			"got", st.TransferID,
			"item_id", it.ID,
			"status", st.Status,
		)
		if d.repo != nil {
			incidentType := "TRANSFER_ID_MISMATCH"
			if st.TransferID == "" {
				incidentType = "EMPTY_TRANSFER_ID"
			}
			msg := fmt.Sprintf("expected transfer ID %q, got %q (status=%s)", expectedTransferID, st.TransferID, st.Status)
			if err := d.recordItemIncident(ctx, lease, batch.ID, it.ID, incidentType, expectedTransferID, st.TransferID, msg); err != nil {
				return fmt.Errorf("payout batch reconcile: persist item incident: %w", err)
			}
		}
		return nil
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
			return nil
		}
		if strings.TrimSpace(st.TransferAmount.String()) == "" {
			slog.Error("payout batch reconcile: missing transfer amount; refusing settlement",
				"item_id", it.ID, "expected_paise", it.AmountPaise)
			return nil
		}
		providerAmountPaise, err := cashfree.ParseRupeesToPaise(st.TransferAmount.String())
		if err != nil || providerAmountPaise != it.AmountPaise {
			slog.Error("payout batch reconcile: amount mismatch; refusing settlement",
				"item_id", it.ID, "expected_paise", it.AmountPaise,
				"provider_amount", st.TransferAmount.String(), "err", err)
			return nil
		}
		utr := ""
		if st.UTR != nil {
			utr = *st.UTR
		}
		if err := d.reconcileItemWithOutbox(ctx, lease, batch.PropertyID, it.ID, it.AmountPaise, domain.PayoutSucceeded, &cfID, &utr, nil, nil); err != nil {
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
		if err := d.updateItemStatus(ctx, lease, it.ID, targetStatus, &cfID, nil, nil, &reason); err != nil {
			return fmt.Errorf("update payout item failed status: %w", err)
		}
	case "APPROVAL_PENDING":
		if err := d.updateItemStatus(ctx, lease, it.ID, domain.PayoutCashfreeApprovalPending, &cfID, nil, nil, nil); err != nil {
			return fmt.Errorf("update payout item approval pending status: %w", err)
		}
		if d.alerter != nil {
			_ = d.alerter.NotifyPayoutProtectAlert(ctx, it.ID, batch.PropertyID, it.AmountPaise, cfID)
		}
	case "PENDING", "RECEIVED", "PROCESSING":
		// Cashfree Transfers V2: In-flight states. No local state mutation; await final settlement or reversal.
		slog.Debug("payout batch reconcile: transfer in flight at gateway",
			"transfer_id", st.TransferID,
			"status", st.Status,
			"status_code", st.StatusCode,
		)
		return nil
	case "REVERSED":
		reason := "transfer reversed by bank"
		if err := d.reconcileItemWithOutbox(ctx, lease, batch.PropertyID, it.ID, it.AmountPaise, domain.PayoutReversed, &cfID, nil, nil, &reason); err != nil {
			return fmt.Errorf("reconcile payout item reversed status: %w", err)
		}
	case "REJECTED":
		reason := st.StatusDescription
		if reason == "" {
			reason = st.StatusCode
		}
		if reason == "" {
			reason = "transfer rejected by gateway"
		}
		if err := d.updateItemStatus(ctx, lease, it.ID, domain.PayoutRejected, &cfID, nil, nil, &reason); err != nil {
			return fmt.Errorf("update payout item rejected status: %w", err)
		}
	default:
		slog.Warn("payout batch reconcile: unhandled transfer status from gateway",
			"transfer_id", st.TransferID,
			"status", st.Status,
			"status_code", st.StatusCode,
		)
	}
	return nil
}

func (d *PayoutDispatcher) reconcileItemWithOutbox(
	ctx context.Context,
	lease *postgres.PayoutDispatchLease,
	propertyID, itemID uuid.UUID,
	amountPaise int64,
	status domain.PayoutItemStatus,
	cfTransferID, utr *string,
	settledAt *time.Time,
	failureReason *string,
) error {
	if lease != nil {
		return d.repo.ReconcilePayoutItemWithOutboxFenced(ctx, lease, propertyID, itemID, amountPaise, status, cfTransferID, utr, settledAt, failureReason)
	}
	return d.repo.ReconcilePayoutItemWithOutbox(ctx, propertyID, itemID, amountPaise, status, cfTransferID, utr, settledAt, failureReason)
}

func (d *PayoutDispatcher) updateItemStatus(
	ctx context.Context,
	lease *postgres.PayoutDispatchLease,
	itemID uuid.UUID,
	status domain.PayoutItemStatus,
	cfTransferID, utr *string,
	settledAt *time.Time,
	failureReason *string,
) error {
	if lease != nil {
		return d.repo.UpdatePayoutItemStatusFenced(ctx, lease, itemID, status, cfTransferID, utr, settledAt, failureReason)
	}
	return d.repo.UpdatePayoutItemStatusTx(ctx, nil, itemID, status, cfTransferID, utr, settledAt, failureReason)
}

func (d *PayoutDispatcher) recordBatchIncident(
	ctx context.Context,
	lease *postgres.PayoutDispatchLease,
	batchID uuid.UUID,
	incidentType, details string,
) error {
	if lease != nil {
		return d.repo.RecordBatchIncidentFenced(ctx, lease, incidentType, details)
	}
	return d.repo.RecordBatchIncident(ctx, batchID, incidentType, details)
}

func (d *PayoutDispatcher) recordItemIncident(
	ctx context.Context,
	lease *postgres.PayoutDispatchLease,
	batchID, itemID uuid.UUID,
	incidentType, expectedTransferID, receivedTransferID, details string,
) error {
	if lease != nil {
		return d.repo.RecordItemIncidentFenced(ctx, lease, itemID, incidentType, expectedTransferID, receivedTransferID, details)
	}
	return d.repo.RecordItemIncident(ctx, batchID, itemID, incidentType, expectedTransferID, receivedTransferID, details)
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

// SanitizePayoutRemarks enforces Cashfree Payouts V2 remark constraints:
// allowed characters: numbers, alphabets and whitespaces (^[a-zA-Z0-9 ]{1,70}$).
// Disallowed characters (such as '-', '_', '@', '.') are converted to spaces.
// Whitespace is collapsed and trimmed, and result is truncated to at most 70 runes.
func SanitizePayoutRemarks(purpose, refNum string) string {
	raw := strings.TrimSpace(fmt.Sprintf("%s %s", purpose, refNum))
	var b strings.Builder
	b.Grow(len(raw))
	lastWasSpace := false

	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastWasSpace = false
		} else if !lastWasSpace && b.Len() > 0 {
			b.WriteByte(' ')
			lastWasSpace = true
		}
	}

	result := strings.TrimSpace(b.String())
	runes := []rune(result)
	if len(runes) > 70 {
		result = strings.TrimSpace(string(runes[:70]))
	}
	if result == "" {
		return "Payout"
	}
	return result
}

// CleanBeneficiaryPhone standardizes Indian phone numbers into clean 10-digit format for Cashfree Beneficiary API.
// It removes "+91", leading "0", and non-digit characters. If the result is not 10 digits, it returns "".
func CleanBeneficiaryPhone(p string) string {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return ""
	}
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if len(d) == 11 && strings.HasPrefix(d, "0") {
		d = d[1:]
	} else if len(d) == 12 && strings.HasPrefix(d, "91") {
		d = d[2:]
	}
	if len(d) == 10 {
		return d
	}
	return ""
}

// DerivePayoutTransferID generates the canonical merchant transfer ID for a payout item.
func DerivePayoutTransferID(it domain.PayoutItem) string {
	cleanID := strings.ReplaceAll(it.ID.String(), "-", "")
	if it.RetryOf != nil {
		return fmt.Sprintf("pgo_%s_a2", cleanID)
	}
	return fmt.Sprintf("pgo_%s", cleanID)
}
