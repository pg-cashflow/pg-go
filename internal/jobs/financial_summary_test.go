package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

func TestPeriodForCadence(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, loc)

	period, tmpl, subject, err := periodForCadence("monthly", now)
	if err != nil {
		t.Fatalf("monthly: %v", err)
	}
	if period != "2026-07" || tmpl != "monthly.html" || !strings.Contains(subject, "Monthly") {
		t.Fatalf("monthly got period=%q tmpl=%q subject=%q", period, tmpl, subject)
	}

	period, tmpl, subject, err = periodForCadence("yearly", now)
	if err != nil {
		t.Fatalf("yearly: %v", err)
	}
	if period != "2025" || tmpl != "yearly.html" || !strings.Contains(subject, "Yearly") {
		t.Fatalf("yearly got period=%q tmpl=%q subject=%q", period, tmpl, subject)
	}

	if _, _, _, err := periodForCadence("weekly", now); err == nil {
		t.Fatal("expected error for unknown cadence")
	}
}

type stubProps struct {
	list []domain.Property
}

func (s stubProps) List(ctx context.Context) ([]domain.Property, error) { return s.list, nil }

type stubSummaries struct {
	sum *payment.ReconciliationSummary
}

func (s stubSummaries) BuildSummary(ctx context.Context, propertyID uuid.UUID, period string) (*payment.ReconciliationSummary, error) {
	out := *s.sum
	out.Period = period
	return &out, nil
}

type recordingMailer struct {
	to, subject, body string
	n                 int
}

func (m *recordingMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	m.n++
	m.to, m.subject, m.body = to, subject, htmlBody
	return nil
}

var _ mailer.Mailer = (*recordingMailer)(nil)

func TestFinancialSummaryJob_RunMonthly(t *testing.T) {
	mail := &recordingMailer{}
	propID := uuid.New()
	job := &FinancialSummaryJob{
		Properties: stubProps{list: []domain.Property{{
			ID: propID, Name: "Alpha PG", OwnerEmail: "owner@example.com",
		}}},
		Summaries: stubSummaries{sum: &payment.ReconciliationSummary{
			RentCollected:      100000,
			OutstandingRent:    5000,
			CreditsHeld:        0,
			DepositsHeld:       200000,
			CollectedByChannel: map[string]int64{"cash": 100000},
		}},
		Mailer: mail,
	}
	if err := job.Run(context.Background(), "monthly"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mail.n != 1 {
		t.Fatalf("expected 1 email, got %d", mail.n)
	}
	if mail.to != "owner@example.com" {
		t.Fatalf("to=%q", mail.to)
	}
	if !strings.Contains(mail.subject, "Monthly") || !strings.Contains(mail.subject, "Alpha PG") {
		t.Fatalf("subject=%q", mail.subject)
	}
	if mail.body == "" {
		t.Fatal("empty body")
	}
}

func TestFormatINR(t *testing.T) {
	if got := formatINR(12345); got != "₹123.45" {
		t.Fatalf("got %q", got)
	}
}
