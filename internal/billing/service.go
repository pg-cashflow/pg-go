package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/qr"
)

var (
	ErrOpenDueExists  = errors.New("billing: open rent due already exists")
	ErrNoOpenRentDue  = errors.New("billing: no open rent due")
	ErrDueNotWaivable = errors.New("billing: due cannot be waived")
)

// Service creates and adjusts dues (rent, deposit, prorate, waive).
type Service struct {
	dues    DueRepository
	tenants TenantRepository
	pub     events.Publisher
	now     func() time.Time
	onProrate func(ctx context.Context, due *domain.Due, original, prorated int64)
}

func NewService(dues DueRepository, tenants TenantRepository, pub events.Publisher) *Service {
	return &Service{
		dues:    dues,
		tenants: tenants,
		pub:     pub,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// CreateRentDue creates the next rent due for tenant.
// Returns ErrOpenDueExists if a pending|partial rent due already exists.
// Applies credit_balance_paise toward the new due and may mark it paid.
func (s *Service) CreateRentDue(ctx context.Context, tenant *domain.Tenant) (*domain.Due, error) {
	if tenant == nil {
		return nil, fmt.Errorf("billing: tenant is required")
	}

	today := calendarDateIST(s.now())
	periodEnd := today.AddDate(0, 1, -1)

	due := &domain.Due{
		TenantID:       tenant.ID,
		PropertyID:     tenant.PropertyID,
		Kind:           domain.DueKindRent,
		Amount:         tenant.RentAmount,
		OriginalAmount: tenant.RentAmount,
		PeriodStart:    today,
		PeriodEnd:      periodEnd,
		DueDate:        today,
		Status:         domain.DueStatusPending,
	}

	if err := s.insertDueWithCodeRetry(ctx, due); err != nil {
		return nil, err
	}

	if err := s.publishDueCreated(ctx, due); err != nil {
		return nil, err
	}

	if tenant.CreditBalancePaise > 0 {
		if err := s.applyCreditToDue(ctx, tenant, due); err != nil {
			return nil, err
		}
	}
	return due, nil
}

// CreateDepositDue creates a one-time deposit due.
func (s *Service) CreateDepositDue(ctx context.Context, tenant *domain.Tenant, amountPaise int64) (*domain.Due, error) {
	if tenant == nil {
		return nil, fmt.Errorf("billing: tenant is required")
	}
	if amountPaise <= 0 {
		return nil, fmt.Errorf("billing: deposit amount must be positive")
	}
	today := calendarDateIST(s.now())
	due := &domain.Due{
		TenantID:       tenant.ID,
		PropertyID:     tenant.PropertyID,
		Kind:           domain.DueKindDeposit,
		Amount:         amountPaise,
		OriginalAmount: amountPaise,
		PeriodStart:    today,
		PeriodEnd:      today,
		DueDate:        today,
		Status:         domain.DueStatusPending,
	}
	if err := s.insertDueWithCodeRetry(ctx, due); err != nil {
		return nil, err
	}
	if err := s.publishDueCreated(ctx, due); err != nil {
		return nil, err
	}
	return due, nil
}

// WaiveDue sets status=waived and publishes DueWaived.
func (s *Service) WaiveDue(ctx context.Context, dueID uuid.UUID) (*domain.Due, error) {
	due, err := s.dues.GetByID(ctx, dueID)
	if err != nil {
		return nil, err
	}
	if due.Status == domain.DueStatusPaid || due.Status == domain.DueStatusWaived {
		return nil, ErrDueNotWaivable
	}
	due.Status = domain.DueStatusWaived
	if err := s.dues.Update(ctx, due); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{
		"due_id":       due.ID.String(),
		"due_code":     due.DueCode,
		"amount_paise": due.Amount,
	})
	did := due.ID
	if err := s.pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(due.TenantID),
		PropertyID: due.PropertyID,
		EventType:  domain.EvtDueWaived,
		DueID:      &did,
		OccurredAt: s.now(),
		Payload:    payload,
	}); err != nil {
		return nil, err
	}
	return due, nil
}

// Prorate finds the open rent due and reduces amount for mid-cycle vacate.
func (s *Service) Prorate(ctx context.Context, tenantID uuid.UUID, vacateDate time.Time) (*domain.Due, error) {
	due, err := s.findOpenRentDue(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	vacate := dateOnly(vacateDate)
	daysOccupied := domain.DaysInPeriod(due.PeriodStart, vacate)
	daysInPeriod := domain.DaysInPeriod(due.PeriodStart, due.PeriodEnd)
	prorated := domain.ProrateAmount(due.OriginalAmount, daysOccupied, daysInPeriod)
	due.Prorate(prorated)
	due.ContractualCeilingPaise = &prorated
	if err := s.dues.Update(ctx, due); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(domain.DueProratedPayload{
		DueID:         due.ID.String(),
		OriginalPaise: int64(due.OriginalAmount),
		ProratedPaise: int64(prorated),
		DaysOccupied:  daysOccupied,
		DaysInPeriod:  daysInPeriod,
	})
	did := due.ID
	if err := s.pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(due.TenantID),
		PropertyID: due.PropertyID,
		EventType:  domain.EvtDueProrated,
		DueID:      &did,
		OccurredAt: s.now(),
		Payload:    payload,
	}); err != nil {
		return nil, err
	}
	if s.onProrate != nil {
		s.onProrate(ctx, due, int64(due.OriginalAmount), int64(prorated))
	}
	return due, nil
}

