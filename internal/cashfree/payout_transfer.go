package cashfree

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// TransferBeneficiaryDetails specifies the target beneficiary ID.
type TransferBeneficiaryDetails struct {
	BeneficiaryID string `json:"beneficiary_id"`
}

// BatchTransferEntry specifies an individual payout item inside a batch transfer request.
type BatchTransferEntry struct {
	TransferID         string                     `json:"transfer_id"`
	TransferAmount     string                     `json:"transfer_amount"` // Exact 2-decimal INR string via FormatPaiseToRupees
	TransferMode       string                     `json:"transfer_mode"`   // "banktransfer" | "upi"
	FundsourceID       string                     `json:"fundsource_id"`
	TransferRemarks    string                     `json:"transfer_remarks,omitempty"` // max 70 chars
	BeneficiaryDetails TransferBeneficiaryDetails `json:"beneficiary_details"`
}

// BatchTransferRequest represents the payload for POST /payout/transfers/batch.
type BatchTransferRequest struct {
	BatchTransferID string               `json:"batch_transfer_id"`
	Transfers       []BatchTransferEntry `json:"transfers"`
}

// BatchTransferResponse represents the immediate response from POST /payout/transfers/batch.
type BatchTransferResponse struct {
	BatchTransferID   string `json:"batch_transfer_id"`
	CFBatchTransferID string `json:"cf_batch_transfer_id"`
	Status            string `json:"status"` // Usually "RECEIVED"
}

// TransferStatusItem holds the reconciliation details for a single transfer.
type TransferStatusItem struct {
	TransferID        string  `json:"transfer_id"`
	CFTransferID      string  `json:"cf_transfer_id"`
	Status            string  `json:"status"`      // "SUCCESS", "PENDING", "FAILED", "APPROVAL_PENDING", "REVERSED", "REJECTED"
	StatusCode        string  `json:"status_code"` // e.g. "WAIT_TIME_EXCEEDED", "BENE_BANK_DECLINED"
	StatusDescription string  `json:"status_description"`
	UTR               *string `json:"utr,omitempty"`
	AcknowledgedAt    *string `json:"acknowledged_at,omitempty"`
}

// BatchTransferStatusResponse represents the response from GET /payout/transfers/batch.
type BatchTransferStatusResponse struct {
	BatchTransferID   string               `json:"batch_transfer_id"`
	CFBatchTransferID string               `json:"cf_batch_transfer_id"`
	Status            string               `json:"status"` // "RECEIVED", "SUCCESS", "PARTIALLY_FAILED", "FAILED"
	Transfers         []TransferStatusItem `json:"transfers"`
}

// RequestBatchTransfer dispatches a batch of payouts via POST /payout/transfers/batch.
//
// Invariant (5xx / Timeout Ambiguity):
// Per Cashfree documentation, on 5xx or transport-level errors, DO NOT initiate another transaction.
// When an ambiguous error occurs, RequestBatchTransfer returns ErrDispatchUnknown, directing the caller
// to transition the batch into 'dispatch_unknown' and mandate a GetBatchTransferStatus query.
func (c *PayoutClient) RequestBatchTransfer(ctx context.Context, req BatchTransferRequest) (*BatchTransferResponse, error) {
	if !c.cfg.Enabled() {
		return nil, ErrPayoutNotConfigured
	}
	if req.BatchTransferID == "" {
		return nil, fmt.Errorf("cashfree payout: batch_transfer_id required")
	}
	if len(req.Transfers) == 0 {
		return nil, fmt.Errorf("cashfree payout: transfers array cannot be empty")
	}

	// Enforcement of Cashfree API chunking limits: 1,000 for sandbox, 5,000 for production
	limit := 1000
	if strings.EqualFold(c.cfg.Env, "production") {
		limit = 5000
	}
	if len(req.Transfers) > limit {
		return nil, fmt.Errorf("cashfree payout: batch transfers count %d exceeds environment limit %d", len(req.Transfers), limit)
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: marshal batch transfer: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint()+"/transfers/batch", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: create batch transfer request: %w", err)
	}
	c.sign(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// Transport failure (network drop, connection reset, timeout) -> Ambiguous!
		return nil, fmt.Errorf("%w: network transport failure: %v", ErrDispatchUnknown, err)
	}
	defer resp.Body.Close()
	c.checkDeprecation(resp)

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: failed reading response body: %v", ErrDispatchUnknown, err)
	}

	// 5xx response from Cashfree -> Ambiguous!
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: Cashfree server returned HTTP %d: %s", ErrDispatchUnknown, resp.StatusCode, string(bodyBytes))
	}

	if resp.StatusCode == http.StatusNotFound && strings.Contains(strings.ToLower(string(bodyBytes)), "beneficiary") {
		return nil, fmt.Errorf("%w: %s", ErrBeneficiaryNotFound, string(bodyBytes))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cashfree payout: batch transfer failed (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var out BatchTransferResponse
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return nil, fmt.Errorf("cashfree payout: unmarshal batch transfer response: %w", err)
	}
	return &out, nil
}

// GetBatchTransferStatus queries Cashfree for batch transfer status via GET /payout/transfers/batch.
func (c *PayoutClient) GetBatchTransferStatus(ctx context.Context, batchTransferID string) (*BatchTransferStatusResponse, error) {
	if !c.cfg.Enabled() {
		return nil, ErrPayoutNotConfigured
	}
	if strings.TrimSpace(batchTransferID) == "" {
		return nil, fmt.Errorf("cashfree payout: batch_transfer_id required")
	}

	reqURL := fmt.Sprintf("%s/transfers/batch?batch_transfer_id=%s", c.endpoint(), url.QueryEscape(batchTransferID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: create batch status request: %w", err)
	}
	c.sign(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: do batch status request: %w", err)
	}
	defer resp.Body.Close()
	c.checkDeprecation(resp)

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: read batch status response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cashfree payout: get batch status failed (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var out BatchTransferStatusResponse
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return nil, fmt.Errorf("cashfree payout: unmarshal batch status response: %w", err)
	}
	return &out, nil
}

// GetTransferStatus queries status for an individual transfer via GET /payout/transfers/{transfer_id}.
func (c *PayoutClient) GetTransferStatus(ctx context.Context, transferID string) (*TransferStatusItem, error) {
	if !c.cfg.Enabled() {
		return nil, ErrPayoutNotConfigured
	}
	if strings.TrimSpace(transferID) == "" {
		return nil, fmt.Errorf("cashfree payout: transfer_id required")
	}

	reqURL := fmt.Sprintf("%s/transfers/%s", c.endpoint(), url.PathEscape(transferID))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: create transfer status request: %w", err)
	}
	c.sign(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: do transfer status request: %w", err)
	}
	defer resp.Body.Close()
	c.checkDeprecation(resp)

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: read transfer status response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cashfree payout: get transfer status failed (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var out TransferStatusItem
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return nil, fmt.Errorf("cashfree payout: unmarshal transfer status response: %w", err)
	}
	return &out, nil
}
