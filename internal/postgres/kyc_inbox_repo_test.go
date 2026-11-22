package postgres

import (
	"testing"

	"github.com/pg-cashflow/pg-go/internal/kyc"
)

// TestKYCInboxRepo_InterfaceCompliance guarantees at compile-time that KYCInboxRepo
// satisfies the kyc.KYCInboxStore interface.
func TestKYCInboxRepo_InterfaceCompliance(t *testing.T) {
	var _ kyc.KYCInboxStore = (*KYCInboxRepo)(nil)
}
