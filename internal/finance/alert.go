package finance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/mailer"
)

// DeadLetterNotifier alerts operators when a ledger outbox event permanently fails.
type DeadLetterNotifier interface {
	NotifyDeadLetter(ctx context.Context, evt *domain.LedgerOutboxEvent, failureErr string) error
}

// SMSGateway defines the interface required to dispatch SMS alerts.
type SMSGateway interface {
	Send(ctx context.Context, phone, message string) error
}

// EmailDeadLetterNotifier sends an urgent email alert (and optional SMS backstop) to the operator/admin.
type EmailDeadLetterNotifier struct {
	mailer     mailer.Mailer
	adminEmail string
	smsGateway SMSGateway
	adminPhone string
}

// NewEmailDeadLetterNotifier constructs a dead-letter email notifier.
func NewEmailDeadLetterNotifier(m mailer.Mailer, adminEmail string) *EmailDeadLetterNotifier {
	return &EmailDeadLetterNotifier{
		mailer:     m,
		adminEmail: adminEmail,
	}
}

// WithSMSBackstop configures an SMS gateway and recipient phone for high-urgency alerts.
func (n *EmailDeadLetterNotifier) WithSMSBackstop(sms SMSGateway, adminPhone string) *EmailDeadLetterNotifier {
	n.smsGateway = sms
	n.adminPhone = adminPhone
	return n
}

// NotifyDeadLetter dispatches a formatted critical email alert (and SMS backstop) to the operator.
func (n *EmailDeadLetterNotifier) NotifyDeadLetter(ctx context.Context, evt *domain.LedgerOutboxEvent, failureErr string) error {
	if n == nil {
		return fmt.Errorf("email dead-letter notifier is nil")
	}
	if n.mailer == nil {
		return fmt.Errorf("mailer not configured for dead-letter notification")
	}
	if n.adminEmail == "" {
		return fmt.Errorf("admin email not configured for dead-letter notification")
	}

	var dispatchErrs []error

	// Primary Leg: Rich HTML Email
	subject := fmt.Sprintf("🚨 CRITICAL: Ledger Outbox Event #%d Dead-Lettered", evt.ID)
	body := fmt.Sprintf(
		"<h2>🚨 CRITICAL: Financial Books Desynchronized</h2>"+
			"<p>A transactional outbox event has exceeded maximum retries (<b>%d attempts</b>) and failed to mirror into the double-entry accounting ledger.</p>"+
			"<table>"+
			"<tr><td><b>Event ID:</b></td><td>%d</td></tr>"+
			"<tr><td><b>Event Type:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Property ID:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Source ID:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Idempotency Key:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Created At:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Terminal Error:</b></td><td><code>%s</code></td></tr>"+
			"</table>"+
			"<br/>"+
			"<p><b>Urgent Action Required:</b> Inspect the <code>ledger_outbox_events</code> table and manually reconcile the departure settlement journal entry to ensure double-entry ledger balance.</p>",
		evt.MaxAttempts,
		evt.ID,
		evt.EventType,
		evt.PropertyID,
		evt.SourceID,
		evt.IdempotencyKey,
		evt.CreatedAt.Format(time.RFC3339),
		failureErr,
	)
	if err := n.mailer.Send(ctx, n.adminEmail, subject, body); err != nil {
		dispatchErrs = append(dispatchErrs, fmt.Errorf("email send: %w", err))
	}

	// Backstop Leg: Instant SMS
	if n.smsGateway != nil && n.adminPhone != "" {
		smsMsg := fmt.Sprintf("🚨 CRITICAL: Ledger outbox event #%d dead-lettered (%s). Financial books desynchronized. Immediate manual reconciliation required.", evt.ID, evt.EventType)
		if err := n.smsGateway.Send(ctx, n.adminPhone, smsMsg); err != nil {
			dispatchErrs = append(dispatchErrs, fmt.Errorf("sms backstop send: %w", err))
		}
	}

	if len(dispatchErrs) > 0 {
		return errors.Join(dispatchErrs...)
	}
	return nil
}
