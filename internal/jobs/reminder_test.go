package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubTenantGetter struct {
	t *domain.Tenant
}

func (s stubTenantGetter) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return s.t, nil
}

type stubPropertyGetter struct {
	p *domain.Property
}

func (s stubPropertyGetter) GetByID(ctx context.Context, id uuid.UUID) (*domain.Property, error) {
	return s.p, nil
}

type stubReminderLogger struct {
	calls  int
	logged int
	exists bool
}

func (s *stubReminderLogger) Exists(ctx context.Context, dueID uuid.UUID, reminderType, channel string) (bool, error) {
	return s.exists, nil
}

func (s *stubReminderLogger) TryLog(ctx context.Context, dueID uuid.UUID, reminderType, channel string) (bool, error) {
	s.calls++
	if s.exists {
		return false, nil
	}
	s.logged++
	return true, nil
}

func (s *stubReminderLogger) DeleteLog(ctx context.Context, dueID uuid.UUID, reminderType, channel string) error {
	s.logged--
	return nil
}

type failingSMS struct {
	n int
}

func (c *failingSMS) Send(ctx context.Context, phone, message string) error {
	c.n++
	return errors.New("gateway down")
}

type stubImportRecency struct {
	at  *time.Time
	err error
}

func (s stubImportRecency) LatestImportedAt(ctx context.Context, propertyID uuid.UUID) (*time.Time, error) {
	return s.at, s.err
}

type countingSMS struct {
	n int
}

func (c *countingSMS) Send(ctx context.Context, phone, message string) error {
	c.n++
	return nil
}

func TestReminder_SkipVacated(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	phone := "9876543210"
	tenantID := uuid.New()
	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID:     tenantID,
			Phone:  &phone,
			Status: domain.TenantStatusVacated,
		}},
		Reminders: &stubReminderLogger{},
		SMS:       &countingSMS{},
	}
	due := domain.Due{
		ID:       uuid.New(),
		TenantID: tenantID,
		DueDate:  today, // D-0
		Amount:   10000,
	}
	sms := job.SMS.(*countingSMS)
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue: %v", err)
	}
	if sms.n != 0 {
		t.Fatalf("expected vacated tenant to skip SMS, got %d sends", sms.n)
	}
}

func TestReminder_SkipPhoneLess(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	tenantID := uuid.New()
	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID:     tenantID,
			Phone:  nil,
			Status: domain.TenantStatusActive,
		}},
		Reminders: &stubReminderLogger{},
		SMS:       &countingSMS{},
	}
	due := domain.Due{
		ID:       uuid.New(),
		TenantID: tenantID,
		DueDate:  today,
		Amount:   10000,
	}
	sms := job.SMS.(*countingSMS)
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue: %v", err)
	}
	if sms.n != 0 {
		t.Fatalf("expected phone-less tenant to skip SMS, got %d sends", sms.n)
	}
}

func TestReminder_ImportRecencyGate(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	phone := "9876543210"
	tenantID := uuid.New()
	propID := uuid.New()

	// Stale import → D+1 must not send
	stale := time.Now().Add(-48 * time.Hour)
	sms := &countingSMS{}
	logs := &stubReminderLogger{}
	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID:     tenantID,
			Phone:  &phone,
			Status: domain.TenantStatusActive,
		}},
		Properties: stubPropertyGetter{p: &domain.Property{ID: propID, OwnerEmail: "o@example.com"}},
		Reminders:  logs,
		Imports:    stubImportRecency{at: &stale},
		SMS:        sms,
		BaseURL:    "https://pay.example.com",
	}
	due := domain.Due{
		ID:         uuid.New(),
		TenantID:   tenantID,
		PropertyID: propID,
		DueDate:    today.AddDate(0, 0, -1), // D+1
		Amount:     10000,
		DueCode:    "ABC123",
	}
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue stale: %v", err)
	}
	if sms.n != 0 {
		t.Fatalf("expected stale import to block D+1, got %d sends", sms.n)
	}

	// Fresh import → D+1 sends
	fresh := time.Now().Add(-1 * time.Hour)
	sms2 := &countingSMS{}
	job.Imports = stubImportRecency{at: &fresh}
	job.SMS = sms2
	job.Reminders = &stubReminderLogger{}
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue fresh: %v", err)
	}
	if sms2.n != 1 {
		t.Fatalf("expected fresh import to allow D+1 SMS, got %d sends", sms2.n)
	}
}

