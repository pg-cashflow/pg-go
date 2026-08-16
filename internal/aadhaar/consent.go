package aadhaar

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
)

// RecordConsent publishes ConsentGiven for DPDP audit.
func RecordConsent(ctx context.Context, pub events.Publisher, tenantID, propertyID uuid.UUID, channel, purpose string) error {
	payload, _ := json.Marshal(domain.ConsentGivenPayload{
		Channel: channel,
		Purpose: purpose,
	})
	return pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(tenantID),
		PropertyID: propertyID,
		EventType:  domain.EvtConsentGiven,
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	})
}

// RecordDepositTerms publishes DepositTermsAccepted with agreed notice period.
func RecordDepositTerms(ctx context.Context, pub events.Publisher, tenantID, propertyID uuid.UUID, channel string, noticePeriodDays int) error {
	payload, _ := json.Marshal(domain.DepositTermsAcceptedPayload{
		Channel:          channel,
		NoticePeriodDays: noticePeriodDays,
	})
	return pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(tenantID),
		PropertyID: propertyID,
		EventType:  domain.EvtDepositTermsAccepted,
		OccurredAt: time.Now().UTC(),
		Payload:    payload,
	})
}
