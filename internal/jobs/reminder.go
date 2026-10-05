package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/mailer"
)

const (
	ReminderDMinus3 = "D-3"
	ReminderD0      = "D-0"
	ReminderDPlus1  = "D+1"
	ReminderDPlus3  = "D+3"
	ReminderDPlus7  = "D+7"
)

// DueLister returns active pending/partial rent dues (tenant status join).
type DueLister interface {
	ActivePendingRentDues(ctx context.Context) ([]domain.Due, error)
}

// TenantGetter loads a tenant by ID.
type TenantGetter interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
}

// PropertyGetter loads a property by ID.
type PropertyGetter interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Property, error)
}

// ReminderLogger provides idempotent reminder logging.
type ReminderLogger interface {
	Exists(ctx context.Context, dueID uuid.UUID, reminderType, channel string) (bool, error)
	TryLog(ctx context.Context, dueID uuid.UUID, reminderType, channel string) (inserted bool, err error)
	DeleteLog(ctx context.Context, dueID uuid.UUID, reminderType, channel string) error
}

// ImportRecency reports the latest CSV import time for a property.
type ImportRecency interface {
	LatestImportedAt(ctx context.Context, propertyID uuid.UUID) (*time.Time, error)
}

// IntentRecency reports payment-intent activity for a due (Cashfree poll/settle freshness).
type IntentRecency interface {
	RecencyForDue(ctx context.Context, dueID uuid.UUID) (count int, latest *time.Time, err error)
}

// ImagePurger drops expired payment-report screenshots.
type ImagePurger interface {
	PurgeExpiredImages(ctx context.Context, olderThan time.Duration) (int64, error)
}

// ReminderJob sends D-3 / D-0 / D+1 / D+7 reminders in IST.
type ReminderJob struct {
	Dues       DueLister
	Tenants    TenantGetter
	Properties PropertyGetter
	Reminders  ReminderLogger
	Imports    ImportRecency
	Intents    IntentRecency
	Reports    ImagePurger
	MagicLink  MagicLinkCreator
	SMS        SMSSender
	Push       PushSender
	Mailer     mailer.Mailer
	Events     events.Publisher
	BaseURL     string
	Log         *slog.Logger
	CatchUpDays int // Days after nominal trigger day to catch up on missed reminders (default: 2)
}

func (j *ReminderJob) wasReminderSent(ctx context.Context, dueID uuid.UUID, remType string) bool {
	if j.Reminders == nil {
		return false
	}
	smsSent, err := j.Reminders.Exists(ctx, dueID, remType, "sms")
	if err == nil && smsSent {
		return true
	}
	pushSent, err := j.Reminders.Exists(ctx, dueID, remType, "push")
	if err == nil && pushSent {
		return true
	}
	return false
}

func (j *ReminderJob) resolveReminderType(ctx context.Context, dueID uuid.UUID, delta int) string {
	if j.Reminders == nil {
		switch delta {
		case -3:
			return ReminderDMinus3
		case 0:
			return ReminderD0
		case 1:
			return ReminderDPlus1
		case 3:
			return ReminderDPlus3
		case 7:
			return ReminderDPlus7
		default:
			return ""
		}
	}

	catchUp := j.CatchUpDays
	if catchUp <= 0 {
		catchUp = 2
	}

	// 1. D-3 stage: nominal delta == -3. Catch-up window [-3, min(-3 + catchUp, -1)]
	if delta == -3 {
		return ReminderDMinus3
	}
	if delta > -3 && delta < 0 && delta <= -3+catchUp {
		if !j.wasReminderSent(ctx, dueID, ReminderDMinus3) {
			return ReminderDMinus3
		}
	}

	// 2. D-0 stage: nominal delta == 0.
	if delta == 0 {
		return ReminderD0
	}

	// 3. D+1 stage: nominal delta == 1. Catch-up window [1, min(1 + catchUp, 6)]
	if delta == 1 {
		return ReminderDPlus1
	}
	if delta > 1 && delta < 7 && delta <= 1+catchUp {
		if !j.wasReminderSent(ctx, dueID, ReminderDPlus1) {
			return ReminderDPlus1
		}
	}

	// 4. D+7 stage: nominal delta == 7. Catch-up window [7, 7 + catchUp]
	if delta == 7 {
		return ReminderDPlus7
	}
	if delta > 7 && delta <= 7+catchUp {
		if !j.wasReminderSent(ctx, dueID, ReminderDPlus7) {
			return ReminderDPlus7
		}
	}

	return ""
}

