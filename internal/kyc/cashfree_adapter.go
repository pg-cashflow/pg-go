// Package kyc — cashfree_adapter.go
// CashfreeClientAdapter bridges *cashfree.Client to the kyc.CashfreeKYCClient
// interface without introducing an import cycle. The kyc package defines
// DigiLockerDoc as its own value type; this adapter translates between the two.
package kyc

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/cashfree"
)

// CashfreeAdapter wraps *cashfree.Client and satisfies CashfreeKYCClient.
type CashfreeAdapter struct {
	client *cashfree.Client
}

// NewCashfreeAdapter constructs a CashfreeAdapter from an existing Cashfree client.
func NewCashfreeAdapter(client *cashfree.Client) *CashfreeAdapter {
	return &CashfreeAdapter{client: client}
}

func (a *CashfreeAdapter) CreateDigiLockerLink(ctx context.Context, verificationID, redirectURL string) (string, error) {
	return a.client.CreateDigiLockerLink(ctx, verificationID, redirectURL)
}

func (a *CashfreeAdapter) GetDigiLockerDocument(ctx context.Context, verificationID string) (*DigiLockerDoc, error) {
	doc, err := a.client.GetDigiLockerDocument(ctx, verificationID)
	if err != nil {
		return nil, err
	}
	return &DigiLockerDoc{
		Name:      doc.Name,
		DOB:       doc.DOB,
		Gender:    doc.Gender,
		MaskedUID: doc.MaskedUID,
	}, nil
}
