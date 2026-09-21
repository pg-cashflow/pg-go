package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/roi"
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

type stubFinanceProvider struct {
	op *finance.OperatingSummary
}

func (s stubFinanceProvider) OperatingSummary(ctx context.Context, propertyID uuid.UUID, period string, collectionsRent int64) (*finance.OperatingSummary, error) {
	return s.op, nil
}

type stubROIProvider struct {
	rep *roi.Report
}

func (s stubROIProvider) Report(ctx context.Context, propertyID uuid.UUID, period string, recon *payment.ReconciliationSummary, rooms []domain.Room, tenants []domain.Tenant, billedRent, fixedOpex, variableOpex int64) (*roi.Report, error) {
	return s.rep, nil
}

func TestFinancialSummaryJob_WithFinanceAndROI(t *testing.T) {
	mail := &recordingMailer{}
	propID := uuid.New()
	tbeMilli := 14200 // 14.2 months
	job := &FinancialSummaryJob{
		Properties: stubProps{list: []domain.Property{{
			ID: propID, Name: "Beta PG", OwnerEmail: "owner@beta.com",
		}}},
		Summaries: stubSummaries{sum: &payment.ReconciliationSummary{
			RentCollected:      15000000,
			OutstandingRent:    200000,
			CollectedByChannel: map[string]int64{"upi": 15000000},
		}},
		Finance: stubFinanceProvider{op: &finance.OperatingSummary{
			OCFPaise:              8000000,
			OpexPaise:             7000000,
			OperatingRevenuePaise: 15000000,
		}},
		ROI: stubROIProvider{rep: &roi.Report{
			Recovery: roi.Recovery{
				TBEMonthsMilli: &tbeMilli,
			},
			BreakEven: roi.BreakEven{
				BreakEvenOccupancyBPS: 7500,
			},
		}},
		Mailer: mail,
	}

	if err := job.Run(context.Background(), "monthly"); err != nil {
		t.Fatalf("Run with finance & roi: %v", err)
	}
	if mail.n != 1 {
		t.Fatalf("expected 1 email, got %d", mail.n)
	}
	if !strings.Contains(mail.body, "Operating Cash Flow") {
		t.Errorf("mail body missing OCF: %s", mail.body)
	}
	if !strings.Contains(mail.body, "Time to Break Even") {
		t.Errorf("mail body missing TBE: %s", mail.body)
	}
}