func TestReminder_CashfreeSkipsCSVGate(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	phone := "9876543210"
	tenantID := uuid.New()
	propID := uuid.New()
	stale := time.Now().Add(-48 * time.Hour)
	sms := &countingSMS{}
	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID: tenantID, Phone: &phone, Status: domain.TenantStatusActive,
		}},
		Properties: stubPropertyGetter{p: &domain.Property{
			ID: propID, OwnerEmail: "o@example.com", PaymentMode: domain.PaymentModeCashfree,
		}},
		Reminders: &stubReminderLogger{},
		Imports:   stubImportRecency{at: &stale},
		SMS:       sms,
		BaseURL:   "https://pay.example.com",
	}
	due := domain.Due{
		ID: uuid.New(), TenantID: tenantID, PropertyID: propID,
		DueDate: today.AddDate(0, 0, -1), Amount: 10000, DueCode: "CF1234",
	}
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue: %v", err)
	}
	if sms.n != 1 {
		t.Fatalf("cashfree overdue must not wait on CSV, got %d sends", sms.n)
	}
}

func TestReminder_CashfreeStaleIntentBlocks(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	phone := "9876543210"
	tenantID := uuid.New()
	propID := uuid.New()
	stale := time.Now().Add(-48 * time.Hour)
	sms := &countingSMS{}
	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID: tenantID, Phone: &phone, Status: domain.TenantStatusActive,
		}},
		Properties: stubPropertyGetter{p: &domain.Property{
			ID: propID, PaymentMode: domain.PaymentModeCashfree,
		}},
		Reminders: &stubReminderLogger{},
		Intents:   stubIntentRecency{n: 1, at: &stale},
		SMS:       sms,
		BaseURL:   "https://pay.example.com",
	}
	due := domain.Due{
		ID: uuid.New(), TenantID: tenantID, PropertyID: propID,
		DueDate: today.AddDate(0, 0, -1), Amount: 10000, DueCode: "CF1234",
	}
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue: %v", err)
	}
	if sms.n != 0 {
		t.Fatalf("stale cashfree intent must block D+1, got %d sends", sms.n)
	}

	fresh := time.Now().Add(-1 * time.Hour)
	sms2 := &countingSMS{}
	job.Intents = stubIntentRecency{n: 1, at: &fresh}
	job.SMS = sms2
	job.Reminders = &stubReminderLogger{}
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatal(err)
	}
	if sms2.n != 1 {
		t.Fatalf("recent cashfree intent must allow D+1, got %d", sms2.n)
	}
}

type stubIntentRecency struct {
	n  int
	at *time.Time
}

func (s stubIntentRecency) RecencyForDue(context.Context, uuid.UUID) (int, *time.Time, error) {
	return s.n, s.at, nil
}

func TestImportFresh_NilImports(t *testing.T) {
	job := &ReminderJob{}
	ok, err := job.importFresh(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("importFresh: %v", err)
	}
	if ok {
		t.Fatal("expected false when Imports is nil")
	}
}

func TestReminderMessage(t *testing.T) {
	msg := reminderMessage(ReminderD0, 500000, "https://pay.example.com/x")
	if msg == "" {
		t.Fatal("empty message")
	}
}

func TestReminder_SendFailDoesNotLog(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	phone := "9876543210"
	tenantID := uuid.New()
	propID := uuid.New()
	logs := &stubReminderLogger{}
	sms := &failingSMS{}
	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID: tenantID, Phone: &phone, Status: domain.TenantStatusActive, Name: "T",
		}},
		Properties: stubPropertyGetter{p: &domain.Property{ID: propID, OwnerEmail: "o@example.com"}},
		Reminders:  logs,
		SMS:        sms,
		BaseURL:    "https://pay.example.com",
	}
	due := domain.Due{
		ID: uuid.New(), TenantID: tenantID, PropertyID: propID,
		DueDate: today, Amount: 10000, DueCode: "FAIL01",
	}
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue: %v", err)
	}
	if sms.n != 1 {
		t.Fatalf("expected one send attempt, got %d", sms.n)
	}
	if logs.logged != 0 {
		t.Fatalf("send failure must not insert reminder_logs, logged=%d", logs.logged)
	}
}

