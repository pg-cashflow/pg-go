package finance

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/mailer"
)

// DeadLetterNotifier alerts operators when a ledger outbox event permanently fails.
type DeadLetterNotifier interface {
	NotifyDeadLetter(ctx context.Context, evt *domain.LedgerOutboxEvent, failureErr string) error
}

// EmailDeadLetterNotifier sends an urgent email alert to the operator/admin via mailer.Mailer.
type EmailDeadLetterNotifier struct {
	mailer     mailer.Mailer
	adminEmail string
}

// NewEmailDeadLetterNotifier constructs a dead-letter email notifier.
func NewEmailDeadLetterNotifier(m mailer.Mailer, adminEmail string) *EmailDeadLetterNotifier {
	return &EmailDeadLetterNotifier{
		mailer:     m,
		adminEmail: adminEmail,
	}
}

// NotifyDeadLetter dispatches a formatted critical email alert to the operator.
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
	return n.mailer.Send(ctx, n.adminEmail, subject, body)
}
