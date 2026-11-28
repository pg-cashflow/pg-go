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

func (a *CashfreeAdapter) UploadDocument(ctx context.Context, fileReader io.Reader, filename string, documentType string, verificationID ...string) (*cashfree.SmartOCRResponse, error) {
	return a.client.UploadDocument(ctx, fileReader, filename, documentType, verificationID...)
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

func (a *CashfreeAdapter) CreateDigiLockerURL(ctx context.Context, in cashfree.CreateDigiLockerURLInput) (*cashfree.DigiLockerSession, error) {
	return a.client.CreateDigiLockerURL(ctx, in)
}

func (a *CashfreeAdapter) VerifyDigiLockerAccount(ctx context.Context, in cashfree.VerifyDigiLockerAccountInput) (*cashfree.DigiLockerAccountResult, error) {
	return a.client.VerifyDigiLockerAccount(ctx, in)
}

func (a *CashfreeAdapter) GetDigiLockerStatus(ctx context.Context, verificationID string) (*cashfree.DigiLockerStatusResult, error) {
	return a.client.GetDigiLockerStatus(ctx, verificationID)
}

func (a *CashfreeAdapter) GetDigiLockerAadhaar(ctx context.Context, verificationID string) (*cashfree.DigiLockerAadhaar, error) {
	return a.client.GetDigiLockerAadhaar(ctx, verificationID)
}
