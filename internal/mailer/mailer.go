package mailer

import "context"

// Mailer sends HTML email (SMS-failure alerts, financial digests).
type Mailer interface {
	Send(ctx context.Context, to, subject, htmlBody string) error
}