func TestReminder_AtomicDedupSkipsSecondAttempt(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	phone := "9876543210"
	tenantID := uuid.New()
	propID := uuid.New()
	logs := &stubReminderLogger{exists: true} // Already claimed/logged
	sms := &countingSMS{}
	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID: tenantID, Phone: &phone, Status: domain.TenantStatusActive, Name: "T",
		}},
		Properties: stubPropertyGetter{p: &domain.Property{ID: propID, OwnerEmail: "o@example.com"}},
		Reminders:  logs,
		SMS:        sms,
		BaseURL:    "https://pay.example.com",
	}
	due := domain.Due{
		ID: uuid.New(), TenantID: tenantID, PropertyID: propID,
		DueDate: today, Amount: 10000, DueCode: "DEDUP01",
	}
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue: %v", err)
	}
	if sms.n != 0 {
		t.Fatalf("expected 0 sends when TryLog returns inserted=false, got %d", sms.n)
	}
}

type mapReminderLogger struct {
	logs map[string]bool
}

func newMapReminderLogger() *mapReminderLogger {
	return &mapReminderLogger{logs: make(map[string]bool)}
}

func (m *mapReminderLogger) key(dueID uuid.UUID, remType, channel string) string {
	return dueID.String() + ":" + remType + ":" + channel
}

func (m *mapReminderLogger) Exists(_ context.Context, dueID uuid.UUID, remType, channel string) (bool, error) {
	return m.logs[m.key(dueID, remType, channel)], nil
}

func (m *mapReminderLogger) TryLog(_ context.Context, dueID uuid.UUID, remType, channel string) (bool, error) {
	k := m.key(dueID, remType, channel)
	if m.logs[k] {
		return false, nil
	}
	m.logs[k] = true
	return true, nil
}

func (m *mapReminderLogger) DeleteLog(_ context.Context, dueID uuid.UUID, remType, channel string) error {
	delete(m.logs, m.key(dueID, remType, channel))
	return nil
}

func TestReminder_CatchUp_DMinus2MissedDMinus3(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	phone := "9876543210"
	tenantID := uuid.New()
	propID := uuid.New()
	logs := newMapReminderLogger()
	sms := &countingSMS{}

	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID: tenantID, Phone: &phone, Status: domain.TenantStatusActive, Name: "T",
		}},
		Properties:  stubPropertyGetter{p: &domain.Property{ID: propID, OwnerEmail: "o@example.com"}},
		Reminders:   logs,
		SMS:         sms,
		CatchUpDays: 2,
		BaseURL:     "https://pay.example.com",
	}

	due := domain.Due{
		ID:         uuid.New(),
		TenantID:   tenantID,
		PropertyID: propID,
		DueDate:    today.AddDate(0, 0, 2), // Delta is -2 (missed D-3 yesterday)
		Amount:     10000,
		DueCode:    "CATCH01",
	}

	// First run: Should catch up and send D-3 reminder
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue: %v", err)
	}
	if sms.n != 1 {
		t.Fatalf("expected 1 send on catch-up for missed D-3, got %d", sms.n)
	}

	// Second run: Already logged, should NOT re-send
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue 2nd: %v", err)
	}
	if sms.n != 1 {
		t.Fatalf("expected duplicate run to not re-send, got %d", sms.n)
	}
}

func TestReminder_CatchUp_DPlus2MissedDPlus1(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	phone := "9876543210"
	tenantID := uuid.New()
	propID := uuid.New()
	logs := newMapReminderLogger()
	sms := &countingSMS{}
	fresh := time.Now().Add(-1 * time.Hour)

	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID: tenantID, Phone: &phone, Status: domain.TenantStatusActive, Name: "T",
		}},
		Properties:  stubPropertyGetter{p: &domain.Property{ID: propID, OwnerEmail: "o@example.com", PaymentMode: domain.PaymentModeCashfree}},
		Reminders:   logs,
		SMS:         sms,
		CatchUpDays: 2,
		Intents:     stubIntentRecency{n: 1, at: &fresh},
		BaseURL:     "https://pay.example.com",
	}

	due := domain.Due{
		ID:         uuid.New(),
		TenantID:   tenantID,
		PropertyID: propID,
		DueDate:    today.AddDate(0, 0, -2), // Delta is 2 (missed D+1 yesterday)
		Amount:     10000,
		DueCode:    "CATCH02",
	}

	// First run: Should catch up and send D+1 reminder
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue: %v", err)
	}
	if sms.n != 1 {
		t.Fatalf("expected 1 send on catch-up for missed D+1, got %d", sms.n)
	}

	// Second run: Already logged, should NOT re-send
	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue 2nd: %v", err)
	}
	if sms.n != 1 {
		t.Fatalf("expected duplicate run to not re-send, got %d", sms.n)
	}
}

