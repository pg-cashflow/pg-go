package cashfree

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDeriveBeneficiaryID(t *testing.T) {
	payeeID := uuid.MustParse("12345678-1234-5678-1234-567812345678")
	hashA := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	hashB := "9999999999999999999999999999999999999999999999999999999999999999"

	beneIDA := DeriveBeneficiaryID(payeeID, hashA)
	beneIDB := DeriveBeneficiaryID(payeeID, hashB)

	// Constraint: Length must be <= 50 characters
	if len(beneIDA) > 50 {
		t.Fatalf("derived beneficiary ID exceeds 50 chars: %d (%s)", len(beneIDA), beneIDA)
	}
	if len(beneIDB) > 50 {
		t.Fatalf("derived beneficiary ID exceeds 50 chars: %d (%s)", len(beneIDB), beneIDB)
	}

	// Cache invalidation invariant: when account hash changes, beneficiary_id MUST change
	if beneIDA == beneIDB {
		t.Fatalf("beneficiary ID must change when account number hash changes: got %s for both", beneIDA)
	}

	expectedPrefix := "bene_12345678123456781234567812345678_"
	if !strings.HasPrefix(beneIDA, expectedPrefix) {
		t.Fatalf("expected prefix %s, got %s", expectedPrefix, beneIDA)
	}
}

func TestCreateBeneficiaryV2(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/beneficiary" {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("x-client-id") != "test_cid" || r.Header.Get("x-api-version") != "2024-01-01" {
				http.Error(w, "bad headers", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(BeneficiaryResponse{
				BeneficiaryID:   "bene_123",
				BeneficiaryName: "Jane Doe",
				Status:          "ACTIVE",
			})
		}))
		defer srv.Close()

		client := NewPayoutClient(PayoutConfig{
			ClientID:     "test_cid",
			ClientSecret: "test_sec",
			FundsourceID: "fund_01",
		})
		client.SetBaseOverride(srv.URL)

		resp, err := client.CreateBeneficiary(context.Background(), CreateBeneficiaryRequest{
			BeneficiaryID:   "bene_123",
			BeneficiaryName: "Jane Doe",
			BeneficiaryInstrument: BeneficiaryInstrumentDetails{
				BankAccountNumber: "1234567890",
				BankIFSC:          "HDFC0001234",
			},
		})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if resp.BeneficiaryID != "bene_123" || resp.Status != "ACTIVE" {
			t.Fatalf("unexpected response: %+v", resp)
		}
	})

	for _, tc := range []struct {
		name        string
		code        string
		fetched     BeneficiaryResponse
		fetchStatus int
		wantOK      bool
		wantGET     bool
	}{
		{name: "matching_active", code: "BENEFICIARY_ALREADY_EXISTS", fetched: BeneficiaryResponse{BeneficiaryID: "bene_existing", Status: "ACTIVE", Instrument: BeneficiaryInstrumentDetails{BankAccountNumber: "1234567890", BankIFSC: "HDFC0001234"}}, wantOK: true, wantGET: true},
		{name: "different_bank", code: "BENEFICIARY_ALREADY_EXISTS", fetched: BeneficiaryResponse{BeneficiaryID: "bene_existing", Status: "ACTIVE", Instrument: BeneficiaryInstrumentDetails{BankAccountNumber: "0000000000", BankIFSC: "HDFC0001234"}}, wantGET: true},
		{name: "different_ifsc", code: "BENEFICIARY_ALREADY_EXISTS", fetched: BeneficiaryResponse{BeneficiaryID: "bene_existing", Status: "ACTIVE", Instrument: BeneficiaryInstrumentDetails{BankAccountNumber: "1234567890", BankIFSC: "ICIC0001234"}}, wantGET: true},
		{name: "inactive", code: "BENEFICIARY_ALREADY_EXISTS", fetched: BeneficiaryResponse{BeneficiaryID: "bene_existing", Status: "PENDING", Instrument: BeneficiaryInstrumentDetails{BankAccountNumber: "1234567890", BankIFSC: "HDFC0001234"}}, wantGET: true},
		{name: "id_mismatch", code: "BENEFICIARY_ALREADY_EXISTS", fetched: BeneficiaryResponse{BeneficiaryID: "other", Status: "ACTIVE", Instrument: BeneficiaryInstrumentDetails{BankAccountNumber: "1234567890", BankIFSC: "HDFC0001234"}}, wantGET: true},
		{name: "provider_get_failure", code: "BENEFICIARY_ALREADY_EXISTS", fetchStatus: http.StatusInternalServerError, wantGET: true},
		{name: "unknown_conflict", code: "SOME_OTHER_CONFLICT"},
		{name: "missing_error_code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getCount := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/beneficiary":
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "message": "conflict"})
				case r.Method == http.MethodGet && r.URL.Path == "/beneficiary/bene_existing":
					getCount++
					if tc.fetchStatus != 0 {
						w.WriteHeader(tc.fetchStatus)
						return
					}
					_ = json.NewEncoder(w).Encode(tc.fetched)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			client := NewPayoutClient(PayoutConfig{ClientID: "test_cid", ClientSecret: "test_sec", FundsourceID: "fund_01"})
			client.SetBaseOverride(srv.URL)
			response, err := client.CreateBeneficiary(context.Background(), CreateBeneficiaryRequest{
				BeneficiaryID: "bene_existing", BeneficiaryName: "Jane Doe",
				BeneficiaryInstrument: BeneficiaryInstrumentDetails{BankAccountNumber: "1234567890", BankIFSC: "HDFC0001234"},
			})
			if tc.wantOK && (err != nil || response == nil || response.Status != "ACTIVE") {
				t.Fatalf("expected verified beneficiary, response=%+v error=%v", response, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("expected conflict to fail closed, response=%+v", response)
			}
			if tc.wantGET && getCount != 1 {
				t.Fatalf("expected one readback, got %d", getCount)
			}
			if !tc.wantGET && getCount != 0 {
				t.Fatalf("unknown conflict must not be fetched, got %d", getCount)
			}
		})
	}

}

