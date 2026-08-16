package sms

import "context"

// SMSGateway sends SMS messages (OTP, reminders, etc.).
type SMSGateway interface {
	Send(ctx context.Context, phone, message string) error
}