func TestReminder_CatchUp_BeyondWindowDropped(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := dateOnly(time.Now().In(loc))
	phone := "9876543210"
	tenantID := uuid.New()
	propID := uuid.New()
	logs := newMapReminderLogger()
	sms := &countingSMS{}
	fresh := time.Now().Add(-1 * time.Hour)

	job := &ReminderJob{
		Tenants: stubTenantGetter{t: &domain.Tenant{
			ID: tenantID, Phone: &phone, Status: domain.TenantStatusActive, Name: "T",
		}},
		Properties:  stubPropertyGetter{p: &domain.Property{ID: propID, OwnerEmail: "o@example.com", PaymentMode: domain.PaymentModeCashfree}},
		Reminders:   logs,
		SMS:         sms,
		CatchUpDays: 2, // window is delta 1..3
		Intents:     stubIntentRecency{n: 1, at: &fresh},
		BaseURL:     "https://pay.example.com",
	}

	due := domain.Due{
		ID:         uuid.New(),
		TenantID:   tenantID,
		PropertyID: propID,
		DueDate:    today.AddDate(0, 0, -5), // Delta is 5 (beyond 1+2=3, before 7)
		Amount:     10000,
		DueCode:    "CATCH03",
	}

	if err := job.processDue(context.Background(), due, today, loc); err != nil {
		t.Fatalf("processDue: %v", err)
	}
	if sms.n != 0 {
		t.Fatalf("expected 0 sends when beyond catch-up window, got %d", sms.n)
	}
}

type stubDueLister struct {
	dues []domain.Due
}

func (s stubDueLister) ActivePendingRentDues(ctx context.Context) ([]domain.Due, error) {
	return s.dues, nil
}

type countingBulkTenantGetter struct {
	tenants     map[uuid.UUID]*domain.Tenant
	bulkCalls   int
	singleCalls int
}

func (s *countingBulkTenantGetter) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	s.singleCalls++
	return s.tenants[id], nil
}

func (s *countingBulkTenantGetter) GetByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*domain.Tenant, error) {
	s.bulkCalls++
	res := make(map[uuid.UUID]*domain.Tenant, len(ids))
	for _, id := range ids {
		if t, ok := s.tenants[id]; ok {
			res[id] = t
		}
	}
	return res, nil
}

type countingPropertyGetter struct {
	prop  *domain.Property
	calls int
}

func (s *countingPropertyGetter) GetByID(ctx context.Context, id uuid.UUID) (*domain.Property, error) {
	s.calls++
	return s.prop, nil
}

func TestReminder_Run_BulkTenantGetterAndMemoization(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	today := time.Now().In(loc)

	propID := uuid.New()
	t1ID := uuid.New()
	t2ID := uuid.New()

	phone := "9876543210"
	tenants := map[uuid.UUID]*domain.Tenant{
		t1ID: {ID: t1ID, PropertyID: propID, Phone: &phone, Status: domain.TenantStatusActive},
		t2ID: {ID: t2ID, PropertyID: propID, Phone: &phone, Status: domain.TenantStatusActive},
	}

	bulkTenant := &countingBulkTenantGetter{tenants: tenants}
	propGetter := &countingPropertyGetter{
		prop: &domain.Property{ID: propID, OwnerEmail: "owner@example.com", PaymentMode: domain.PaymentModeCashfree},
	}

	fresh := today.Add(-10 * time.Minute)
	sms := &failingSMS{}
	logs := &stubReminderLogger{exists: false}

	job := &ReminderJob{
		Dues: stubDueLister{
			dues: []domain.Due{
				{ID: uuid.New(), TenantID: t1ID, PropertyID: propID, DueDate: today, Amount: 5000, DueCode: "DUE1"},
				{ID: uuid.New(), TenantID: t1ID, PropertyID: propID, DueDate: today, Amount: 6000, DueCode: "DUE2"},
				{ID: uuid.New(), TenantID: t2ID, PropertyID: propID, DueDate: today, Amount: 7000, DueCode: "DUE3"},
			},
		},
		Tenants:     bulkTenant,
		Properties:  propGetter,
		Reminders:   logs,
		SMS:         sms,
		CatchUpDays: 2,
		Intents:     stubIntentRecency{n: 1, at: &fresh},
		BaseURL:     "https://pay.example.com",
	}

	_ = job.Run(context.Background())

	if bulkTenant.bulkCalls != 1 {
		t.Errorf("expected 1 bulk call, got %d", bulkTenant.bulkCalls)
	}
	if bulkTenant.singleCalls != 0 {
		t.Errorf("expected 0 single tenant calls due to memoization, got %d", bulkTenant.singleCalls)
	}
	if propGetter.calls != 1 {
		t.Errorf("expected 1 property call due to memoization, got %d", propGetter.calls)
	}
}
