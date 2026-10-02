package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/roi"
)

// SummaryBuilder builds a reconciliation summary for a property/period.
type SummaryBuilder interface {
	BuildSummary(ctx context.Context, propertyID uuid.UUID, period string) (*payment.ReconciliationSummary, error)
}

// FinanceSummaryProvider provides operating summary (OCF, Opex).
type FinanceSummaryProvider interface {
	OperatingSummary(ctx context.Context, propertyID uuid.UUID, period string, collectionsRent int64) (*finance.OperatingSummary, error)
}

// ROISummaryProvider provides ROI metrics (TBE, recovery).
type ROISummaryProvider interface {
	Report(ctx context.Context, propertyID uuid.UUID, period string, recon *payment.ReconciliationSummary, rooms []domain.Room, tenants []domain.Tenant, billedRent, fixedOpex, variableOpex int64) (*roi.Report, error)
}

// PropertyLister lists all properties.
type PropertyLister interface {
	List(ctx context.Context) ([]domain.Property, error)
}

// PropertyTenants lists tenants for a property (for event FK when publishing).
type PropertyTenants interface {
	ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.Tenant, error)
}

// FinancialSummaryJob emails monthly/yearly digests to property owners.
type FinancialSummaryJob struct {
	Properties  PropertyLister
	Tenants     PropertyTenants
	Summaries   SummaryBuilder
	Finance     FinanceSummaryProvider
	ROI         ROISummaryProvider
	Mailer      mailer.Mailer
	Events      events.Publisher
	TemplateDir string
	Log         *slog.Logger
}

// Run sends digests for cadence "monthly" or "yearly".
func (j *FinancialSummaryJob) Run(ctx context.Context, cadence string) error {
	log := j.Log
	if log == nil {
		log = slog.Default()
	}
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return err
	}
	now := time.Now().In(loc)
	period, tmplName, subjectPrefix, err := periodForCadence(cadence, now)
	if err != nil {
		return err
	}

	props, err := j.Properties.List(ctx)
	if err != nil {
		return fmt.Errorf("financial-summary: list properties: %w", err)
	}

	tmpl, err := j.loadTemplate(tmplName)
	if err != nil {
		return err
	}

	var firstErr error
	for _, p := range props {
		if err := j.sendOne(ctx, p, period, cadence, subjectPrefix, tmpl); err != nil {
			log.Error("financial-summary: send failed", "property_id", p.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (j *FinancialSummaryJob) sendOne(ctx context.Context, p domain.Property, period, cadence, subjectPrefix string, tmpl *template.Template) error {
	sum, err := j.Summaries.BuildSummary(ctx, p.ID, period)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	data := map[string]any{
		"Period":             sum.Period,
		"PropertyName":       p.Name,
		"RentCollected":      formatINR(sum.RentCollected),
		"OutstandingRent":    formatINR(sum.OutstandingRent),
		"CreditsHeld":        formatINR(sum.CreditsHeld),
		"DepositsHeld":       formatINR(sum.DepositsHeld),
		"DepositsRefunded":   formatINR(sum.DepositsRefunded),
		"CollectedByChannel": formatChannels(sum.CollectedByChannel),
	}

	payloadMap := map[string]any{
		"period":  period,
		"cadence": cadence,
		"sent_to": p.OwnerEmail,
	}

	if j.Finance != nil {
		if op, err := j.Finance.OperatingSummary(ctx, p.ID, period, sum.RentCollected); err == nil && op != nil {
			data["OCF"] = formatINR(op.OCFPaise)
			data["Opex"] = formatINR(op.OpexPaise)
			data["OperatingRevenue"] = formatINR(op.OperatingRevenuePaise)
			payloadMap["ocf_paise"] = op.OCFPaise
			payloadMap["opex_paise"] = op.OpexPaise
		}
	}

	if j.ROI != nil {
		if rep, err := j.ROI.Report(ctx, p.ID, period, sum, nil, nil, 0, 0, 0); err == nil && rep != nil {
			if rep.Recovery.TBEMonthsMilli != nil {
				tbeMonths := float64(*rep.Recovery.TBEMonthsMilli) / 1000.0
				data["TBE"] = fmt.Sprintf("%.1f months", tbeMonths)
				payloadMap["tbe_months"] = tbeMonths
			}
			if rep.BreakEven.BreakEvenOccupancyBPS > 0 {
				data["BreakEvenOccupancy"] = fmt.Sprintf("%.1f%%", float64(rep.BreakEven.BreakEvenOccupancyBPS)/100.0)
			}
		}
	}

	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}
	subject := fmt.Sprintf("%s — %s — %s", subjectPrefix, p.Name, period)
	if err := j.Mailer.Send(ctx, p.OwnerEmail, subject, buf.String()); err != nil {
		return err
	}

	if j.Events != nil {
		payload, _ := json.Marshal(payloadMap)
		var tenantPtr *uuid.UUID
		if j.Tenants != nil {
			if ts, err := j.Tenants.ListByProperty(ctx, p.ID); err == nil && len(ts) > 0 {
				tenantPtr = domain.Ptr(ts[0].ID)
			}
		}
		_ = j.Events.Publish(ctx, domain.Event{
			TenantID:   tenantPtr,
			PropertyID: p.ID,
			EventType:  domain.EvtFinancialSummarySent,
			OccurredAt: time.Now().UTC(),
			Payload:    payload,
		})
	}
	return nil
}

func (j *FinancialSummaryJob) loadTemplate(name string) (*template.Template, error) {
	dir := j.TemplateDir
	if dir == "" {
		dir = filepath.Join("internal", "mailer", "templates")
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			dir = filepath.Join("..", "..", "internal", "mailer", "templates")
		}
	}
	path := filepath.Join(dir, name)
	b, err := os.ReadFile(filepath.Clean(path)) // #nosec G304
	if err != nil {
		return template.New(name).Parse(`<h1>{{.Period}}</h1><p>{{.PropertyName}}</p>
<p>Rent collected: {{.RentCollected}}</p>
<p>Outstanding: {{.OutstandingRent}}</p>
{{if .OCF}}<p>Operating Cash Flow: {{.OCF}}</p>{{end}}
{{if .Opex}}<p>OPEX: {{.Opex}}</p>{{end}}
{{if .TBE}}<p>Time to Break Even: {{.TBE}}</p>{{end}}`)
	}
	return template.New(name).Parse(string(b))
}

func periodForCadence(cadence string, now time.Time) (period, tmpl, subject string, err error) {
	switch cadence {
	case "monthly":
		firstOfThis := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		prev := firstOfThis.AddDate(0, -1, 0)
		return prev.Format("2006-01"), "monthly.html", "Monthly collections digest", nil
	case "yearly":
		year := now.Year() - 1
		return fmt.Sprintf("%d", year), "yearly.html", "Yearly collections digest", nil
	default:
		return "", "", "", fmt.Errorf("financial-summary: unknown cadence %q (want monthly|yearly)", cadence)
	}
}

func formatINR(paise int64) string {
	rupees := float64(paise) / 100.0
	return fmt.Sprintf("₹%.2f", rupees)
}

func formatChannels(m map[string]int64) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = formatINR(v)
	}
	return out
}
