package collector

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubIntents struct {
	created  *domain.PaymentIntent
	existing *domain.PaymentIntent
}

func (s *stubIntents) Create(_ context.Context, p *domain.PaymentIntent) error {
	p.ID = uuid.New()
	s.created = p
	return nil
}
func (s *stubIntents) LatestOpenForDue(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	if s.existing != nil {
		return s.existing, nil
	}
	return nil, pgx.ErrNoRows
}

type stubCF struct{ n int }

func (s *stubCF) CreateUPIOrder(context.Context, string, int, string, string) (string, *time.Time, error) {
	s.n++
	exp := time.Now().Add(time.Hour)
	return "sess_1", &exp, nil
}

func TestPayIntentManualIncludesVPA(t *testing.T) {
	svc := New(&stubIntents{}, nil)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 1500000, Status: domain.DueStatusPending}
	prop := &domain.Property{UPIVPA: "owner@upi", OwnerName: "Owner", PaymentMode: domain.PaymentModeManual}
	intent, png, err := svc.PayIntent(context.Background(), due, prop, "12", "/tenant/dues/x/qr", "")
	if err != nil {
		t.Fatal(err)
	}
	if intent.VPA != "owner@upi" || intent.Note != "PG-ABC123" || !intent.Payable || intent.Mode != domain.PaymentModeManual {
		t.Fatalf("%+v", intent)
	}
	if len(png) == 0 {
		t.Fatal("expected png")
	}
}

func TestPayIntentHidesWhenPaid(t *testing.T) {
	svc := New(&stubIntents{}, nil)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 1, Status: domain.DueStatusPaid}
	prop := &domain.Property{UPIVPA: "owner@upi", PaymentMode: domain.PaymentModeManual}
	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Payable || intent.VPA != "" {
		t.Fatalf("paid due must not expose pay fields: %+v", intent)
	}
}

func TestPayIntentManualIgnoresCashfreeClient(t *testing.T) {
	cf := &stubCF{}
	svc := New(&stubIntents{}, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 100, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeManual, UPIVPA: "owner@upi", OwnerName: "O"}
	intent, png, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Mode != domain.PaymentModeManual || cf.n != 0 || len(png) == 0 {
		t.Fatalf("manual must not create Cashfree orders: mode=%s n=%d png=%d", intent.Mode, cf.n, len(png))
	}
}

func TestPayIntentCashfreeSession(t *testing.T) {
	cf := &stubCF{}
	svc := New(&stubIntents{}, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 100, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree, UPIVPA: "hidden@upi"}
	intent, png, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "9999999999")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Mode != domain.PaymentModeCashfree || intent.PaymentSessionID != "sess_1" || intent.VPA != "" {
		t.Fatalf("%+v", intent)
	}
	if png != nil {
		t.Fatal("cashfree should not return personal QR png")
	}
	if cf.n != 1 {
		t.Fatalf("orders=%d", cf.n)
	}
}

func TestPayIntentCashfreeNoPhoneFallsBackToManual(t *testing.T) {
	cf := &stubCF{}
	svc := New(&stubIntents{}, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 100, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree, UPIVPA: "owner@upi", OwnerName: "O"}
	intent, png, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if intent.Mode != domain.PaymentModeManual || cf.n != 0 || len(png) == 0 {
		t.Fatalf("missing phone must fall back to manual: mode=%s n=%d png=%d", intent.Mode, cf.n, len(png))
	}
}

func TestEnsureCashfreeRejectsStaleAmount(t *testing.T) {
	sess := "old_sess"
	existing := &domain.PaymentIntent{AmountPaise: 100, PaymentSessionID: &sess, Status: domain.IntentCreated}
	intents := &stubIntents{existing: existing}
	cf := &stubCF{}
	svc := New(intents, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 200, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}
	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "9999999999")
	if err != nil {
		t.Fatal(err)
	}
	if cf.n != 1 || intent.PaymentSessionID != "sess_1" {
		t.Fatalf("stale amount must create a new order: n=%d session=%s", cf.n, intent.PaymentSessionID)
	}
}

func TestEnsureCashfreeReusesMatchingAmount(t *testing.T) {
	sess := "keep_sess"
	existing := &domain.PaymentIntent{AmountPaise: 100, PaymentSessionID: &sess, Status: domain.IntentCreated}
	intents := &stubIntents{existing: existing}
	cf := &stubCF{}
	svc := New(intents, cf)
	due := &domain.Due{ID: uuid.New(), DueCode: "ABC123", Amount: 100, Status: domain.DueStatusPending}
	prop := &domain.Property{PaymentMode: domain.PaymentModeCashfree}
	intent, _, err := svc.PayIntent(context.Background(), due, prop, "", "/x", "9999999999")
	if err != nil {
		t.Fatal(err)
	}
	if cf.n != 0 || intent.PaymentSessionID != "keep_sess" {
		t.Fatalf("matching amount must reuse session: n=%d session=%s", cf.n, intent.PaymentSessionID)
	}
}

func TestPNGURL(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	if got := PNGURL("owner", id); got != "/api/owner/dues/11111111-1111-1111-1111-111111111111/qr" {
		t.Fatalf("unexpected owner PNG URL: %s", got)
	}
	if got := PNGURL("tenant", id); got != "/api/tenant/dues/11111111-1111-1111-1111-111111111111/qr" {
		t.Fatalf("unexpected tenant PNG URL: %s", got)
	}
}
