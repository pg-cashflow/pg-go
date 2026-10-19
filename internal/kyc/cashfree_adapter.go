// Package kyc — cashfree_adapter.go
// CashfreeAdapter bridges *cashfree.Client to the kyc.CashfreeKYCClient
// interface without introducing an import cycle.
package kyc

import (
	"context"
	"io"

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

func (a *CashfreeAdapter) UploadAadhaarDocument(ctx context.Context, fileReader io.Reader, filename string) (*cashfree.SmartOCRResponse, error) {
	return a.client.UploadAadhaarDocument(ctx, fileReader, filename)
}

func (a *CashfreeAdapter) RequestAadhaarOTP(ctx context.Context, aadhaarNumber string) (*cashfree.AadhaarOTPInitResult, error) {
	return a.client.RequestAadhaarOTP(ctx, aadhaarNumber)
}

func (a *CashfreeAdapter) VerifyAadhaarOTP(ctx context.Context, refID, otp string) (*cashfree.AadhaarOTPVerifyResult, error) {
	return a.client.VerifyAadhaarOTP(ctx, refID, otp)
}