func TestRequestBatchTransferV2(t *testing.T) {
	t.Run("success_received", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/transfers/batch" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-deprecated-at", "2028-01-01")
			_ = json.NewEncoder(w).Encode(BatchTransferResponse{
				BatchTransferID:   "pgo_batch_123",
				CFBatchTransferID: "cf_batch_999",
				Status:            "RECEIVED",
			})
		}))
		defer srv.Close()

		client := NewPayoutClient(PayoutConfig{
			ClientID:     "test_cid",
			ClientSecret: "test_sec",
			FundsourceID: "fund_01",
		})
		client.SetBaseOverride(srv.URL)

		resp, err := client.RequestBatchTransfer(context.Background(), BatchTransferRequest{
			BatchTransferID: "pgo_batch_123",
			Transfers: []BatchTransferEntry{
				{
					TransferID:     "pgo_item_1",
					TransferAmount: json.Number(FormatPaiseToRupees(150000)), // "1500.00"
					TransferMode:   "banktransfer",
					FundsourceID:   "fund_01",
					BeneficiaryDetails: TransferBeneficiaryDetails{
						BeneficiaryID: "bene_123",
					},
				},
			},
		})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if resp.Status != "RECEIVED" || resp.CFBatchTransferID != "cf_batch_999" {
			t.Fatalf("unexpected response: %+v", resp)
		}
	})

	t.Run("5xx_ambiguity_fails_with_ErrDispatchUnknown", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal server error on gateway", http.StatusInternalServerError)
		}))
		defer srv.Close()

		client := NewPayoutClient(PayoutConfig{
			ClientID:     "test_cid",
			ClientSecret: "test_sec",
			FundsourceID: "fund_01",
		})
		client.SetBaseOverride(srv.URL)

		_, err := client.RequestBatchTransfer(context.Background(), BatchTransferRequest{
			BatchTransferID: "pgo_batch_500",
			Transfers: []BatchTransferEntry{
				{
					TransferID:     "pgo_item_1",
					TransferAmount: "100.00",
					TransferMode:   "banktransfer",
					FundsourceID:   "fund_01",
				},
			},
		})
		if !errors.Is(err, ErrDispatchUnknown) {
			t.Fatalf("expected ErrDispatchUnknown on 5xx, got %v", err)
		}
	})

	t.Run("beneficiary_not_found_404", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"beneficiary not found"}`))
		}))
		defer srv.Close()

		client := NewPayoutClient(PayoutConfig{
			ClientID:     "test_cid",
			ClientSecret: "test_sec",
			FundsourceID: "fund_01",
		})
		client.SetBaseOverride(srv.URL)

		_, err := client.RequestBatchTransfer(context.Background(), BatchTransferRequest{
			BatchTransferID: "pgo_batch_404",
			Transfers: []BatchTransferEntry{
				{
					TransferID:     "pgo_item_1",
					TransferAmount: "100.00",
					TransferMode:   "banktransfer",
					FundsourceID:   "fund_01",
				},
			},
		})
		if !errors.Is(err, ErrBeneficiaryNotFound) {
			t.Fatalf("expected ErrBeneficiaryNotFound on 404 beneficiary error, got %v", err)
		}
	})
}

func TestGetBatchTransferStatusV2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/transfers/batch" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("batch_transfer_id") != "pgo_batch_abc" {
			http.Error(w, "unknown batch", http.StatusBadRequest)
			return
		}
		utr := "UTR123456789"
		_ = json.NewEncoder(w).Encode(BatchTransferStatusResponse{
			BatchTransferID:   "pgo_batch_abc",
			CFBatchTransferID: "cf_batch_123",
			Status:            "SUCCESS",
			Transfers: []TransferStatusItem{
				{
					TransferID:   "pgo_item_1",
					CFTransferID: "cf_tr_1",
					Status:       "SUCCESS",
					UTR:          &utr,
				},
			},
		})
	}))
	defer srv.Close()

	client := NewPayoutClient(PayoutConfig{
		ClientID:     "test_cid",
		ClientSecret: "test_sec",
		FundsourceID: "fund_01",
	})
	client.SetBaseOverride(srv.URL)

	st, err := client.GetBatchTransferStatus(context.Background(), "pgo_batch_abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.Status != "SUCCESS" || len(st.Transfers) != 1 || *st.Transfers[0].UTR != "UTR123456789" {
		t.Fatalf("unexpected response: %+v", st)
	}
}

func TestPayoutSignature_RSAOAEP_SHA1(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	fixedTime := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	clientID := "CF_MOCK_CLIENT_ID_123"

	cfg := PayoutConfig{
		ClientID:     clientID,
		ClientSecret: "mock_secret",
		FundsourceID: "fund_01",
		AuthMode:     "signature",
		PublicKey:    &privKey.PublicKey,
		Now:          func() time.Time { return fixedTime },
	}

	client := NewPayoutClient(cfg)

	var capturedSignature string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSignature = r.Header.Get("X-Cf-Signature")
		if capturedSignature == "" {
			http.Error(w, "missing signature", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BatchTransferStatusResponse{
			BatchTransferID: "batch_1",
			Status:          "SUCCESS",
		})
	}))
	defer srv.Close()
	client.SetBaseOverride(srv.URL)

	_, err = client.GetBatchTransferStatus(context.Background(), "batch_1")
	if err != nil {
		t.Fatalf("unexpected error calling with signature: %v", err)
	}

	if capturedSignature == "" {
		t.Fatal("expected X-Cf-Signature header to be set")
	}

	// Verify decryptability using RSA-OAEP with SHA-1
	sigBytes, err := base64.StdEncoding.DecodeString(capturedSignature)
	if err != nil {
		t.Fatalf("signature is not valid base64: %v", err)
	}

	decryptedBytes, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, privKey, sigBytes, nil)
	if err != nil {
		t.Fatalf("failed to decrypt signature with RSA-OAEP SHA-1: %v", err)
	}

	expectedPlaintext := fmt.Sprintf("%s.%d", clientID, fixedTime.Unix())
	if string(decryptedBytes) != expectedPlaintext {
		t.Fatalf("decrypted signature mismatch: got %q, want %q", string(decryptedBytes), expectedPlaintext)
	}
}

func TestPayoutClient_AuthRejected_401_403(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"message": "ip not whitelisted or invalid signature", "code": "ip_validation_failed"}`))
			}))
			defer srv.Close()

			client := NewPayoutClient(PayoutConfig{
				ClientID:     "test_cid",
				ClientSecret: "test_sec",
				FundsourceID: "fund_01",
			})
			client.SetBaseOverride(srv.URL)

			_, err := client.GetBatchTransferStatus(context.Background(), "batch_1")
			if err == nil {
				t.Fatalf("expected error for status %d, got nil", code)
			}
			if !errors.Is(err, ErrPayoutAuthRejected) {
				t.Fatalf("expected ErrPayoutAuthRejected, got %v", err)
			}
			if !strings.Contains(err.Error(), "ip_validation_failed") {
				t.Fatalf("expected response body error code in error message, got %v", err)
			}
		})
	}
}
