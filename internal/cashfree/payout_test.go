package cashfree

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	t.Run("409_conflict_treated_as_idempotent_success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"beneficiary already exists"}`))
		}))
		defer srv.Close()

		client := NewPayoutClient(PayoutConfig{
			ClientID:     "test_cid",
			ClientSecret: "test_sec",
			FundsourceID: "fund_01",
		})
		client.SetBaseOverride(srv.URL)

		resp, err := client.CreateBeneficiary(context.Background(), CreateBeneficiaryRequest{
			BeneficiaryID:   "bene_existing",
			BeneficiaryName: "Jane Doe",
		})
		if err != nil {
			t.Fatalf("expected 409 to be treated as idempotent success, got %v", err)
		}
		if resp.BeneficiaryID != "bene_existing" || resp.Status != "ACTIVE" {
			t.Fatalf("unexpected response: %+v", resp)
		}
	})
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
					TransferAmount: FormatPaiseToRupees(150000), // "1500.00"
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
