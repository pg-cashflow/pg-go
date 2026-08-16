package api

import (
	"context"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
	"github.com/pg-cashflow/pg-go/internal/csv"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// AuthService handles Firebase ID-token exchange (and legacy OTP helpers kept for rollback).
type AuthService interface {
	RequestOTP(ctx context.Context, phone string) error
	VerifyOTPAndIssueToken(ctx context.Context, phone, otp string) (token string, user *domain.User, err error)
	VerifyFirebaseAndIssueToken(ctx context.Context, idToken string) (token string, user *domain.User, err error)
}

// MagicLinkService resolves and creates payment tokens.
type MagicLinkService interface {
	CreatePaymentToken(ctx context.Context, dueID uuid.UUID) (string, error)
	ResolveToken(ctx context.Context, raw string) (*magiclink.DueView, error)
}

// PushService manages web-push subscriptions.
type PushService interface {
	Subscribe(ctx context.Context, tenantID uuid.UUID, endpoint, p256dh, auth string) error
}

// TenantService owns tenant lifecycle mutations.
type TenantService interface {
	CreateTenant(ctx context.Context, in domain.NewTenantInput, depositPaise int) (*domain.Tenant, error)
	UpdateTenant(ctx context.Context, t *domain.Tenant) error
	Vacate(ctx context.Context, tenantID uuid.UUID) error
	LogNotice(ctx context.Context, tenantID uuid.UUID, at time.Time) error
	AttachPhone(ctx context.Context, tenantID uuid.UUID, phone string) error
}

// BillingService adjusts dues.
type BillingService interface {
	WaiveDue(ctx context.Context, dueID uuid.UUID) (*domain.Due, error)
	Prorate(ctx context.Context, tenantID uuid.UUID, vacateDate time.Time) (*domain.Due, error)
}

// PaymentService matches payments and cash/deposit settlement.
type PaymentService interface {
	MatchPayment(ctx context.Context, propertyID uuid.UUID, txnID string, amountPaise int, date time.Time, note string) (*domain.Payment, error)
	ManualMatch(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string, recordedBy uuid.UUID) (*domain.Payment, error)
	MarkCashPaid(ctx context.Context, dueID uuid.UUID, amountPaise int, recordedBy uuid.UUID, note string) (*domain.Payment, error)
	SettleDeposit(ctx context.Context, tenantID uuid.UUID, refundedPaise int64, reason string) error
	BuildSummary(ctx context.Context, propertyID uuid.UUID, period string) (*payment.ReconciliationSummary, error)
}

// PropertyStore reads properties.
type PropertyStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Property, error)
	List(ctx context.Context) ([]domain.Property, error)
	GetByOwnerPhone(ctx context.Context, phone string) (*domain.Property, error)
}

// TenantStore reads tenants.
type TenantStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.Tenant, error)
	Update(ctx context.Context, t *domain.Tenant) error
}

// DueStore reads dues.
type DueStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Due, error)
	List(ctx context.Context, f postgres.DueListFilter) ([]domain.Due, error)
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Due, error)
}

// PaymentStore reads payments.
type PaymentStore interface {
	ListByProperty(ctx context.Context, propertyID uuid.UUID, matchedBy *domain.MatchedBy) ([]domain.Payment, error)
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Payment, error)
}

// EventStore reads audit events.
type EventStore interface {
	List(ctx context.Context, f postgres.EventFilter) ([]domain.Event, error)
}

// ImportStore records CSV imports.
type ImportStore interface {
	Create(ctx context.Context, l *postgres.ImportLog) error
}

// AadhaarDecoder is optional override for tests.
type AadhaarDecoder func(raw string) (aadhaar.AadhaarData, bool, error)

// CSVParser is optional override for tests.
type CSVParser func(r io.Reader) ([]csv.Row, error)
