package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

var (
	ErrInvalidDueDay = errors.New("tenant: due_day must be between 1 and 28")
	ErrInvalidPhone  = errors.New("tenant: phone is required")
	ErrNotFound      = errors.New("tenant: not found")
)

// Service manages tenant lifecycle.
type Service struct {
	tenants Repository
	push    PushRepository
	billing DepositDueCreator
	pub     events.Publisher
	now     func() time.Time

	// Optional TX wiring for CreateTenant atomicity.
	pool     *pgxpool.Pool
	tenantDB *postgres.TenantRepo
	dueDB    *postgres.DueRepo
	eventDB  *postgres.EventRepo
}

func NewService(tenants Repository, push PushRepository, billing DepositDueCreator, pub events.Publisher) *Service {
	return &Service{
		tenants: tenants,
		push:    push,
		billing: billing,
		pub:     pub,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// NewServiceWithPool enables transactional CreateTenant (tenant + events + deposit).
func NewServiceWithPool(
	pool *pgxpool.Pool,
	tenants *postgres.TenantRepo,
	dues *postgres.DueRepo,
	eventsRepo *postgres.EventRepo,
	push PushRepository,
) *Service {
	pub := events.NewPostgresPublisher(eventsRepo)
	bill := billing.NewService(dues, tenants, pub)
	s := NewService(tenants, push, bill, pub)
	s.pool = pool
	s.tenantDB = tenants
	s.dueDB = dues
	s.eventDB = eventsRepo
	return s
}

func normalizeNewTenant(in domain.NewTenantInput, depositPaise int64) (domain.NewTenantInput, int64, error) {
	if in.DueDay < 1 || in.DueDay > 28 {
		return in, 0, ErrInvalidDueDay
	}
	if in.Name == "" {
		return in, 0, fmt.Errorf("tenant: name is required")
	}
	if in.RentAmount <= 0 {
		return in, 0, fmt.Errorf("tenant: rent_amount must be positive")
	}
	if in.NoticePeriodDays <= 0 {
		in.NoticePeriodDays = 30
	}
	if depositPaise <= 0 {
		depositPaise = in.RentAmount
	}
	return in, depositPaise, nil
}

// CreateTenant validates due_day, creates the tenant, deposit due, and onboarding events.
// If depositPaise <= 0, deposit defaults to rent_amount.
func (s *Service) CreateTenant(ctx context.Context, in domain.NewTenantInput, depositPaise int64) (*domain.Tenant, error) {
	in, depositPaise, err := normalizeNewTenant(in, depositPaise)
	if err != nil {
		return nil, err
	}

	if s.pool != nil && s.tenantDB != nil && s.dueDB != nil && s.eventDB != nil {
		var out *domain.Tenant
		err := postgres.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
			t, err := s.createTenantOnTx(ctx, tx, in, depositPaise)
			if err != nil {
				return err
			}
			out = t
			return nil
		})
		return out, err
	}

	return createTenantCore(ctx, s.tenants, s.billing, s.pub, s.now(), in, depositPaise)
}

// CreateTenantTx creates tenant + deposit + events on an already-open transaction.
// Callers that wrap Activate (or similar) must use this instead of CreateTenant to avoid a nested Begin.
func (s *Service) CreateTenantTx(ctx context.Context, tx pgx.Tx, in domain.NewTenantInput, depositPaise int64) (*domain.Tenant, error) {
	in, depositPaise, err := normalizeNewTenant(in, depositPaise)
	if err != nil {
		return nil, err
	}
	if s.tenantDB == nil || s.dueDB == nil || s.eventDB == nil {
		return nil, fmt.Errorf("tenant: tx create requires pool wiring")
	}
	return s.createTenantOnTx(ctx, tx, in, depositPaise)
}

func (s *Service) createTenantOnTx(ctx context.Context, tx pgx.Tx, in domain.NewTenantInput, depositPaise int64) (*domain.Tenant, error) {
	tenants := s.tenantDB.WithTx(tx)
	dues := s.dueDB.WithTx(tx)
	pub := events.NewPostgresPublisher(s.eventDB.WithTx(tx))
	bill := billing.NewService(dues, tenants, pub)
	return createTenantCore(ctx, tenants, bill, pub, s.now(), in, depositPaise)
}

