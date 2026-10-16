package mailer

import "context"

// NoopMailer discards email sends (tests / local development).
type NoopMailer struct{}

func (NoopMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	return nil
}

var _ Mailer = NoopMailer{}

// IsNoop reports whether m is the discard implementation (SMTP not configured).
// A nil Mailer also reports true: callers must not treat "unset" and "noop"
// as different states when deciding whether a delivery channel is real.
func IsNoop(m Mailer) bool {
	if m == nil {
		return true
	}
	_, ok := m.(NoopMailer)
	return ok
}
