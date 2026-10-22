package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
	encryptionKey []byte
	keyRing       *crypto.KeyRing
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

// DispatchBatch processes an outbox-triggered batch transfer against Cashfree Transfers V2.
func (d *PayoutDispatcher) DispatchBatch(ctx context.Context, batchID uuid.UUID) error {
	if d.client == nil {
		return fmt.Errorf("payout client not configured")
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

	batchTransferID := fmt.Sprintf("pgo_%s", batch.ID)

	// Step 1: Ambiguity Resolver — if batch is in dispatch_unknown, query Cashfree status first!
	if batch.Status == domain.BatchDispatchUnknown {
		statusResp, err := d.client.GetBatchTransferStatus(ctx, batchTransferID)
		if err == nil && statusResp != nil {
			slog.Info("cashfree batch transfer recovered from dispatch_unknown via status poll",
				"batch_id", batch.ID,
				"cf_batch_id", statusResp.CFBatchTransferID,
				"status", statusResp.Status,
			)
			// Successfully queried status: reconcile items and return
			return d.reconcileBatchStatus(ctx, batch, items, statusResp)
		}
		// If query fails or returns not found, proceed to normal dispatch below
	}

	// Step 2: Deduplicate payees and ensure all beneficiaries are registered with derived IDs
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

	// Concurrent bounded beneficiary registration (max 8 concurrent workers)
	var g errgroup.Group
	g.SetLimit(8)
	for _, payee := range uniquePayees {
		p := payee
		beneID := cashfree.DeriveBeneficiaryID(p.ID, p.AccountNumberHash)
		g.Go(func() error {
			if bErr := d.ensureBeneficiary(ctx, p, beneID); bErr != nil {
				slog.Warn("payout dispatcher: proactive beneficiary registration warning",
					"payee_id", p.ID,
					"beneficiary_id", beneID,
					"err", bErr,
				)
			}
			return nil
		})
	}
	_ = g.Wait()

	transferEntries := make([]cashfree.BatchTransferEntry, 0, len(items))
	for _, it := range items {
		payee := uniquePayees[it.PayeeID]
		beneID := cashfree.DeriveBeneficiaryID(payee.ID, payee.AccountNumberHash)

		mode := "banktransfer"
		if payee.UPIVPA != nil && strings.TrimSpace(*payee.UPIVPA) != "" {
			mode = "upi"
		}

		remarks := strings.TrimSpace(fmt.Sprintf("%s %s", it.Purpose, it.ReferenceNumber))
		if len(remarks) > 70 {
			remarks = remarks[:70]
		}

		transferEntries = append(transferEntries, cashfree.BatchTransferEntry{
			TransferID:      fmt.Sprintf("pgo_%s", it.ID),
			TransferAmount:  json.Number(cashfree.FormatPaiseToRupees(it.AmountPaise)),
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
					_ = d.ensureBeneficiary(ctx, p, beneID)
					return nil
				})
			}
			_ = rg.Wait()

			retryResp, retryErr := d.client.RequestBatchTransfer(ctx, req)
			if retryErr != nil {
				return fmt.Errorf("payout dispatcher: retry batch transfer after bene register: %w", retryErr)
			}
			resp = retryResp
		} else {
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
		itemMap[fmt.Sprintf("pgo_%s", it.ID)] = it
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
			utr := ""
			if st.UTR != nil {
				utr = *st.UTR
			}
			if err := d.repo.UpdatePayoutItemStatusTx(ctx, nil, it.ID, domain.PayoutSucceeded, &cfID, &utr, nil, nil); err != nil {
				return fmt.Errorf("update payout item succeeded status: %w", err)
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
			if err := d.repo.UpdatePayoutItemStatusTx(ctx, nil, it.ID, domain.PayoutReversed, &cfID, nil, nil, &reason); err != nil {
				return fmt.Errorf("update payout item reversed status: %w", err)
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
