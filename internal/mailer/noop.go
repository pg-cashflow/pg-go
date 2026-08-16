package mailer

import "context"

// NoopMailer discards email sends (tests / local development).
type NoopMailer struct{}

func (NoopMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	return nil
}

var _ Mailer = NoopMailer{}
