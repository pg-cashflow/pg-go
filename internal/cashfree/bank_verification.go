package cashfree

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var (
	ErrBankAccountRequired = errors.New("cashfree bank verification: bank_account is required")
	ErrIFSCRequired        = errors.New("cashfree bank verification: ifsc is required")
)

// BankAccountVerificationRequest represents the input parameters for POST /bank-account/sync.
type BankAccountVerificationRequest struct {
	BankAccount string `json:"bank_account"`
	IFSC        string `json:"ifsc"`
	Name        string `json:"name,omitempty"`
	Phone       string `json:"phone,omitempty"`
}

// BankAccountVerificationResult contains the parsed output of Cashfree Bank Account Verification Sync V2.
type BankAccountVerificationResult struct {
	ReferenceID       string `json:"reference_id"`
	BankAccount       string `json:"bank_account"`
	IFSC              string `json:"ifsc"`
	Name              string `json:"name,omitempty"`
	NameAtBank        string `json:"name_at_bank,omitempty"`
	AccountStatus     string `json:"account_status"`
	AccountStatusCode string `json:"account_status_code"`
	Status            string `json:"status"`
	SubCode           string `json:"sub_code,omitempty"`
	Message           string `json:"message,omitempty"`
	NameMatchScore    string `json:"name_match_score,omitempty"`
	NameMatchResult   string `json:"name_match_result,omitempty"`
	BankName          string `json:"bank_name,omitempty"`
	UTR               string `json:"utr,omitempty"`
}

// IsValid reports whether the bank verified the account as active and existing.
func (r *BankAccountVerificationResult) IsValid() bool {
	if r == nil {
		return false
	}
	return strings.EqualFold(r.AccountStatus, "VALID") ||
		strings.EqualFold(r.AccountStatusCode, "ACCOUNT_IS_VALID")
}

// VerifyBankAccountSync executes a synchronous penny-drop bank account verification request.
// Network call: callers must invoke outside database transactions.
func (c *Client) VerifyBankAccountSync(ctx context.Context, req BankAccountVerificationRequest) (*BankAccountVerificationResult, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}

	cleanAccount := strings.TrimSpace(req.BankAccount)
	if cleanAccount == "" {
		return nil, ErrBankAccountRequired
	}
	cleanIFSC := strings.ToUpper(strings.TrimSpace(req.IFSC))
	if cleanIFSC == "" {
		return nil, ErrIFSCRequired
	}

	bodyMap := map[string]string{
		"bank_account": cleanAccount,
		"ifsc":         cleanIFSC,
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		bodyMap["name"] = name
	}
	if phone := strings.TrimSpace(req.Phone); phone != "" {
		bodyMap["phone"] = phone
	}

	payload, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("cashfree bank verification: marshal request: %w", err)
	}

	endpoint := c.verificationEndpoint() + "/bank-account/sync"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("cashfree bank verification: create request: %w", err)
	}
	if err := c.signKYC(httpReq); err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.kycClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cashfree bank verification: do request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("cashfree bank verification: read response: %w", err)
	}

	if resp.StatusCode >= 300 {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(bodyBytes),
		}
	}

	var raw struct {
		ReferenceID       any    `json:"reference_id"`
		BankAccount       string `json:"bank_account"`
		IFSC              string `json:"ifsc"`
		Name              string `json:"name"`
		NameAtBank        string `json:"name_at_bank"`
		AccountStatus     string `json:"account_status"`
		AccountStatusCode string `json:"account_status_code"`
		Status            string `json:"status"`
		SubCode           any    `json:"sub_code"`
		Message           string `json:"message"`
		NameMatchScore    any    `json:"name_match_score"`
		NameMatchResult   string `json:"name_match_result"`
		BankName          string `json:"bank_name"`
		UTR               string `json:"utr"`
	}

	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, &CorruptDocumentError{Reason: fmt.Sprintf("unmarshal bank verification response: %v", err)}
	}

	subCodeStr := ""
	if raw.SubCode != nil {
		subCodeStr = fmt.Sprintf("%v", raw.SubCode)
	}

	matchScoreStr := ""
	if raw.NameMatchScore != nil {
		matchScoreStr = fmt.Sprintf("%v", raw.NameMatchScore)
	}

	return &BankAccountVerificationResult{
		ReferenceID:       parseRefID(raw.ReferenceID),
		BankAccount:       raw.BankAccount,
		IFSC:              raw.IFSC,
		Name:              raw.Name,
		NameAtBank:        raw.NameAtBank,
		AccountStatus:     raw.AccountStatus,
		AccountStatusCode: raw.AccountStatusCode,
		Status:            raw.Status,
		SubCode:           subCodeStr,
		Message:           raw.Message,
		NameMatchScore:    matchScoreStr,
		NameMatchResult:   raw.NameMatchResult,
		BankName:          raw.BankName,
		UTR:               raw.UTR,
	}, nil
}
