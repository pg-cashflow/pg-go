package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// userLookup is the subset of UserRepo methods the resolver needs.
type userLookup interface {
	GetByPropertyAndRole(ctx context.Context, propertyID uuid.UUID, role domain.Role) ([]domain.User, error)
	GetByTenantID(ctx context.Context, tenantID uuid.UUID) (*domain.User, error)
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
}

// Resolver maps outbox event types to recipient Notification templates.
// An empty return slice with no error means "valid business state, zero
// recipients" — never treat that as a failure.
type Resolver struct {
	users userLookup
}

func NewResolver(users userLookup) *Resolver {
	return &Resolver{users: users}
}

// Resolve implements RecipientResolver.
func (r *Resolver) Resolve(ctx context.Context, evt *domain.OutboxEvent) ([]domain.Notification, error) {
	switch evt.EventType {

	// Owner-directed: payment report submitted by tenant
	case string(domain.EvtPaymentReportSubmitted):
		return r.toOwner(ctx, evt, "payment_review", "📋 New payment report awaiting review", "/owner/payment-reports")

	// Tenant-directed: payment report rejected by owner
	case string(domain.EvtPaymentReportRejected):
		return r.toTenant(ctx, evt, "payment_rejected", "❌ Your payment report was rejected", "/tenant/dues")

	// Owner-directed: due paid (tenant confirmed payment)
	case string(domain.EvtDuePaidOnTime), string(domain.EvtDuePaidLate):
		return r.toOwner(ctx, evt, "due_paid", "✅ Rent payment received", "/owner/payments")

	// Owner-directed: tenant joined (join request approved)
	case string(domain.EvtJoinRequested):
		return r.toOwner(ctx, evt, "join_requested", "👤 New join request pending your review", "/owner/join-requests")

	// Tenant-directed: their join application was approved
	case string(domain.EvtJoinApproved):
		return r.toTenant(ctx, evt, "join_approved", "🎉 Your join request was approved! Welcome aboard.", "/tenant/me")

	// Tenant-directed: onboarding complete / tenant profile created
	case string(domain.EvtTenantCreated):
		return r.toTenant(ctx, evt, "tenant_created", "👋 Your tenant account is ready. Tap to get started.", "/tenant/me")

	// Tenant-directed: inspection completed (they can review it)
	case string(domain.EvtInspectionCompleted):
		deepLink := r.inspectionDeepLink(evt.Payload, "/tenant/inspections")
		return r.toTenant(ctx, evt, "inspection_completed", "🔍 A new room inspection is ready for your review.", deepLink)

	// Owner + Manager: inspection disputed by tenant
	case string(domain.EvtInspectionDisputed):
		deepLink := r.inspectionDeepLink(evt.Payload, "/owner/inspections")
		return r.toOwnerAndManagers(ctx, evt, "inspection_disputed", "⚠️ Tenant has disputed an inspection item.", deepLink)

	// Owner + Manager: hazard reported
	case string(domain.EvtHazardReported):
		deepLink := r.hazardDeepLink(evt.Payload, "/owner/hazards")
		return r.toOwnerAndManagers(ctx, evt, "hazard_reported", "🚨 A new hazard has been reported.", deepLink)

	// Tenant-directed: hazard they reported has been resolved
	case string(domain.EvtHazardResolved):
		return r.toTenant(ctx, evt, "hazard_resolved", "✅ A hazard you reported has been resolved.", "/tenant/hazards")

	// Tenant-directed: violation issued
	case string(domain.EvtViolationIssued):
		return r.toTenant(ctx, evt, "violation_issued", "⚠️ A conduct violation has been noted on your account.", "/tenant/violations")

	// Tenant-directed: points awarded
	case string(domain.EvtPointsAwarded):
		return r.toTenant(ctx, evt, "points_awarded", "🌟 You've earned reward points!", "/tenant/points")

	// Tenant-directed: reward redeemed confirmation
	case string(domain.EvtRewardRedeemed):
		return r.toTenant(ctx, evt, "reward_redeemed", "🎁 Your reward redemption is confirmed.", "/tenant/rewards")

	// Owner-directed: recurring tie-out exception across 3 consecutive months (ADR-1 Level 3 alert)
	case string(domain.EvtRecurringTieOutException):
		return r.toOwner(ctx, evt, "recurring_tie_out_exception", "⚠️ Recurring tie-out exception across 3 consecutive months", "/owner/finance/tie-out")

	// Owner-directed: period tie-out blocked due to unexplained discrepancy
	case string(domain.EvtPeriodTieOutBlocked):
		return r.toOwner(ctx, evt, "period_tie_out_blocked", "⚠️ Period tie-out blocked: unexplained discrepancy", "/owner/finance/tie-out")

	default:
		// Unknown event type → skip silently. Returning no recipients with no
		// error is the correct "skip" signal to the dispatcher.
		return nil, nil
	}
}

