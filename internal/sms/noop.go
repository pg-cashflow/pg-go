package sms

import "context"

// NoopGateway discards SMS sends (tests / local development).
type NoopGateway struct{}

func (NoopGateway) Send(ctx context.Context, phone, message string) error {
	return nil
}

var _ SMSGateway = NoopGateway{}
