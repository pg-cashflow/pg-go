package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
)

// BillingService creates rent dues for anniversary billing.
type BillingService interface {
	CreateRentDue(ctx context.Context, tenant *domain.Tenant) (*domain.Due, error)
}

// TenantLister lists active tenants by due day (vacated excluded by query).
type TenantLister interface {
	ListActiveByDueDay(ctx context.Context, dueDay int, propertyID *uuid.UUID) ([]domain.Tenant, error)
}

// MagicLinkCreator issues payment magic-link paths.
type MagicLinkCreator interface {
	CreatePaymentToken(ctx context.Context, dueID uuid.UUID) (path string, err error)
}

// SMSSender sends SMS messages.
type SMSSender interface {
	Send(ctx context.Context, phone, message string) error
}

// PushSender delivers web-push payloads.
type PushSender interface {
	Send(ctx context.Context, tenantID uuid.UUID, payload []byte) error
}

// BillingCycle runs anniversary rent-due creation.
type BillingCycle struct {
	Billing    BillingService
	Tenants    TenantLister
	Properties PropertyGetter
	MagicLink  MagicLinkCreator
	SMS        SMSSender
	Push       PushSender
	Mailer     interface {
		Send(ctx context.Context, to, subject, htmlBody string) error
	}
	Events  events.Publisher
	BaseURL string // e.g. https://pay.example.com
	Log     *slog.Logger
	// ErrOpenDueExists is matched with errors.Is to skip tenants with open dues.
	ErrOpenDueExists error
}

// Run creates rent dues for active tenants whose due_day is today in Asia/Kolkata.
func (j *BillingCycle) Run(ctx context.Context) error {
	log := j.Log
	if log == nil {
		log = slog.Default()
	}
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return fmt.Errorf("billing-cycle: load IST: %w", err)
	}
	now := time.Now().In(loc)
	day := now.Day()
	if day > 28 {
		log.Info("billing-cycle: day>28, nothing to do", "day", day)
		return nil
	}

	tenants, err := j.Tenants.ListActiveByDueDay(ctx, day, nil)
	if err != nil {
		return fmt.Errorf("billing-cycle: list tenants: %w", err)
	}

	var firstErr error
	for _, t := range tenants {
		if err := j.processTenant(ctx, t); err != nil {
			log.Error("billing-cycle: tenant failed", "tenant_id", t.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (j *BillingCycle) processTenant(ctx context.Context, t domain.Tenant) error {
	if t.Status != domain.TenantStatusActive {
		return nil
	}
	due, err := j.Billing.CreateRentDue(ctx, &t)
	if err != nil {
		if j.ErrOpenDueExists != nil && errors.Is(err, j.ErrOpenDueExists) {
			return nil
		}
		return err
	}
	if due == nil {
		return nil
	}

	path, err := j.MagicLink.CreatePaymentToken(ctx, due.ID)
	if err != nil {
		return fmt.Errorf("magic link: %w", err)
	}
	payURL := joinURL(j.BaseURL, path)

	if t.IsPhoneLess() {
		return nil
	}

	rupees := float64(due.Amount) / 100.0
	msg := fmt.Sprintf("Rent ₹%.0f due today — pay: %s", rupees, payURL)
	if t.Phone != nil && j.SMS != nil {
		if err := j.SMS.Send(ctx, *t.Phone, msg); err != nil {
			log := j.Log
			if log == nil {
				log = slog.Default()
			}
			log.Warn("billing-cycle: sms failed", "tenant_id", t.ID, "err", err)
			if j.Mailer != nil && j.Properties != nil {
				if prop, perr := j.Properties.GetByID(ctx, t.PropertyID); perr == nil && prop.OwnerEmail != "" {
					_ = j.Mailer.Send(ctx, prop.OwnerEmail, "SMS delivery failed",
						fmt.Sprintf("<p>Both SMS gateways failed for tenant %s (due %s).</p><p>%v</p>", t.Name, due.DueCode, err))
				}
			}
		}
	}
	if j.Push != nil {
		payload := []byte(fmt.Sprintf(`{"title":"Rent due","body":%q,"url":%q}`,
			fmt.Sprintf("Rent ₹%.0f due today", rupees), payURL))
		_ = j.Push.Send(ctx, t.ID, payload)
	}
	return nil
}

func joinURL(base, path string) string {
	if base == "" {
		return path
	}
	if len(base) > 0 && base[len(base)-1] == '/' && len(path) > 0 && path[0] == '/' {
		return base[:len(base)-1] + path
	}
	if len(base) > 0 && base[len(base)-1] != '/' && len(path) > 0 && path[0] != '/' {
		return base + "/" + path
	}
	return base + path
}
