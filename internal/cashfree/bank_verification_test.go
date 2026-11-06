package cashfree

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyBankAccountSync_NotConfigured(t *testing.T) {
	c := &Client{cfg: Config{}, http: http.DefaultClient}
	_, err := c.VerifyBankAccountSync(context.Background(), BankAccountVerificationRequest{
		BankAccount: "1234567890",
		IFSC:        "HDFC0001234",
	})
	if err == nil {
		t.Fatal("expected error when cashfree is not configured")
	}
}

func TestVerifyBankAccountSync_Validation(t *testing.T) {
	c := &Client{cfg: Config{AppID: "app", SecretKey: "sec"}, http: http.DefaultClient}
	_, err := c.VerifyBankAccountSync(context.Background(), BankAccountVerificationRequest{
		BankAccount: "",
		IFSC:        "HDFC0001234",
	})
	if !errors.Is(err, ErrBankAccountRequired) {
		t.Fatalf("expected ErrBankAccountRequired, got %v", err)
	}

	_, err = c.VerifyBankAccountSync(context.Background(), BankAccountVerificationRequest{
		BankAccount: "1234567890",
		IFSC:        "",
	})
	if !errors.Is(err, ErrIFSCRequired) {
		t.Fatalf("expected ErrIFSCRequired, got %v", err)
	}
}

func TestVerifyBankAccountSync_SuccessValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bank-account/sync" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-client-id") != "test-id" {
			t.Errorf("missing or incorrect x-client-id: %s", r.Header.Get("x-client-id"))
		}
		if r.Header.Get("x-client-secret") != "test-secret" {
			t.Errorf("missing or incorrect x-client-secret: %s", r.Header.Get("x-client-secret"))
		}

		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode body: %v", err)
		}
		if req["bank_account"] != "26291800001191" {
			t.Errorf("bank_account mismatch: %s", req["bank_account"])
		}
		if req["ifsc"] != "YESB0000001" {
			t.Errorf("ifsc mismatch: %s", req["ifsc"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"reference_id": 998877,
			"bank_account": "26291800001191",
			"ifsc": "YESB0000001",
			"name": "Jane Doe",
			"name_at_bank": "JANE DOE",
			"account_status": "VALID",
			"account_status_code": "ACCOUNT_IS_VALID",
			"status": "SUCCESS",
			"sub_code": "200",
			"message": "Transaction Successful",
			"name_match_score": "100.00",
			"name_match_result": "DIRECT_MATCH",
			"bank_name": "YES BANK",
			"utr": "123456789012"
		}`))
	}))
	defer srv.Close()

	c := newKYCTestClient(srv)
	res, err := c.VerifyBankAccountSync(context.Background(), BankAccountVerificationRequest{
		BankAccount: " 26291800001191 ",
		IFSC:        "yesb0000001",
		Name:        "Jane Doe",
		Phone:       "9999999999",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.IsValid() {
		t.Errorf("expected IsValid() to be true")
	}
	if res.ReferenceID != "998877" {
		t.Errorf("reference_id mismatch: %s", res.ReferenceID)
	}
	if res.NameAtBank != "JANE DOE" {
		t.Errorf("name_at_bank mismatch: %s", res.NameAtBank)
	}
	if res.AccountStatus != "VALID" {
		t.Errorf("account_status mismatch: %s", res.AccountStatus)
	}
	if res.NameMatchResult != "DIRECT_MATCH" {
		t.Errorf("name_match_result mismatch: %s", res.NameMatchResult)
	}
	if res.NameMatchScore != "100.00" {
		t.Errorf("name_match_score mismatch: %s", res.NameMatchScore)
	}
	if res.BankName != "YES BANK" {
		t.Errorf("bank_name mismatch: %s", res.BankName)
	}
}

func TestVerifyBankAccountSync_AccountInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"reference_id": "ref_invalid_1",
			"bank_account": "000000000000",
			"ifsc": "SBIN0000001",
			"account_status": "INVALID",
			"account_status_code": "INVALID_BANK_ACCOUNT",
			"status": "SUCCESS",
			"sub_code": "404",
			"message": "Account does not exist"
		}`))
	}))
	defer srv.Close()

	c := newKYCTestClient(srv)
	res, err := c.VerifyBankAccountSync(context.Background(), BankAccountVerificationRequest{
		BankAccount: "000000000000",
		IFSC:        "SBIN0000001",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.IsValid() {
		t.Errorf("expected IsValid() to be false for INVALID account")
	}
	if res.AccountStatus != "INVALID" {
		t.Errorf("expected account_status=INVALID, got %s", res.AccountStatus)
	}
	if res.AccountStatusCode != "INVALID_BANK_ACCOUNT" {
		t.Errorf("expected account_status_code=INVALID_BANK_ACCOUNT, got %s", res.AccountStatusCode)
	}
}

func TestVerifyBankAccountSync_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"message":"Insufficient balance in verification wallet"}`))
	}))
	defer srv.Close()

	c := newKYCTestClient(srv)
	_, err := c.VerifyBankAccountSync(context.Background(), BankAccountVerificationRequest{
		BankAccount: "1234567890",
		IFSC:        "HDFC0001234",
	})
	if err == nil {
		t.Fatal("expected APIError on HTTP 402")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if apiErr.StatusCode != http.StatusPaymentRequired {
		t.Errorf("expected status 402, got %d", apiErr.StatusCode)
	}
}

func TestVerifyBankAccountSync_SignatureAuth(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	var capturedSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSig = r.Header.Get("X-Cf-Signature")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"account_status":"VALID"}`))
	}))
	defer srv.Close()

	c := &Client{
		cfg: Config{
			AppID:        "app-sig",
			SecretKey:    "sec-sig",
			KYCAuthMode:  "signature",
			KYCPublicKey: &key.PublicKey,
		},
		http:         srv.Client(),
		baseOverride: srv.URL,
	}

	_, err = c.VerifyBankAccountSync(context.Background(), BankAccountVerificationRequest{
		BankAccount: "1122334455",
		IFSC:        "ICIC0001234",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedSig == "" {
		t.Errorf("expected X-Cf-Signature header to be set")
	}
}
