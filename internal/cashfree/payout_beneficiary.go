package cashfree

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	if err := c.sign(httpReq); err != nil {
		return nil, err
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: do beneficiary request: %w", err)
	}
	defer resp.Body.Close()
	c.checkDeprecation(resp)

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: read beneficiary response: %w", err)
	}

	if resp.StatusCode == http.StatusConflict {
		// A 409 is NOT proof that the existing record belongs to this payee.
		// Cashfree V2 returns beneficiary_already_exists or beneficiary_id_already_exists.
		// Unknown conflict shapes fail closed until the provider contract is confirmed.
		var providerErr struct {
			Code string `json:"code"`
			Type string `json:"type"`
		}
		if json.Unmarshal(bodyBytes, &providerErr) != nil ||
			(!strings.EqualFold(providerErr.Code, "BENEFICIARY_ALREADY_EXISTS") &&
				!strings.EqualFold(providerErr.Code, "beneficiary_already_exists") &&
				!strings.EqualFold(providerErr.Code, "beneficiary_id_already_exists")) {
			return nil, fmt.Errorf("cashfree payout: unrecognized beneficiary conflict (HTTP 409)")
		}
		existing, err := c.GetBeneficiary(ctx, req.BeneficiaryID)
		if err != nil {
			return nil, fmt.Errorf("cashfree payout: cannot verify existing beneficiary: %w", err)
		}
		if err := ValidateBeneficiaryReady(req.BeneficiaryID, req.BeneficiaryInstrument, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	errCode := parseProviderErrorCode(bodyBytes)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		slog.Error("CASHFREE PAYOUT AUTHENTICATION REJECTED - CONFIGURATION ERROR",
			"status", resp.StatusCode,
			"path", resp.Request.URL.Path,
			"code", errCode,
		)
		if errCode != "" {
			return nil, fmt.Errorf("%w: HTTP %d: %s", ErrPayoutAuthRejected, resp.StatusCode, errCode)
		}
		return nil, fmt.Errorf("%w: HTTP %d", ErrPayoutAuthRejected, resp.StatusCode)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if errCode != "" {
			return nil, fmt.Errorf("cashfree payout: create beneficiary failed (HTTP %d, code: %s)", resp.StatusCode, errCode)
		}
		return nil, fmt.Errorf("cashfree payout: create beneficiary failed (HTTP %d)", resp.StatusCode)
	}

	var out BeneficiaryResponse
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return nil, fmt.Errorf("cashfree payout: unmarshal beneficiary response: %w", err)
	}
	if err := ValidateBeneficiaryReady(req.BeneficiaryID, req.BeneficiaryInstrument, &out); err != nil {
		if out.BeneficiaryID != "" && out.BeneficiaryID != req.BeneficiaryID {
			return nil, err
		}
		switch strings.ToUpper(strings.TrimSpace(out.Status)) {
		case "INVALID", "CANCELLED", "FAILED", "DELETED":
			return nil, err
		}
		// 2xx create may return INITIATED or omit instrument details; read back from provider before allowing payout.
		existing, getErr := c.GetBeneficiary(ctx, req.BeneficiaryID)
		if getErr != nil {
			return nil, fmt.Errorf("cashfree payout: beneficiary readiness read-back failed: %w", getErr)
		}
		if readyErr := ValidateBeneficiaryReady(req.BeneficiaryID, req.BeneficiaryInstrument, existing); readyErr != nil {
			return nil, readyErr
		}
		return existing, nil
	}
	return &out, nil
}

// ValidateBeneficiaryReady verifies that a Cashfree Beneficiary V2 response matches
// the expected beneficiary ID, bank/VPA instrument, and is in VERIFIED state.
func ValidateBeneficiaryReady(expectedID string, expectedInst BeneficiaryInstrumentDetails, resp *BeneficiaryResponse) error {
	if resp == nil {
		return fmt.Errorf("cashfree payout: empty beneficiary response")
	}
	if resp.BeneficiaryID != expectedID {
		return fmt.Errorf("cashfree payout: beneficiary_id mismatch (got %q, want %q)", resp.BeneficiaryID, expectedID)
	}
	if !sameBeneficiaryInstrument(resp.Instrument, expectedInst) {
		return fmt.Errorf("cashfree payout: beneficiary instrument mismatch for %q", expectedID)
	}
	if !strings.EqualFold(strings.TrimSpace(resp.Status), "VERIFIED") {
		return fmt.Errorf("cashfree payout: beneficiary %q is not VERIFIED (status=%q)", expectedID, resp.Status)
	}
	return nil
}

// safeProviderErrorCode retains only a short machine-readable provider code.
// Provider error bodies can contain account data or request details, so do not
// surface arbitrary strings in application errors or logs.
func safeProviderErrorCode(body []byte) string {
	code := strings.TrimSpace(parseProviderErrorCode(body))
	if code == "" || len(code) > 64 {
		return ""
	}
	for _, r := range code {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.') {
			return ""
		}
	}
	return code
}

// GetBeneficiary retrieves the current provider state; never invent status or bank details.
// Cashfree Payouts V2 uses GET /beneficiary?beneficiary_id={beneficiary_id}.
func (c *PayoutClient) GetBeneficiary(ctx context.Context, beneficiaryID string) (*BeneficiaryResponse, error) {
	if !c.cfg.Enabled() {
		return nil, ErrPayoutNotConfigured
	}
	if strings.TrimSpace(beneficiaryID) == "" {
		return nil, fmt.Errorf("cashfree payout: beneficiary_id required")
	}
	reqURL := fmt.Sprintf("%s/beneficiary?beneficiary_id=%s", c.endpoint(), url.QueryEscape(beneficiaryID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cashfree payout: get beneficiary request: %w", err)
	}
	if err := c.sign(req); err != nil {
		return nil, err
	}
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
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Provider error bodies are not safe log material. Keep only the structured
		// provider error code so response text cannot leak beneficiary or request data.
		errCode := safeProviderErrorCode(body)
		slog.Error("CASHFREE PAYOUT AUTHENTICATION REJECTED - CONFIGURATION ERROR",
			"status", resp.StatusCode,
			"path", resp.Request.URL.Path,
			"code", errCode,
		)
		if errCode != "" {
			return nil, fmt.Errorf("%w: HTTP %d: %s", ErrPayoutAuthRejected, resp.StatusCode, errCode)
		}
		return nil, fmt.Errorf("%w: HTTP %d", ErrPayoutAuthRejected, resp.StatusCode)
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