func createTenantCore(
	ctx context.Context,
	tenants Repository,
	bill DepositDueCreator,
	pub events.Publisher,
	at time.Time,
	in domain.NewTenantInput,
	depositPaise int64,
) (*domain.Tenant, error) {
	dueDay := in.DueDay
	t := &domain.Tenant{
		PropertyID:         in.PropertyID,
		Name:               in.Name,
		Phone:              in.Phone,
		RoomNumber:         in.RoomNumber,
		AadhaarLast4:       in.AadhaarLast4,
		RentAmount:         in.RentAmount,
		DueDay:             &dueDay,
		NoticePeriodDays:   in.NoticePeriodDays,
		Status:             domain.TenantStatusActive,
		PermanentAddress:   in.PermanentAddress,
		CurrentAddress:     in.CurrentAddress,
		ParentName:         in.ParentName,
		EmergencyPhone:     in.EmergencyPhone,
		JoinedOn:           in.JoinedOn,
		IDPhotoBytes:       in.IDPhotoBytes,
	}
	if err := tenants.Create(ctx, t); err != nil {
		return nil, err
	}

	if err := pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(t.ID),
		PropertyID: t.PropertyID,
		EventType:  domain.EvtTenantCreated,
		OccurredAt: at,
		Payload:    json.RawMessage(`{}`),
	}); err != nil {
		return nil, err
	}

	termsPayload, _ := json.Marshal(domain.DepositTermsAcceptedPayload{
		Channel:          "onboarding",
		NoticePeriodDays: int(t.NoticePeriodDays),
	})
	if err := pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(t.ID),
		PropertyID: t.PropertyID,
		EventType:  domain.EvtDepositTermsAccepted,
		OccurredAt: at,
		Payload:    termsPayload,
	}); err != nil {
		return nil, err
	}

	if bill != nil {
		if _, err := bill.CreateDepositDue(ctx, t, depositPaise); err != nil {
			return nil, fmt.Errorf("tenant: create deposit due: %w", err)
		}
	}
	return t, nil
}

// CreatePendingFromOnboarding creates a tenant with pending_allocation and no deposit due.
func (s *Service) CreatePendingFromOnboarding(ctx context.Context, in domain.NewTenantInput) (*domain.Tenant, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("tenant: name is required")
	}
	if in.NoticePeriodDays <= 0 {
		in.NoticePeriodDays = 30
	}
	t := &domain.Tenant{
		PropertyID:       in.PropertyID,
		Name:             strings.TrimSpace(in.Name),
		Phone:            in.Phone,
		RentAmount:       0,
		DueDay:           nil,
		NoticePeriodDays: in.NoticePeriodDays,
		Status:           domain.TenantStatusPendingAllocation,
		PermanentAddress: strings.TrimSpace(in.PermanentAddress),
		CurrentAddress:   strings.TrimSpace(in.CurrentAddress),
		ParentName:       strings.TrimSpace(in.ParentName),
		EmergencyPhone:   strings.TrimSpace(in.EmergencyPhone),
		JoinedOn:         in.JoinedOn,
		IDPhotoBytes:     in.IDPhotoBytes,
		HasIDPhoto:       len(in.IDPhotoBytes) > 0,
	}

	create := func(tenants Repository, pub events.Publisher, at time.Time) (*domain.Tenant, error) {
		if err := tenants.Create(ctx, t); err != nil {
			return nil, err
		}
		if err := pub.Publish(ctx, domain.Event{
			TenantID:   domain.Ptr(t.ID),
			PropertyID: t.PropertyID,
			EventType:  domain.EvtTenantCreated,
			OccurredAt: at,
			Payload:    json.RawMessage(`{"status":"pending_allocation"}`),
		}); err != nil {
			return nil, err
		}
		return t, nil
	}

	if s.pool != nil && s.tenantDB != nil && s.eventDB != nil {
		var out *domain.Tenant
		err := postgres.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
			tenants := s.tenantDB.WithTx(tx)
			pub := events.NewPostgresPublisher(s.eventDB.WithTx(tx))
			got, err := create(tenants, pub, s.now())
			if err != nil {
				return err
			}
			out = got
			return nil
		})
		return out, err
	}
	return create(s.tenants, s.pub, s.now())
}

