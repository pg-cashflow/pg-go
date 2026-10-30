package app

import (
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/notification"
	"github.com/pg-cashflow/pg-go/internal/sms"
)

// Channels holds the mailer and SMS gateway shared by the server and worker binaries.
type Channels struct {
	Mail     mailer.Mailer
	Gateway  sms.SMSGateway
	WhatsApp notification.WhatsAppSender
}

// OpenChannels builds SMTP, SMS, and WhatsApp clients from config.
// Empty SMTP, SMS, or WhatsApp settings select the noop implementations.
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
	var wa notification.WhatsAppSender = notification.NoopWhatsAppSender{}
	if cfg.WhatsAppPhoneNumberID != "" && cfg.WhatsAppAccessToken != "" {
		wa = notification.NewMetaWhatsAppSender(notification.MetaWhatsAppConfig{
			BaseURL:       cfg.WhatsAppBaseURL,
			PhoneNumberID: cfg.WhatsAppPhoneNumberID,
			AccessToken:   cfg.WhatsAppAccessToken,
		})
	}
	return Channels{Mail: mail, Gateway: gateway, WhatsApp: wa}
}
