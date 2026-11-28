package kyc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type preflightSpyCashfree struct {
	*fakeCashfree
	verifyCalls int
	createCalls int
	verifyErr   error
}

func (p *preflightSpyCashfree) VerifyDigiLockerAccount(ctx context.Context, in cashfree.VerifyDigiLockerAccountInput) (*cashfree.DigiLockerAccountResult, error) {
	p.verifyCalls++
	if p.verifyErr != nil {
		return nil, p.verifyErr
	}
	return &cashfree.DigiLockerAccountResult{
		VerificationID: in.VerificationID,
		ReferenceID:    "ref_preflight_1",
		Status:         "ACCOUNT_EXISTS",
	}, nil
}

func (p *preflightSpyCashfree) CreateDigiLockerURL(ctx context.Context, in cashfree.CreateDigiLockerURLInput) (*cashfree.DigiLockerSession, error) {
	p.createCalls++
	return &cashfree.DigiLockerSession{
		VerificationID: in.VerificationID,
		ReferenceID:    "ref_sess_1",
		URL:            "https://digilocker.cashfree.com/consent/123",
		Status:         cashfree.DigiLockerPending,
	}, nil
}

func setupPreflightTestService(t *testing.T, enabled bool, cf *preflightSpyCashfree) (*Service, uuid.UUID) {
	t.Helper()
	repo := newFakeRepo()
	tenantID := uuid.New()
	repo.consents[tenantID] = &domain.KYCConsent{
		ID:         uuid.New(),
		TenantID:   tenantID,
		ConsentVer: "v1",
		GrantedAt:  time.Now().UTC(),
	}
	svc := NewService(repo, cf, Config{
		IdentitySecret:                   "test-secret-key-32-chars-minimum!!",
		HashKeyVersion:                   1,
		VerificationValidityDays:         365,
		DigiLockerVerifyAccountPreflight: enabled,
	})
	return svc, tenantID
}

func TestDigiLockerVerifyAccountPreflight_DefaultDisabledSkipsPreflight(t *testing.T) {
	cf := &preflightSpyCashfree{fakeCashfree: &fakeCashfree{}}
	svc, tenantID := setupPreflightTestService(t, false, cf)

	_, url, err := svc.InitiateDigiLockerVerification(context.Background(), tenantID, "https://app.example.com/kyc/cb", cashfree.DigiLockerFlowSignUp, "tenant")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if url == "" {
		t.Fatal("expected consent URL")
	}
	if cf.verifyCalls != 0 {
		t.Fatalf("expected 0 verify-account calls when disabled, got %d", cf.verifyCalls)
	}
	if cf.createCalls != 1 {
		t.Fatalf("expected 1 create-url call, got %d", cf.createCalls)
	}
}

func TestDigiLockerVerifyAccountPreflight_EnabledRunsForSignupOnly(t *testing.T) {
	t.Run("signup invokes preflight then creates URL", func(t *testing.T) {
		cf := &preflightSpyCashfree{fakeCashfree: &fakeCashfree{}}
		svc, tenantID := setupPreflightTestService(t, true, cf)

		_, url, err := svc.InitiateDigiLockerVerification(context.Background(), tenantID, "https://app.example.com/kyc/cb", cashfree.DigiLockerFlowSignUp, "tenant")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if url == "" {
			t.Fatal("expected consent URL")
		}
		if cf.verifyCalls != 1 || cf.createCalls != 1 {
			t.Fatalf("expected 1 verify and 1 create call, got verify=%d create=%d", cf.verifyCalls, cf.createCalls)
		}
	})

	t.Run("signin skips preflight even when enabled", func(t *testing.T) {
		cf := &preflightSpyCashfree{fakeCashfree: &fakeCashfree{}}
		svc, tenantID := setupPreflightTestService(t, true, cf)

		_, _, err := svc.InitiateDigiLockerVerification(context.Background(), tenantID, "https://app.example.com/kyc/cb", cashfree.DigiLockerFlowSignIn, "tenant")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if cf.verifyCalls != 0 || cf.createCalls != 1 {
			t.Fatalf("expected 0 verify and 1 create call for signin, got verify=%d create=%d", cf.verifyCalls, cf.createCalls)
		}
	})

	t.Run("failed preflight blocks consent URL creation", func(t *testing.T) {
		cf := &preflightSpyCashfree{fakeCashfree: &fakeCashfree{}, verifyErr: errors.New("provider 500")}
		svc, tenantID := setupPreflightTestService(t, true, cf)

		_, url, err := svc.InitiateDigiLockerVerification(context.Background(), tenantID, "https://app.example.com/kyc/cb", cashfree.DigiLockerFlowSignUp, "tenant")
		if err == nil {
			t.Fatal("expected error when preflight fails")
		}
		if url != "" {
			t.Fatalf("expected empty URL on preflight failure, got %q", url)
		}
		if cf.createCalls != 0 {
			t.Fatalf("expected 0 create-url calls when preflight fails, got %d", cf.createCalls)
		}
	})

	t.Run("timeout preflight blocks consent URL creation", func(t *testing.T) {
		cf := &preflightSpyCashfree{fakeCashfree: &fakeCashfree{}, verifyErr: context.DeadlineExceeded}
		svc, tenantID := setupPreflightTestService(t, true, cf)

		_, url, err := svc.InitiateDigiLockerVerification(context.Background(), tenantID, "https://app.example.com/kyc/cb", cashfree.DigiLockerFlowSignUp, "tenant")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected DeadlineExceeded, got %v", err)
		}
		if url != "" || cf.createCalls != 0 {
			t.Fatalf("expected empty URL and 0 create calls on timeout, got url=%q create=%d", url, cf.createCalls)
		}
	})
}