// CreatePendingFromOnboardingTx creates pending_allocation tenant on an open transaction.
func (s *Service) CreatePendingFromOnboardingTx(ctx context.Context, tx pgx.Tx, in domain.NewTenantInput) (*domain.Tenant, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("tenant: name is required")
	}
	if in.NoticePeriodDays <= 0 {
		in.NoticePeriodDays = 30
	}
	if s.tenantDB == nil || s.eventDB == nil {
		return nil, fmt.Errorf("tenant: tx create requires pool wiring")
	}
	t := &domain.Tenant{
		PropertyID:       in.PropertyID,
		Name:             strings.TrimSpace(in.Name),
		Phone:            in.Phone,
		RentAmount:       0,
		DueDay:           nil,
		NoticePeriodDays: in.NoticePeriodDays,
		Status:           domain.TenantStatusPendingAllocation,
		PermanentAddress: strings.TrimSpace(in.PermanentAddress),
		CurrentAddress:   strings.TrimSpace(in.CurrentAddress),
		ParentName:       strings.TrimSpace(in.ParentName),
		EmergencyPhone:   strings.TrimSpace(in.EmergencyPhone),
		JoinedOn:         in.JoinedOn,
		IDPhotoBytes:     in.IDPhotoBytes,
		HasIDPhoto:       len(in.IDPhotoBytes) > 0,
	}
	tenants := s.tenantDB.WithTx(tx)
	pub := events.NewPostgresPublisher(s.eventDB.WithTx(tx))
	if err := tenants.Create(ctx, t); err != nil {
		return nil, err
	}
	if err := pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(t.ID),
		PropertyID: t.PropertyID,
		EventType:  domain.EvtTenantCreated,
		OccurredAt: s.now(),
		Payload:    json.RawMessage(`{"status":"pending_allocation"}`),
	}); err != nil {
		return nil, err
	}
	return t, nil
}

// AssignTerms flips pending_allocation → active and creates the deposit due.
func (s *Service) AssignTerms(ctx context.Context, tenantID uuid.UUID, room *string, rentAmount int64, dueDay int16, depositPaise int64, noticePeriodDays int16) (*domain.Tenant, error) {
	if dueDay < 1 || dueDay > 28 {
		return nil, ErrInvalidDueDay
	}
	if rentAmount <= 0 {
		return nil, fmt.Errorf("tenant: rent_amount must be positive")
	}
	if noticePeriodDays <= 0 {
		noticePeriodDays = 30
	}
	if depositPaise <= 0 {
		depositPaise = rentAmount
	}

	assign := func(tenants Repository, bill DepositDueCreator, pub events.Publisher, at time.Time) (*domain.Tenant, error) {
		t, err := tenants.GetByID(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		if t.Status != domain.TenantStatusPendingAllocation {
			return nil, fmt.Errorf("tenant: not awaiting room/rent assignment")
		}
		t.RoomNumber = room
		t.RentAmount = rentAmount
		t.DueDay = &dueDay
		t.NoticePeriodDays = noticePeriodDays
		t.Status = domain.TenantStatusActive
		if err := tenants.Update(ctx, t); err != nil {
			return nil, err
		}
		termsPayload, _ := json.Marshal(domain.DepositTermsAcceptedPayload{
			Channel:          "assign_terms",
			NoticePeriodDays: int(t.NoticePeriodDays),
		})
		if err := pub.Publish(ctx, domain.Event{
			TenantID:   domain.Ptr(t.ID),
			PropertyID: t.PropertyID,
			EventType:  domain.EvtDepositTermsAccepted,
			OccurredAt: at,
			Payload:    termsPayload,
		}); err != nil {
			return nil, err
		}
		if bill != nil {
			if _, err := bill.CreateDepositDue(ctx, t, depositPaise); err != nil {
				return nil, fmt.Errorf("tenant: create deposit due: %w", err)
			}
		}
		return t, nil
	}

	if s.pool != nil && s.tenantDB != nil && s.dueDB != nil && s.eventDB != nil {
		var out *domain.Tenant
		err := postgres.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
			tenants := s.tenantDB.WithTx(tx)
			dues := s.dueDB.WithTx(tx)
			pub := events.NewPostgresPublisher(s.eventDB.WithTx(tx))
			bill := billing.NewService(dues, tenants, pub)
			got, err := assign(tenants, bill, pub, s.now())
			if err != nil {
				return err
			}
			out = got
			return nil
		})
		return out, err
	}
	return assign(s.tenants, s.billing, s.pub, s.now())
}