// --- recipient helper builders ---

// toOwner resolves all owner users for the event's property and returns one
// Notification template per owner (typically exactly one).
func (r *Resolver) toOwner(ctx context.Context, evt *domain.OutboxEvent, notifType, title, deepLink string) ([]domain.Notification, error) {
	owners, err := r.users.GetByPropertyAndRole(ctx, evt.PropertyID, domain.RoleOwner)
	if err != nil {
		return nil, fmt.Errorf("resolve owner: %w", err)
	}
	return notifTemplates(owners, evt.PropertyID, notifType, title, deepLink, true), nil
}

// toTenant resolves the user account for the event's tenant_id.
// If tenant_id is nil or has no linked user account yet, returns zero
// recipients (valid business state, not an error).
func (r *Resolver) toTenant(ctx context.Context, evt *domain.OutboxEvent, notifType, title, deepLink string) ([]domain.Notification, error) {
	if evt.TenantID == nil {
		return nil, nil
	}
	u, err := r.users.GetByTenantID(ctx, *evt.TenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Tenant has no user account yet — valid state (e.g. owner-created tenants
		// whose phone hasn't been attached). Not an error.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve tenant user: %w", err)
	}
	return notifTemplates([]domain.User{*u}, evt.PropertyID, notifType, title, deepLink, false), nil
}

// toOwnerAndManagers resolves the property owner and any managers.
// Missing managers (empty result) is a valid business state — silently skipped.
func (r *Resolver) toOwnerAndManagers(ctx context.Context, evt *domain.OutboxEvent, notifType, title, deepLink string) ([]domain.Notification, error) {
	owners, err := r.users.GetByPropertyAndRole(ctx, evt.PropertyID, domain.RoleOwner)
	if err != nil {
		return nil, fmt.Errorf("resolve owners: %w", err)
	}
	managers, err := r.users.GetByPropertyAndRole(ctx, evt.PropertyID, domain.RoleManager)
	if err != nil {
		return nil, fmt.Errorf("resolve managers: %w", err)
	}
	all := append(owners, managers...)
	return notifTemplates(all, evt.PropertyID, notifType, title, deepLink, true), nil
}

// notifTemplates builds one Notification template per user in recipients.
// isActionRequired is set by the caller based on whether the notification
// demands user action (approve payment, respond to dispute, etc.).
func notifTemplates(users []domain.User, propertyID uuid.UUID, notifType, title, deepLink string, isActionRequired bool) []domain.Notification {
	out := make([]domain.Notification, 0, len(users))
	for _, u := range users {
		out = append(out, domain.Notification{
			RecipientID:      u.ID,
			PropertyID:       propertyID,
			Type:             notifType,
			Title:            title,
			DeepLink:         deepLink,
			IsActionRequired: isActionRequired,
		})
	}
	return out
}

// --- deep link helpers ---

// inspectionDeepLink extracts inspection_id from the payload and appends it
// to a base path. Falls back to basePath on parse error.
func (r *Resolver) inspectionDeepLink(payload json.RawMessage, basePath string) string {
	var p struct {
		InspectionID *string `json:"inspection_id"`
	}
	if err := json.Unmarshal(payload, &p); err == nil && p.InspectionID != nil {
		return basePath + "/" + *p.InspectionID
	}
	return basePath
}

// hazardDeepLink extracts hazard_id from the payload and appends it to the
// base path. Falls back to basePath on parse error.
func (r *Resolver) hazardDeepLink(payload json.RawMessage, basePath string) string {
	var p struct {
		HazardID *string `json:"hazard_id"`
	}
	if err := json.Unmarshal(payload, &p); err == nil && p.HazardID != nil {
		return basePath + "/" + *p.HazardID
	}
	return basePath
}
