package finance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/mailer"
)

// DeadLetterNotifier alerts operators when a ledger outbox event permanently fails.
type DeadLetterNotifier interface {
	NotifyDeadLetter(ctx context.Context, evt *domain.LedgerOutboxEvent, failureErr string) error
}

// PayoutProtectAlerter alerts operators when Cashfree flags a payout transfer for dashboard approval.
type PayoutProtectAlerter interface {
	NotifyPayoutProtectAlert(ctx context.Context, itemID, propertyID uuid.UUID, amountPaise int64, cfTransferID string) error
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
		if strings.Contains(fmt.Sprintf("%T", n.smsGateway), "Noop") {
			slog.Warn("dead-letter alert: SMS backstop gateway is a no-op implementation; SMS alert skipped",
				"admin_phone", n.adminPhone, "event_id", evt.ID)
		} else {
			smsMsg := fmt.Sprintf("🚨 CRITICAL: Ledger outbox event #%d dead-lettered (%s). Financial books desynchronized. Immediate manual reconciliation required.", evt.ID, evt.EventType)
			if err := n.smsGateway.Send(ctx, n.adminPhone, smsMsg); err != nil {
				dispatchErrs = append(dispatchErrs, fmt.Errorf("sms backstop send: %w", err))
			}
		}
	}

	if len(dispatchErrs) > 0 {
		return errors.Join(dispatchErrs...)
	}
	return nil
}

// NotifyPaymentCommandDeadLetter dispatches a formatted critical email alert when a payment gateway command permanently fails.
func (n *EmailDeadLetterNotifier) NotifyPaymentCommandDeadLetter(ctx context.Context, cmd *domain.PaymentCommand, failureErr string) error {
	if n == nil || n.mailer == nil || n.adminEmail == "" {
		return nil
	}

	subject := fmt.Sprintf("🚨 CRITICAL: Payment Command #%d Dead-Lettered", cmd.ID)
	body := fmt.Sprintf(
		"<h2>🚨 CRITICAL: Payment Command Failed</h2>"+
			"<p>A gateway payment command has exceeded maximum retries (<b>%d attempts</b>) and failed to execute.</p>"+
			"<table>"+
			"<tr><td><b>Command ID:</b></td><td>%d</td></tr>"+
			"<tr><td><b>Command Type:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Property ID:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Source ID:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Idempotency Key:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Created At:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Terminal Error:</b></td><td><code>%s</code></td></tr>"+
			"</table>"+
			"<br/>"+
			"<p><b>Urgent Action Required:</b> Inspect the <code>payment_commands</code> table and investigate gateway connectivity or command payload.</p>",
		cmd.MaxAttempts,
		cmd.ID,
		cmd.CommandType,
		cmd.PropertyID,
		cmd.SourceID,
		cmd.IdempotencyKey,
		cmd.CreatedAt.Format(time.RFC3339),
		failureErr,
	)
	_ = n.mailer.Send(ctx, n.adminEmail, subject, body)
	if n.smsGateway != nil && n.adminPhone != "" {
		smsMsg := fmt.Sprintf("🚨 CRITICAL: Payment command #%d dead-lettered (%s). Immediate manual investigation required.", cmd.ID, cmd.CommandType)
		_ = n.smsGateway.Send(ctx, n.adminPhone, smsMsg)
	}
	return nil
}

// NotifyPayoutProtectAlert dispatches an alert when a payout transfer requires manual approval in Cashfree Dashboard.
func (n *EmailDeadLetterNotifier) NotifyPayoutProtectAlert(ctx context.Context, itemID, propertyID uuid.UUID, amountPaise int64, cfTransferID string) error {
	if n == nil || n.mailer == nil || n.adminEmail == "" {
		return nil
	}

	subject := fmt.Sprintf("⚠️ ACTION REQUIRED: Payout Transfer %s Pending Cashfree Approval", cfTransferID)
	body := fmt.Sprintf(
		"<h2>⚠️ Cashfree Payout Protect: Manual Approval Required</h2>"+
			"<p>A payout transfer has been placed in <b>APPROVAL_PENDING</b> status by Cashfree Payout Protect.</p>"+
			"<table>"+
			"<tr><td><b>Payout Item ID:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Property ID:</b></td><td>%s</td></tr>"+
			"<tr><td><b>Amount:</b></td><td>₹%.2f</td></tr>"+
			"<tr><td><b>Cashfree Transfer ID:</b></td><td>%s</td></tr>"+
			"</table>"+
			"<br/>"+
			"<p><b>Action Required:</b> Log in to the <a href=\"https://merchant.cashfree.com/merchants/payouts\">Cashfree Merchant Dashboard</a> to approve or reject the transfer.</p>",
		itemID,
		propertyID,
		float64(amountPaise)/100.0,
		cfTransferID,
	)
	_ = n.mailer.Send(ctx, n.adminEmail, subject, body)
	if n.smsGateway != nil && n.adminPhone != "" {
		smsMsg := fmt.Sprintf("⚠️ ACTION REQUIRED: Payout transfer for ₹%.2f requires manual approval in Cashfree dashboard (transfer: %s).", float64(amountPaise)/100.0, cfTransferID)
		_ = n.smsGateway.Send(ctx, n.adminPhone, smsMsg)
	}
	return nil
}
