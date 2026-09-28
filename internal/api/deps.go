package api

import (
	"context"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/csv"
	"github.com/pg-cashflow/pg-go/internal/domain"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// KYCService orchestrates Aadhaar identity verification (ADR-004).
// Implemented by *kyc.Service; defined here so the api package owns the interface
// (standard Go dependency-inversion pattern — consumer defines the interface).
type KYCService interface {
	RecordConsent(ctx context.Context, tenantID uuid.UUID, purpose, consentVersion, consentText, ip, userAgent, actor string) (*domain.KYCConsent, error)
	InitiateDigiLocker(ctx context.Context, tenantID uuid.UUID, actor string) (verificationURL string, err error)
	ProcessDigiLockerCompletion(ctx context.Context, vendorRefID, failedReason, actor string) error
	VerifySecureQR(ctx context.Context, tenantID uuid.UUID, rawQR, actor string) (*domain.KYCVerification, error)
	VerifyAadhaarDocument(ctx context.Context, tenantID uuid.UUID, fileReader io.Reader, filename, actor string) (*domain.KYCVerification, error)
	GetDigiLockerReturnStatus(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error)
	GetAttestedPhoto(ctx context.Context, tenantID uuid.UUID) ([]byte, error)
	RevokeConsent(ctx context.Context, tenantID uuid.UUID, actor string) error
	GetStatus(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error)
	GetOwnerView(ctx context.Context, tenantID uuid.UUID) (*domain.KYCVerification, []domain.KYCAuditLog, error)
	GetVerificationByVendorRefID(ctx context.Context, vendorRefID string) (*domain.KYCVerification, error)
	ClearDuplicateFlag(ctx context.Context, verificationID uuid.UUID, actor, reason string) error
}

// AuthService handles Firebase ID-token exchange (and legacy OTP helpers kept for rollback).
type AuthService interface {
	RequestOTP(ctx context.Context, phone string) error
	RequestOTPWithPurpose(ctx context.Context, phone, purpose string) error
	VerifyOTPAndIssueToken(ctx context.Context, phone, otp string) (token string, user *domain.User, err error)
	VerifyStepUpOTP(ctx context.Context, phone, otp string) error
	VerifyFirebaseAndIssueToken(ctx context.Context, idToken, inviteCode string) (token string, user *domain.User, err error)
	VerifyFirebaseStepUp(ctx context.Context, idToken string, maxAge time.Duration) (auth.FirebaseIdentity, error)
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
	SuggestMatch(ctx context.Context, propertyID uuid.UUID, amountPaise int, txnDate time.Time, note string) (*payment.MatchResult, error)
	ManualMatch(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string, recordedBy uuid.UUID) (*domain.Payment, error)
	MarkCashPaid(ctx context.Context, dueID uuid.UUID, amountPaise int, recordedBy uuid.UUID, note string) (*domain.Payment, error)
	SettleDeposit(ctx context.Context, tenantID uuid.UUID, refundedPaise int64, reason string) error
	BuildSummary(ctx context.Context, propertyID uuid.UUID, period string) (*payment.ReconciliationSummary, error)
	GatewaySettle(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string, dedupKey ...string) (*domain.Payment, error)
}

// PropertyStore reads properties.
type PropertyStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Property, error)
	List(ctx context.Context) ([]domain.Property, error)
	GetByOwnerPhone(ctx context.Context, phone string) (*domain.Property, error)
	GetByInviteCode(ctx context.Context, code string) (*domain.Property, error)
}

// TenantStore reads tenants.
type TenantStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.Tenant, error)
	Update(ctx context.Context, t *domain.Tenant) error
	GetIDPhoto(ctx context.Context, id uuid.UUID) ([]byte, error)
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

// OutboxStore writes notification outbox events from handler code that does
// not have an open transaction (best-effort, same as Events.Publish pattern).
type OutboxStore interface {
	InsertEvent(ctx context.Context, evt *domain.OutboxEvent) error
}

// UserStore manages users.
type UserStore interface {
	Create(ctx context.Context, u *domain.User) error
	GetByPhone(ctx context.Context, phone string) (*domain.User, error)
	GetByPropertyAndRole(ctx context.Context, propertyID uuid.UUID, role domain.Role) ([]domain.User, error)
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
}

// PreferencesStore manages user preferences.
type PreferencesStore interface {
	GetByUserID(ctx context.Context, userID uuid.UUID) (*domain.UserPreferences, error)
	Upsert(ctx context.Context, userID uuid.UUID, locale string) (*domain.UserPreferences, error)
}

// AadhaarDecoder is optional override for tests.
type AadhaarDecoder func(raw string) (aadhaar.AadhaarData, bool, error)

// CSVParser is optional override for tests.
type CSVParser func(r io.Reader) ([]csv.Row, error)

