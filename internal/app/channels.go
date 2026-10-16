package app

import (
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/sms"
)

// Channels holds the mailer and SMS gateway shared by the server and worker binaries.
type Channels struct {
	Mail    mailer.Mailer
	Gateway sms.SMSGateway
}

// OpenChannels builds SMTP and SMS clients from config.
// Empty SMTP or SMS settings select the noop implementations.
func OpenChannels(cfg *config.Config) Channels {
	var mail mailer.Mailer = mailer.NoopMailer{}
	if cfg.SMTPHost != "" {
		mail = &mailer.SMTPMailer{
			Host:     cfg.SMTPHost,
			Port:     cfg.SMTPPort,
			Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword,
			From:     cfg.SMTPFrom,
		}
	}
	var gateway sms.SMSGateway = sms.NoopGateway{}
	return Channels{Mail: mail, Gateway: gateway}
}
