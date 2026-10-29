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

	"github.com/google/uuid"
)

// DeriveBeneficiaryID computes a deterministic, collision-safe beneficiary_id <= 50 chars:
// Format: "bene_" + 32-char hex UUID + "_" + 8-char account hash prefix = 46 chars.
// Alphanumeric and underscores only, compliant with Cashfree constraints.
//
// Invariant: If a payee's bank account or IFSC changes, AccountNumberHash changes,
// which automatically derives a distinct beneficiary_id. This ensures that bank detail
// modifications bypass stale beneficiary records without requiring manual deletion.
func DeriveBeneficiaryID(payeeID uuid.UUID, accountNumberHash string) string {
	cleanUUID := strings.ReplaceAll(payeeID.String(), "-", "")
	hashPrefix := accountNumberHash
	if len(hashPrefix) > 8 {
		hashPrefix = hashPrefix[:8]
	}
	return fmt.Sprintf("bene_%s_%s", cleanUUID, hashPrefix)
}

// BeneficiaryInstrumentDetails holds bank account or UPI details for Beneficiary V2.
type BeneficiaryInstrumentDetails struct {
	BankAccountNumber string `json:"bank_account_number,omitempty"`
	BankIFSC          string `json:"bank_ifsc,omitempty"`
	VPA               string `json:"vpa,omitempty"`
}

// BeneficiaryContactDetails holds optional contact metadata.
type BeneficiaryContactDetails struct {
	BeneficiaryEmail string `json:"beneficiary_email,omitempty"`
	BeneficiaryPhone string `json:"beneficiary_phone,omitempty"`
}

// CreateBeneficiaryRequest represents the body for POST /payout/beneficiary.
type CreateBeneficiaryRequest struct {
	BeneficiaryID         string                       `json:"beneficiary_id"`
	BeneficiaryName       string                       `json:"beneficiary_name"`
	BeneficiaryInstrument BeneficiaryInstrumentDetails `json:"beneficiary_instrument_details"`
	BeneficiaryContact    *BeneficiaryContactDetails   `json:"beneficiary_contact_details,omitempty"`
}

// BeneficiaryResponse represents the response from Beneficiary V2 endpoints.
type BeneficiaryResponse struct {
	BeneficiaryID   string                       `json:"beneficiary_id"`
	BeneficiaryName string                       `json:"beneficiary_name"`
	Status          string                       `json:"beneficiary_status"`
	Instrument      BeneficiaryInstrumentDetails `json:"beneficiary_instrument_details"`
}

// CreateBeneficiary registers a beneficiary with Cashfree Transfers V2.
// An already-existing beneficiary is accepted only after a provider read-back and exact instrument comparison.
func (c *PayoutClient) CreateBeneficiary(ctx context.Context, req CreateBeneficiaryRequest) (*BeneficiaryResponse, error) {
	if !c.cfg.Enabled() {
		return nil, ErrPayoutNotConfigured
	}
	if req.BeneficiaryID == "" {
		return nil, fmt.Errorf("cashfree payout: beneficiary_id required")
	}
	if req.BeneficiaryName == "" {
		return nil, fmt.Errorf("cashfree payout: beneficiary_name required")
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: marshal beneficiary request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint()+"/beneficiary", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: create beneficiary request: %w", err)
	}
	c.sign(httpReq)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: do beneficiary request: %w", err)
	}
	defer resp.Body.Close()
	c.checkDeprecation(resp)

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: read beneficiary response: %w", err)
	}

	if resp.StatusCode == http.StatusConflict {
		// A 409 is NOT proof that the existing record belongs to this payee.
		// Unknown conflict shapes fail closed until the provider contract is confirmed.
		var providerErr struct {
			Code string `json:"code"`
			Type string `json:"type"`
		}
		if json.Unmarshal(bodyBytes, &providerErr) != nil ||
			!strings.EqualFold(providerErr.Code, "BENEFICIARY_ALREADY_EXISTS") {
			return nil, fmt.Errorf("cashfree payout: unrecognized beneficiary conflict (HTTP 409)")
		}
		existing, err := c.GetBeneficiary(ctx, req.BeneficiaryID)
		if err != nil {
			return nil, fmt.Errorf("cashfree payout: cannot verify existing beneficiary: %w", err)
		}
		if existing.BeneficiaryID != req.BeneficiaryID || !strings.EqualFold(existing.Status, "ACTIVE") ||
			!sameBeneficiaryInstrument(existing.Instrument, req.BeneficiaryInstrument) {
			return nil, fmt.Errorf("cashfree payout: existing beneficiary differs from requested instrument or is not ACTIVE")
		}
		return existing, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cashfree payout: create beneficiary failed (HTTP %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var out BeneficiaryResponse
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return nil, fmt.Errorf("cashfree payout: unmarshal beneficiary response: %w", err)
	}
	return &out, nil
}

// GetBeneficiary retrieves the current provider state; never invent status or bank details.
func (c *PayoutClient) GetBeneficiary(ctx context.Context, beneficiaryID string) (*BeneficiaryResponse, error) {
	if !c.cfg.Enabled() {
		return nil, ErrPayoutNotConfigured
	}
	if strings.TrimSpace(beneficiaryID) == "" {
		return nil, fmt.Errorf("cashfree payout: beneficiary_id required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint()+"/beneficiary/"+url.PathEscape(beneficiaryID), nil)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: get beneficiary request: %w", err)
	}
	c.sign(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: get beneficiary: %w", err)
	}
	defer resp.Body.Close()
	c.checkDeprecation(resp)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: read beneficiary: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cashfree payout: get beneficiary HTTP %d", resp.StatusCode)
	}
	var out BeneficiaryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("cashfree payout: decode beneficiary: %w", err)
	}
	return &out, nil
}

func sameBeneficiaryInstrument(a, b BeneficiaryInstrumentDetails) bool {
	if b.VPA != "" {
		return a.BankAccountNumber == "" && a.BankIFSC == "" &&
			strings.EqualFold(strings.TrimSpace(a.VPA), strings.TrimSpace(b.VPA))
	}
	return b.BankAccountNumber != "" && b.BankIFSC != "" &&
		a.VPA == "" && strings.TrimSpace(a.BankAccountNumber) == strings.TrimSpace(b.BankAccountNumber) &&
		strings.EqualFold(strings.TrimSpace(a.BankIFSC), strings.TrimSpace(b.BankIFSC))
}