// Run evaluates all active pending rent dues and sends due reminders.
func (j *ReminderJob) Run(ctx context.Context) error {
	log := j.Log
	if log == nil {
		log = slog.Default()
	}
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return fmt.Errorf("reminder: load IST: %w", err)
	}
	today := dateOnly(time.Now().In(loc))

	if j.Reports != nil {
		if _, err := j.Reports.PurgeExpiredImages(ctx, 30*24*time.Hour); err != nil {
			log.Error("reminder: purge report images", "err", err)
		}
	}

	dues, err := j.Dues.ActivePendingRentDues(ctx)
	if err != nil {
		return fmt.Errorf("reminder: list dues: %w", err)
	}

	var firstErr error
	for _, due := range dues {
		if err := j.processDue(ctx, due, today, loc); err != nil {
			log.Error("reminder: due failed", "due_id", due.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (j *ReminderJob) processDue(ctx context.Context, due domain.Due, today time.Time, loc *time.Location) error {
	tenant, err := j.Tenants.GetByID(ctx, due.TenantID)
	if err != nil {
		return err
	}
	if tenant.Status != domain.TenantStatusActive {
		return nil
	}
	if tenant.IsPhoneLess() {
		return nil
	}

	dueDate := dateOnly(due.DueDate.In(loc))
	delta := int(today.Sub(dueDate).Hours() / 24)

	remType := j.resolveReminderType(ctx, due.ID, delta)
	if remType == "" {
		return nil
	}

	if remType == ReminderDPlus1 || remType == ReminderDPlus7 {
		prop, err := j.Properties.GetByID(ctx, due.PropertyID)
		if err != nil {
			return err
		}
		var ok bool
		if prop.PaymentMode == domain.PaymentModeCashfree {
			ok, err = j.cashfreeFresh(ctx, due.ID)
		} else {
			ok, err = j.importFresh(ctx, due.PropertyID)
		}
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}

	prop, err := j.Properties.GetByID(ctx, due.PropertyID)
	if err != nil {
		return err
	}

	path := ""
	if j.MagicLink != nil {
		if p, err := j.MagicLink.CreatePaymentToken(ctx, due.ID); err == nil {
			path = p
		}
	}
	payURL := joinURL(j.BaseURL, path)
	msg := reminderMessage(remType, due.Amount, payURL)

	smsOK := j.sendSMS(ctx, due, tenant, prop, remType, msg)
	pushOK := j.sendPush(ctx, due, remType, msg, payURL)

	if !smsOK && !pushOK {
		return nil
	}
	return nil
}

func (j *ReminderJob) importFresh(ctx context.Context, propertyID uuid.UUID) (bool, error) {
	if j.Imports == nil {
		return false, nil
	}
	at, err := j.Imports.LatestImportedAt(ctx, propertyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if at == nil {
		return false, nil
	}
	return time.Since(*at) <= 24*time.Hour, nil
}

func (j *ReminderJob) cashfreeFresh(ctx context.Context, dueID uuid.UUID) (bool, error) {
	if j.Intents == nil {
		return true, nil
	}
	n, latest, err := j.Intents.RecencyForDue(ctx, dueID)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return true, nil
	}
	if latest == nil {
		return false, nil
	}
	return time.Since(*latest) <= 24*time.Hour, nil
}

func (j *ReminderJob) sendSMS(ctx context.Context, due domain.Due, tenant *domain.Tenant, prop *domain.Property, remType, msg string) bool {
	if j.SMS == nil || tenant.Phone == nil {
		return false
	}
	if j.Reminders != nil {
		inserted, err := j.Reminders.TryLog(ctx, due.ID, remType, "sms")
		if err != nil || !inserted {
			return false
		}
	}
	err := j.SMS.Send(ctx, *tenant.Phone, msg)
	if err != nil {
		if j.Reminders != nil {
			_ = j.Reminders.DeleteLog(ctx, due.ID, remType, "sms")
		}
		j.publishReminder(ctx, due, remType, "sms", err)
		if j.Mailer != nil && prop != nil && prop.OwnerEmail != "" {
			_ = j.Mailer.Send(ctx, prop.OwnerEmail,
				"SMS delivery failed",
				fmt.Sprintf("<p>Both SMS gateways failed for due %s (tenant %s).</p><p>%s</p><p>Error: %v</p>",
					due.DueCode, tenant.Name, msg, err))
		}
		return false
	}
	j.publishReminder(ctx, due, remType, "sms", nil)
	return true
}

func (j *ReminderJob) sendPush(ctx context.Context, due domain.Due, remType, msg, payURL string) bool {
	if j.Push == nil {
		return false
	}
	if j.Reminders != nil {
		inserted, err := j.Reminders.TryLog(ctx, due.ID, remType, "push")
		if err != nil || !inserted {
			return false
		}
	}
	payload := fmt.Appendf(nil, `{"title":"Rent reminder","body":%q,"url":%q}`, msg, payURL)
	err := j.Push.Send(ctx, due.TenantID, payload)
	if err != nil {
		if j.Reminders != nil {
			_ = j.Reminders.DeleteLog(ctx, due.ID, remType, "push")
		}
		j.publishReminder(ctx, due, remType, "push", err)
		return false
	}
	j.publishReminder(ctx, due, remType, "push", nil)
	return true
}

func (j *ReminderJob) publishReminder(ctx context.Context, due domain.Due, remType, channel string, sendErr error) {
	if j.Events == nil {
		return
	}
	payload := domain.ReminderPayload{
		DueID:        due.ID.String(),
		ReminderType: remType,
		Channel:      channel,
	}
	evtType := domain.EvtReminderSent
	if sendErr != nil {
		evtType = domain.EvtReminderFailed
		payload.Error = sendErr.Error()
	}
	b, _ := json.Marshal(payload)
	_ = j.Events.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(due.TenantID),
		PropertyID: due.PropertyID,
		EventType:  evtType,
		DueID:      &due.ID,
		OccurredAt: time.Now().UTC(),
		Payload:    b,
	})
}

func reminderMessage(remType string, amountPaise int, payURL string) string {
	rupees := float64(amountPaise) / 100.0
	switch remType {
	case ReminderDMinus3:
		return fmt.Sprintf("Rent ₹%.0f due in 3 days — pay: %s", rupees, payURL)
	case ReminderD0:
		return fmt.Sprintf("Rent ₹%.0f due today — pay: %s", rupees, payURL)
	case ReminderDPlus1:
		return fmt.Sprintf("Overdue since yesterday — Rent ₹%.0f — pay: %s", rupees, payURL)
	case ReminderDPlus7:
		return fmt.Sprintf("OVERDUE ₹%.0f — contact owner — pay: %s", rupees, payURL)
	default:
		return fmt.Sprintf("Rent ₹%.0f — pay: %s", rupees, payURL)
	}
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