type JoinService interface {
	LookupInvite(ctx context.Context, code string) (*domain.Property, error)
	RotateInvite(ctx context.Context, propertyID uuid.UUID) (string, error)
	EnsurePending(ctx context.Context, user *domain.User, propertyID uuid.UUID) (*domain.JoinRequest, error)
	SetProfile(ctx context.Context, userID uuid.UUID, name string, aadhaarLast4 *string) (*domain.JoinRequest, error)
	CompleteOnboarding(ctx context.Context, userID uuid.UUID, in joinsvc.ProfileInput) (*domain.JoinRequest, *domain.Tenant, error)
	Me(ctx context.Context, userID uuid.UUID) (*domain.JoinRequest, error)
	List(ctx context.Context, propertyID uuid.UUID, status *domain.JoinStatus) ([]domain.JoinRequest, error)
	Activate(ctx context.Context, propertyID, joinID uuid.UUID, in joinsvc.ActivateInput) (*domain.Tenant, error)
	Reject(ctx context.Context, propertyID, joinID uuid.UUID) error
}

type ReportStore interface {
	Create(ctx context.Context, p *domain.PaymentReport) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.PaymentReport, error)
	GetByUPITxnID(ctx context.Context, txnID string) (*domain.PaymentReport, error)
	ListByProperty(ctx context.Context, propertyID uuid.UUID, status *domain.PaymentReportStatus) ([]domain.PaymentReport, error)
	UpdateReview(ctx context.Context, p *domain.PaymentReport) error
	HasImageWithHash(ctx context.Context, propertyID uuid.UUID, hash string) (bool, error)
}

type IntentStore interface {
	GetByOrderID(ctx context.Context, orderID string) (*domain.PaymentIntent, error)
	GetByCFPaymentID(ctx context.Context, cfID string) (*domain.PaymentIntent, error)
	MarkPaid(ctx context.Context, id uuid.UUID, cfPaymentID string) error
	GetDuesSnapshot(ctx context.Context, intentID uuid.UUID) ([]domain.PaymentIntentDue, error)
}

type PaymentLookup interface {
	GetByUPITxnID(ctx context.Context, txnID string) (*domain.Payment, error)
}

type GatewayPaymentRepo interface {
	Create(ctx context.Context, p *domain.Payment) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error)
	GetByUPITxnID(ctx context.Context, txnID string) (*domain.Payment, error)
	GetByCFPaymentID(ctx context.Context, cfID string) (*domain.Payment, error)
	RecordProcessedEvent(ctx context.Context, provider, eventType, providerRefID, eventStatus string) (bool, error)
	CreateAllocation(ctx context.Context, paymentID, dueID uuid.UUID, amountPaise int64) error
	ListAllocationsByPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.PaymentAllocation, error)
	CreateWebhookEvent(ctx context.Context, evt *domain.WebhookEvent) error
	UpdateWebhookEventStatus(ctx context.Context, id uuid.UUID, status string, errMsg *string) error
	RecordUnmatchedReceipt(ctx context.Context, orderID, cfPaymentID string, intentID *uuid.UUID, amountPaise int64, failureReason string, payload []byte) error
	GetRefundByID(ctx context.Context, id uuid.UUID) (*domain.GatewayRefund, error)
	GetRefundByCFRefundID(ctx context.Context, cfRefundID string) (*domain.GatewayRefund, error)
	GetRefundByReference(ctx context.Context, ref string) (*domain.GatewayRefund, error)
	GetRefundByPaymentAndIdempotency(ctx context.Context, paymentID uuid.UUID, idempotencyKey string) (*domain.GatewayRefund, error)
	GetPaymentRefundedPaise(ctx context.Context, paymentID uuid.UUID) (int64, error)
	ListRefundsByPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.GatewayRefund, error)
	CreateOrUpdateRefund(ctx context.Context, ref *domain.GatewayRefund) error
	CreateRefundAllocation(ctx context.Context, alloc *domain.RefundAllocation) error
	GetDueNetPaidPaise(ctx context.Context, dueID uuid.UUID) (int64, error)
	ListStaleNonTerminalRefunds(ctx context.Context, olderThan time.Time) ([]domain.GatewayRefund, error)
}

type CashfreeClient interface {
	CreateRefund(ctx context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*cashfree.RefundDetails, error)
	FetchRefundStatus(ctx context.Context, orderID, refundID string) (*cashfree.RefundDetails, error)
}

// KYCSvc is set to a *kyc.Service in production. Optional — if nil the KYC
// routes respond 503 with a clear message so the rest of the app keeps running.
// (Field declaration only; the interface is defined above.)

type BankTransactionStore interface {
	InsertTransaction(ctx context.Context, tx pgx.Tx, txn *domain.BankTransaction) (bool, error)
	GetByID(ctx context.Context, id uuid.UUID) (*domain.BankTransaction, error)
	GetByPropertyAndID(ctx context.Context, propertyID, id uuid.UUID) (*domain.BankTransaction, error)
	ListByProperty(ctx context.Context, propertyID uuid.UUID, filter domain.BankTransactionFilter) ([]*domain.BankTransaction, int, error)
	UpdateStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, status domain.BankTransactionStatus, matchedDueID *uuid.UUID, matchedBy *uuid.UUID, matchedAt *time.Time, journalEntryID *uuid.UUID) error
}

