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

// RecurringExpenseScheduler processes recurring expenses during the billing cycle.
type RecurringExpenseScheduler interface {
	ProcessRecurringExpenses(ctx context.Context, asOf time.Time) error
}

// RunReport captures execution statistics for scheduled rent due generation (Requirement 12).
type RunReport struct {
	TenantsProcessed    int `json:"tenants_processed"`
	DuesCreated         int `json:"dues_created"`
	ExistingDuesSkipped int `json:"existing_dues_skipped"`
	Errors              int `json:"errors"`
}

// BillingCycle runs anniversary rent-due creation.
type BillingCycle struct {
	Billing           BillingService
	Tenants           TenantLister
	Properties        PropertyGetter
	MagicLink         MagicLinkCreator
	SMS               SMSSender
	Push              PushSender
	RecurringExpenses RecurringExpenseScheduler
	Mailer            interface {
		Send(ctx context.Context, to, subject, htmlBody string) error
	}
	Events  events.Publisher
	BaseURL string // e.g. https://pay.example.com
	Log     *slog.Logger
	// ErrOpenDueExists is matched with errors.Is to skip tenants with open dues.
	ErrOpenDueExists error
}

// GenerateMonthlyRentDues executes idempotent monthly rent due generation for active tenants and returns a RunReport.
func (j *BillingCycle) GenerateMonthlyRentDues(ctx context.Context, asOf time.Time) (RunReport, error) {
	var report RunReport
	log := j.Log
	if log == nil {
		log = slog.Default()
	}
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	now := asOf.In(loc)
	day := now.Day()

	dueDays := []int{day}
	tomorrow := now.AddDate(0, 0, 1)
	if tomorrow.Month() != now.Month() {
		for d := day + 1; d <= 31; d++ {
			dueDays = append(dueDays, d)
		}
	}

	var allTenants []domain.Tenant
	for _, d := range dueDays {
		list, err := j.Tenants.ListActiveByDueDay(ctx, d, nil)
		if err != nil {
			return report, fmt.Errorf("billing-cycle: list tenants for day %d: %w", d, err)
		}
		allTenants = append(allTenants, list...)
	}

	var firstErr error
	for _, t := range allTenants {
		if t.Status != domain.TenantStatusActive {
			continue
		}
		report.TenantsProcessed++
		due, err := j.Billing.CreateRentDue(ctx, &t)
		if err != nil {
			if j.ErrOpenDueExists != nil && errors.Is(err, j.ErrOpenDueExists) {
				report.ExistingDuesSkipped++
				continue
			}
			report.Errors++
			log.Error("billing-cycle: tenant failed", "tenant_id", t.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if due == nil {
			report.ExistingDuesSkipped++
			continue
		}
		report.DuesCreated++

		if err := j.processNotifications(ctx, t, due); err != nil {
			log.Warn("billing-cycle: notification failed", "tenant_id", t.ID, "err", err)
		}
	}

	if j.RecurringExpenses != nil {
		if err := j.RecurringExpenses.ProcessRecurringExpenses(ctx, now); err != nil {
			log.Error("billing-cycle: recurring expenses hook failed", "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return report, firstErr
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

	// Month-end clamping logic:
	// If today is the last day of the current month, also query tenants whose due_day > today's day (e.g. 29, 30, 31).
	tomorrow := now.AddDate(0, 0, 1)
	isMonthEnd := tomorrow.Month() != now.Month()

	dueDays := []int{day}
	if isMonthEnd {
		for d := day + 1; d <= 31; d++ {
			dueDays = append(dueDays, d)
		}
	}

	var allTenants []domain.Tenant
	for _, d := range dueDays {
		list, err := j.Tenants.ListActiveByDueDay(ctx, d, nil)
		if err != nil {
			return fmt.Errorf("billing-cycle: list tenants for day %d: %w", d, err)
		}
		allTenants = append(allTenants, list...)
	}

	var firstErr error
	for _, t := range allTenants {
		if err := j.processTenant(ctx, t); err != nil {
			log.Error("billing-cycle: tenant failed", "tenant_id", t.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	if j.RecurringExpenses != nil {
		if err := j.RecurringExpenses.ProcessRecurringExpenses(ctx, now); err != nil {
			log.Error("billing-cycle: recurring expenses hook failed", "err", err)
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
	return j.processNotifications(ctx, t, due)
}

func (j *BillingCycle) processNotifications(ctx context.Context, t domain.Tenant, due *domain.Due) error {
	if j.MagicLink == nil {
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

	rupees := due.Amount / 100
	msg := fmt.Sprintf("Rent ₹%d due today — pay: %s", rupees, payURL)
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
		payload := fmt.Appendf(nil, `{"title":"Rent due","body":%q,"url":%q}`,
			fmt.Sprintf("Rent ₹%d due today", rupees), payURL)
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
