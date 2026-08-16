package mailer

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// SMTPMailer sends mail via SMTP (Gmail App Password; Brevo is a drop-in via same env vars).
type SMTPMailer struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
}

func (m *SMTPMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	_ = ctx // net/smtp has no native context; cancellation is best-effort via dialer if needed later
	if m.Host == "" || m.Port == "" || m.From == "" {
		return fmt.Errorf("smtp mailer not configured")
	}
	addr := net.JoinHostPort(m.Host, m.Port)
	msg := buildMessage(m.From, to, subject, htmlBody)

	var auth smtp.Auth
	if m.Username != "" {
		auth = smtp.PlainAuth("", m.Username, m.Password, m.Host)
	}
	if err := smtp.SendMail(addr, auth, m.From, []string{to}, msg); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

func buildMessage(from, to, subject, htmlBody string) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(htmlBody)
	return []byte(b.String())
}

var _ Mailer = (*SMTPMailer)(nil)
