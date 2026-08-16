package sms

import (
	"context"
	"fmt"

	"github.com/pg-cashflow/pg-go/internal/mailer"
)

// MailAlert returns an AlertFunc that emails the given address when both SMS legs fail.
func MailAlert(m mailer.Mailer, to string) AlertFunc {
	return func(ctx context.Context, phone, message string, primaryErr, fallbackErr error) {
		if m == nil || to == "" {
			return
		}
		body := fmt.Sprintf(
			`<p>Both primary and fallback Android SMS gateways failed.</p>
<p><strong>Phone:</strong> %s</p>
<p><strong>Message:</strong> %s</p>
<p><strong>Primary error:</strong> %v</p>
<p><strong>Fallback error:</strong> %v</p>`,
			phone, message, primaryErr, fallbackErr,
		)
		_ = m.Send(ctx, to, "SMS delivery failed", body)
	}
}
