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
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// PayoutBatchDispatcher defines the worker interface for dispatching approved payout batches to Cashfree.
type PayoutBatchDispatcher interface {
	DispatchBatch(ctx context.Context, batchID uuid.UUID) error
}

// PayoutDispatcher handles the Cashfree Transfers V2 batch transfer lifecycle.
type PayoutDispatcher struct {
	repo         *postgres.PayoutRepo
	client       *cashfree.PayoutClient
	fundsourceID string
}

// NewPayoutDispatcher creates a new PayoutDispatcher.
func NewPayoutDispatcher(repo *postgres.PayoutRepo, client *cashfree.PayoutClient, fundsourceID string) *PayoutDispatcher {
	return &PayoutDispatcher{
		repo:         repo,
		client:       client,
		fundsourceID: fundsourceID,
	}
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

	// Step 2: Ensure all beneficiaries are registered with derived IDs (hash-based cache invalidation)
	transferEntries := make([]cashfree.BatchTransferEntry, 0, len(items))
	for _, it := range items {
		payee, err := d.repo.GetPayeeByID(ctx, it.PayeeID)
		if err != nil {
			return fmt.Errorf("payout dispatcher: get payee %s for item %s: %w", it.PayeeID, it.ID, err)
		}

		beneID := cashfree.DeriveBeneficiaryID(payee.ID, payee.AccountNumberHash)
		if err := d.ensureBeneficiary(ctx, payee, beneID); err != nil {
			slog.Warn("payout dispatcher: proactive beneficiary registration warning",
				"payee_id", payee.ID,
				"beneficiary_id", beneID,
				"err", err,
			)
		}

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
			TransferAmount:  cashfree.FormatPaiseToRupees(it.AmountPaise),
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
			// One retry after re-registering beneficiaries
			for _, it := range items {
				if payee, pErr := d.repo.GetPayeeByID(ctx, it.PayeeID); pErr == nil {
					beneID := cashfree.DeriveBeneficiaryID(payee.ID, payee.AccountNumberHash)
					_ = d.ensureBeneficiary(ctx, payee, beneID)
				}
			}
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
		req.BeneficiaryInstrument = cashfree.BeneficiaryInstrumentDetails{
			BankAccountNumber: string(payee.AccountNumberEncrypted),
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
			_ = d.repo.UpdatePayoutItemStatusTx(ctx, nil, it.ID, domain.PayoutSucceeded, &cfID, &utr, nil, nil)
		case "FAILED":
			reason := st.StatusDescription
			if reason == "" {
				reason = st.StatusCode
			}
			targetStatus := domain.PayoutFailed
			if st.StatusCode == "WAIT_TIME_EXCEEDED" {
				targetStatus = domain.PayoutRetriableFailed
			}
			_ = d.repo.UpdatePayoutItemStatusTx(ctx, nil, it.ID, targetStatus, &cfID, nil, nil, &reason)
		case "APPROVAL_PENDING":
			_ = d.repo.UpdatePayoutItemStatusTx(ctx, nil, it.ID, domain.PayoutCashfreeApprovalPending, &cfID, nil, nil, nil)
		case "REVERSED":
			reason := "transfer reversed by bank"
			_ = d.repo.UpdatePayoutItemStatusTx(ctx, nil, it.ID, domain.PayoutReversed, &cfID, nil, nil, &reason)
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
