package app

import (
	"testing"

	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
)

// TestNilCashfreeInterfaces_NotTypedNil guards against the Go typed-nil interface
// trap: assigning a nil *cashfree.Client directly to an interface field makes
// `iface == nil` evaluate to false and panics on call.
func TestNilCashfreeInterfaces_NotTypedNil(t *testing.T) {
	var cfClient *cashfree.Client

	var cfCashfreeClient api.CashfreeClient
	var cfBankVerifier api.BankAccountVerifier
	if cfClient != nil {
		cfCashfreeClient = cfClient
		cfBankVerifier = cfClient
	}

	if cfCashfreeClient != nil {
		t.Fatal("expected nil api.CashfreeClient interface, got typed nil")
	}
	if cfBankVerifier != nil {
		t.Fatal("expected nil api.BankAccountVerifier interface, got typed nil")
	}
}