// AssignTermsTx assigns room/rent on an open transaction.
func (s *Service) AssignTermsTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, room *string, rentAmount int64, dueDay int16, depositPaise int64, noticePeriodDays int16) (*domain.Tenant, error) {
	if dueDay < 1 || dueDay > 28 {
		return nil, ErrInvalidDueDay
	}
	if rentAmount <= 0 {
		return nil, fmt.Errorf("tenant: rent_amount must be positive")
	}
	if noticePeriodDays <= 0 {
		noticePeriodDays = 30
	}
	if depositPaise <= 0 {
		depositPaise = rentAmount
	}
	if s.tenantDB == nil || s.dueDB == nil || s.eventDB == nil {
		return nil, fmt.Errorf("tenant: tx assign requires pool wiring")
	}
	tenants := s.tenantDB.WithTx(tx)
	dues := s.dueDB.WithTx(tx)
	pub := events.NewPostgresPublisher(s.eventDB.WithTx(tx))
	bill := billing.NewService(dues, tenants, pub)
	t, err := tenants.GetByID(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if t.Status != domain.TenantStatusPendingAllocation {
		return nil, fmt.Errorf("tenant: not awaiting room/rent assignment")
	}
	t.RoomNumber = room
	t.RentAmount = rentAmount
	t.DueDay = &dueDay
	t.NoticePeriodDays = noticePeriodDays
	t.Status = domain.TenantStatusActive
	if err := tenants.Update(ctx, t); err != nil {
		return nil, err
	}
	termsPayload, _ := json.Marshal(domain.DepositTermsAcceptedPayload{
		Channel:          "assign_terms",
		NoticePeriodDays: int(t.NoticePeriodDays),
	})
	if err := pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(t.ID),
		PropertyID: t.PropertyID,
		EventType:  domain.EvtDepositTermsAccepted,
		OccurredAt: s.now(),
		Payload:    termsPayload,
	}); err != nil {
		return nil, err
	}
	if _, err := bill.CreateDepositDue(ctx, t, depositPaise); err != nil {
		return nil, fmt.Errorf("tenant: create deposit due: %w", err)
	}
	return t, nil
}