func (s *Service) SetProrateHook(fn func(ctx context.Context, due *domain.Due, original, prorated int64)) {
	s.onProrate = fn
}

func (s *Service) applyCreditToDue(ctx context.Context, tenant *domain.Tenant, due *domain.Due) error {
	if tenant.CreditBalancePaise <= 0 || due.Amount <= 0 {
		return nil
	}
	applied := tenant.CreditBalancePaise
	if applied > due.Amount {
		applied = due.Amount
	}
	due.Amount -= applied
	tenant.CreditBalancePaise -= applied
	at := s.now()
	if due.Amount == 0 {
		due.MarkPaid(at)
	} else if due.Status == domain.DueStatusPending {
		// remaining after partial credit still pending until payment
	}

	if err := s.dues.Update(ctx, due); err != nil {
		return err
	}
	if err := s.tenants.Update(ctx, tenant); err != nil {
		return err
	}

	payload, _ := json.Marshal(domain.CreditAppliedPayload{
		TenantID:       tenant.ID.String(),
		DueID:          due.ID.String(),
		CreditPaise:    int64(applied),
		RemainingPaise: int64(tenant.CreditBalancePaise),
	})
	did := due.ID
	if err := s.pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(tenant.ID),
		PropertyID: tenant.PropertyID,
		EventType:  domain.EvtCreditApplied,
		DueID:      &did,
		OccurredAt: at,
		Payload:    payload,
	}); err != nil {
		return err
	}

	if due.Status == domain.DueStatusPaid {
		return publishDuePaid(ctx, s.pub, due, at, "credit")
	}
	return nil
}

func (s *Service) insertDueWithCodeRetry(ctx context.Context, due *domain.Due) error {
	_, err := qr.GenerateDueCode(func(code string) error {
		due.DueCode = code
		err := s.dues.Create(ctx, due)
		if err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == "uq_dues_tenant_rent_cycle" {
				// Duplicate rent cycle for the same period already billed; do not retry due_code
				return ErrOpenDueExists
			}
			return qr.ErrConflict
		}
		return err
	})
	return err
}

func (s *Service) publishDueCreated(ctx context.Context, due *domain.Due) error {
	payload, _ := json.Marshal(domain.DueCreatedPayload{
		DueID:       due.ID.String(),
		DueCode:     due.DueCode,
		Kind:        string(due.Kind),
		AmountPaise: int64(due.OriginalAmount),
	})
	did := due.ID
	return s.pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(due.TenantID),
		PropertyID: due.PropertyID,
		EventType:  domain.EvtDueCreated,
		DueID:      &did,
		OccurredAt: s.now(),
		Payload:    payload,
	})
}

func (s *Service) findOpenRentDue(ctx context.Context, tenantID uuid.UUID) (*domain.Due, error) {
	list, err := s.dues.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		d := &list[i]
		if d.Kind == domain.DueKindRent &&
			(d.Status == domain.DueStatusPending || d.Status == domain.DueStatusPartial) {
			return d, nil
		}
	}
	return nil, ErrNoOpenRentDue
}

func publishDuePaid(ctx context.Context, pub events.Publisher, due *domain.Due, paidAt time.Time, matchedBy string) error {
	days := daysEarlyOrLate(due.DueDate, paidAt)
	evt := domain.EvtDuePaidOnTime
	if days > 0 {
		evt = domain.EvtDuePaidLate
	}
	amount := due.OriginalAmount
	if due.Amount == 0 && due.PaidAt != nil {
		// remaining was cleared; report original for analytics when fully paid from creation credit
		amount = due.OriginalAmount
	}
	payload, _ := json.Marshal(domain.DuePaidPayload{
		DueID:           due.ID.String(),
		DueCode:         due.DueCode,
		AmountPaise:     int64(amount),
		PaidAt:          paidAt.UTC().Format(time.RFC3339),
		DaysEarlyOrLate: days,
		MatchedBy:       matchedBy,
	})
	did := due.ID
	return pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(due.TenantID),
		PropertyID: due.PropertyID,
		EventType:  evt,
		DueID:      &did,
		OccurredAt: paidAt,
		Payload:    payload,
	})
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func calendarDateIST(now time.Time) time.Time {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+30*60)
	}
	local := now.In(loc)
	// Store as UTC midnight of the IST calendar day for DATE columns.
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func daysEarlyOrLate(dueDate, paidAt time.Time) int {
	d := dateOnly(dueDate)
	p := dateOnly(paidAt)
	return int(p.Sub(d).Hours() / 24)
}