// UpdateTenant updates mutable fields. Rent changes publish RentAmountChanged and never touch dues.
func (s *Service) UpdateTenant(ctx context.Context, t *domain.Tenant) error {
	if t.Status == domain.TenantStatusPendingAllocation {
		return fmt.Errorf("tenant: assign room and rent before editing")
	}
	if t.DueDay == nil || *t.DueDay < 1 || *t.DueDay > 28 {
		return ErrInvalidDueDay
	}
	if t.RentAmount <= 0 {
		return fmt.Errorf("tenant: rent_amount must be positive")
	}
	existing, err := s.tenants.GetByID(ctx, t.ID)
	if err != nil {
		return err
	}
	oldRent := existing.RentAmount
	if err := s.tenants.Update(ctx, t); err != nil {
		return err
	}
	if t.RentAmount != oldRent {
		at := s.now()
		payload, _ := json.Marshal(domain.RentAmountChangedPayload{
			TenantID:      t.ID.String(),
			OldPaise:      int64(oldRent),
			NewPaise:      int64(t.RentAmount),
			EffectiveFrom: at.UTC().Format("2006-01-02"),
		})
		if err := s.pub.Publish(ctx, domain.Event{
			TenantID:   domain.Ptr(t.ID),
			PropertyID: t.PropertyID,
			EventType:  domain.EvtRentAmountChanged,
			OccurredAt: at,
			Payload:    payload,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Vacate marks tenant vacated, deletes push subscriptions, publishes TenantVacated.
// Payment tokens are intentionally left intact.
func (s *Service) Vacate(ctx context.Context, tenantID uuid.UUID) error {
	t, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return err
	}
	t.Status = domain.TenantStatusVacated
	if err := s.tenants.Update(ctx, t); err != nil {
		return err
	}
	if s.push != nil {
		if err := s.push.DeleteByTenant(ctx, tenantID); err != nil {
			return fmt.Errorf("tenant: delete push subscriptions: %w", err)
		}
	}
	return s.pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(t.ID),
		PropertyID: t.PropertyID,
		EventType:  domain.EvtTenantVacated,
		OccurredAt: s.now(),
		Payload:    json.RawMessage(`{}`),
	})
}

// LogNotice sets notice_given_at and publishes NoticeGiven.
func (s *Service) LogNotice(ctx context.Context, tenantID uuid.UUID, givenAt time.Time) error {
	t, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return err
	}
	at := givenAt.UTC()
	if at.IsZero() {
		at = s.now()
	}
	t.NoticeGivenAt = &at
	if err := s.tenants.Update(ctx, t); err != nil {
		return err
	}
	expected := at.AddDate(0, 0, int(t.NoticePeriodDays))
	payload, _ := json.Marshal(domain.NoticeGivenPayload{
		TenantID:         t.ID.String(),
		NoticeGivenAt:    at.Format(time.RFC3339),
		NoticePeriodDays: int(t.NoticePeriodDays),
		ExpectedVacateBy: expected.Format("2006-01-02"),
	})
	return s.pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(t.ID),
		PropertyID: t.PropertyID,
		EventType:  domain.EvtNoticeGiven,
		OccurredAt: at,
		Payload:    payload,
	})
}

// AttachPhone sets phone and publishes PhoneAttached (phone-less → self-service upgrade).
func (s *Service) AttachPhone(ctx context.Context, tenantID uuid.UUID, phone string) error {
	if phone == "" {
		return ErrInvalidPhone
	}
	t, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return err
	}
	t.Phone = &phone
	if err := s.tenants.Update(ctx, t); err != nil {
		return err
	}
	payload, _ := json.Marshal(domain.PhoneAttachedPayload{
		TenantID: t.ID.String(),
		Phone:    phone,
	})
	return s.pub.Publish(ctx, domain.Event{
		TenantID:   domain.Ptr(t.ID),
		PropertyID: t.PropertyID,
		EventType:  domain.EvtPhoneAttached,
		OccurredAt: s.now(),
		Payload:    payload,
	})
}

// ApplyCredit adds paise to the tenant's credit_balance_paise atomically.
func (s *Service) ApplyCredit(ctx context.Context, tenantID uuid.UUID, creditPaise int64) error {
	if creditPaise == 0 {
		return nil
	}
	if adder, ok := s.tenants.(interface {
		AddCredit(ctx context.Context, id uuid.UUID, deltaPaise int64) error
	}); ok {
		return adder.AddCredit(ctx, tenantID, creditPaise)
	}
	if locker, ok := s.tenants.(interface {
		GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	}); ok {
		t, err := locker.GetByIDForUpdate(ctx, tenantID)
		if err != nil {
			return err
		}
		t.CreditBalancePaise += creditPaise
		if t.CreditBalancePaise < 0 {
			t.CreditBalancePaise = 0
		}
		return s.tenants.Update(ctx, t)
	}
	return errors.New("tenant: repository does not support atomic credit mutation")
}
