package join

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/invite"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/tenant"
)

var (
	ErrInvalidInvite     = errors.New("join: invalid invite code")
	ErrAlreadyActive     = errors.New("join: already an active tenant")
	ErrNotPending        = errors.New("join: no pending join request")
	ErrNameRequired      = errors.New("join: name is required")
	ErrNotFound          = errors.New("join: not found")
	ErrNotPendingOwner   = errors.New("join: request is not pending")
	ErrProfileIncomplete = errors.New("join: profile incomplete")
	ErrPhotoRequired     = errors.New("join: id photo required")
	ErrConsentRequired   = errors.New("join: consent required")
	ErrAlreadyOnboarded  = errors.New("join: already onboarded")
	ErrNotAwaitingAssign = errors.New("join: not awaiting room/rent assignment")
)

const inviteAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
const maxIDPhotoBytes = 2 << 20

func GenerateInviteCode() (string, error) {
	const n = 8
	out := make([]byte, n)
	max := big.NewInt(int64(len(inviteAlphabet)))
	for i := range n {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = inviteAlphabet[v.Int64()]
	}
	return string(out), nil
}

func NormalizeInvite(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

type PropertyStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Property, error)
	GetByInviteCode(ctx context.Context, code string) (*domain.Property, error)
	SetInviteCode(ctx context.Context, id uuid.UUID, code string) error
}

type JoinStore interface {
	Create(ctx context.Context, j *domain.JoinRequest) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.JoinRequest, error)
	GetPendingByUser(ctx context.Context, userID uuid.UUID) (*domain.JoinRequest, error)
	GetLatestByUser(ctx context.Context, userID uuid.UUID) (*domain.JoinRequest, error)
	ListByProperty(ctx context.Context, propertyID uuid.UUID, status *domain.JoinStatus) ([]domain.JoinRequest, error)
	Update(ctx context.Context, j *domain.JoinRequest) error
}

type UserLinker interface {
	SetTenantID(ctx context.Context, userID, tenantID uuid.UUID) error
}

type TenantCreator interface {
	CreateTenant(ctx context.Context, in domain.NewTenantInput, depositPaise int64) (*domain.Tenant, error)
}

type TenantTxCreator interface {
	CreateTenantTx(ctx context.Context, tx pgx.Tx, in domain.NewTenantInput, depositPaise int64) (*domain.Tenant, error)
}

type PendingOnboarder interface {
	CreatePendingFromOnboarding(ctx context.Context, in domain.NewTenantInput) (*domain.Tenant, error)
}

type PendingOnboarderTx interface {
	CreatePendingFromOnboardingTx(ctx context.Context, tx pgx.Tx, in domain.NewTenantInput) (*domain.Tenant, error)
}

type TermsAssigner interface {
	AssignTerms(ctx context.Context, tenantID uuid.UUID, room *string, rentAmount int64, dueDay int16, depositPaise int64, noticePeriodDays int16) (*domain.Tenant, error)
}

type TermsAssignerTx interface {
	AssignTermsTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, room *string, rentAmount int64, dueDay int16, depositPaise int64, noticePeriodDays int16) (*domain.Tenant, error)
}

type Service struct {
	props   PropertyStore
	joins   JoinStore
	users   UserLinker
	tenants TenantCreator
	pub     events.Publisher
	now     func() time.Time

	pool         *pgxpool.Pool
	joinDB       *postgres.JoinRepo
	userDB       *postgres.UserRepo
	eventDB      *postgres.EventRepo
	tenantTx     TenantTxCreator
	tenantSvc    *tenant.Service
	resolver     *invite.Resolver
	slotDB       *postgres.SlotInviteRepo
	onboardingDB *postgres.OnboardingSettingsRepo
}

func NewService(props PropertyStore, joins JoinStore, users UserLinker, tenants TenantCreator, pub events.Publisher) *Service {
	return &Service{props: props, joins: joins, users: users, tenants: tenants, pub: pub, now: func() time.Time { return time.Now().UTC() }}
}

// NewServiceWithPool enables transactional CompleteOnboarding and Activate.
func NewServiceWithPool(
	pool *pgxpool.Pool,
	props PropertyStore,
	joins *postgres.JoinRepo,
	users *postgres.UserRepo,
	tenants *tenant.Service,
	eventsRepo *postgres.EventRepo,
	resolver *invite.Resolver,
	slotInvites *postgres.SlotInviteRepo,
	onboarding *postgres.OnboardingSettingsRepo,
) *Service {
	pub := events.NewPostgresPublisher(eventsRepo)
	s := NewService(props, joins, users, tenants, pub)
	s.pool = pool
	s.joinDB = joins
	s.userDB = users
	s.eventDB = eventsRepo
	s.tenantTx = tenants
	s.tenantSvc = tenants
	s.resolver = resolver
	s.slotDB = slotInvites
	s.onboardingDB = onboarding
	return s
}

func (s *Service) LookupInvite(ctx context.Context, code string) (*domain.Property, error) {
	code = NormalizeInvite(code)
	if code == "" {
		return nil, ErrInvalidInvite
	}
	if s.resolver != nil {
		res, err := s.resolver.Resolve(ctx, code)
		if err != nil {
			if errors.Is(err, invite.ErrInvalidInvite) {
				return nil, ErrInvalidInvite
			}
			return nil, err
		}
		return s.props.GetByID(ctx, res.PropertyID)
	}
	p, err := s.props.GetByInviteCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidInvite
		}
		return nil, err
	}
	return p, nil
}

func (s *Service) RotateInvite(ctx context.Context, propertyID uuid.UUID) (string, error) {
	code, err := GenerateInviteCode()
	if err != nil {
		return "", err
	}
	if err := s.props.SetInviteCode(ctx, propertyID, code); err != nil {
		return "", err
	}
	return code, nil
}

// EnsurePending creates a pending join row for a newly authenticated invite user.
func (s *Service) EnsurePending(ctx context.Context, user *domain.User, propertyID uuid.UUID, slotInviteID *uuid.UUID) (*domain.JoinRequest, error) {
	if user.TenantID != nil {
		return nil, ErrAlreadyActive
	}
	existing, err := s.joins.GetPendingByUser(ctx, user.ID)
	if err == nil {
		return existing, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if slotInviteID != nil && s.slotDB != nil {
		slot, err := s.slotDB.GetByID(ctx, *slotInviteID)
		if err != nil || slot.PropertyID != propertyID || slot.Status != domain.SlotInviteOpen {
			return nil, ErrInvalidInvite
		}
	}
	j := &domain.JoinRequest{
		PropertyID:   propertyID,
		UserID:       user.ID,
		Phone:        user.Phone,
		Name:         "",
		Status:       domain.JoinPending,
		SlotInviteID: slotInviteID,
	}
	if err := s.joins.Create(ctx, j); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"join_id": j.ID.String(), "phone": j.Phone})
	_ = s.pub.Publish(ctx, domain.Event{
		TenantID:   nil,
		PropertyID: propertyID,
		EventType:  domain.EvtJoinRequested,
		OccurredAt: s.now(),
		Payload:    payload,
	})
	return j, nil
}

type ProfileInput struct {
	Name                 string
	PermanentAddress     string
	CurrentAddress       string
	ParentName           string
	EmergencyContactName string
	EmergencyPhone       string
	OccupationType       domain.OccupationType
	DateOfBirth          *time.Time
	Details              domain.OccupationDetails
	GuardianName         string
	GuardianPhone        string
	Consent              bool
	IDPhotoBytes         []byte
}

// SetProfile is kept for tests; prefer CompleteOnboarding for the invite-in flow.
func (s *Service) SetProfile(ctx context.Context, userID uuid.UUID, name string, aadhaarLast4 *string) (*domain.JoinRequest, error) {
	name = strings.TrimSpace(name)
	j, err := s.joins.GetPendingByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotPending
		}
		return nil, err
	}
	if name != "" {
		j.Name = name
	}
	if aadhaarLast4 != nil {
		j.AadhaarLast4 = aadhaarLast4
	}
	if err := s.joins.Update(ctx, j); err != nil {
		return nil, err
	}
	return j, nil
}

// CompleteOnboarding saves profile + photo, creates pending_allocation tenant, links user.
func (s *Service) CompleteOnboarding(ctx context.Context, userID uuid.UUID, in ProfileInput) (*domain.JoinRequest, *domain.Tenant, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.PermanentAddress = strings.TrimSpace(in.PermanentAddress)
	in.CurrentAddress = strings.TrimSpace(in.CurrentAddress)
	in.ParentName = strings.TrimSpace(in.ParentName)
	in.EmergencyContactName = strings.TrimSpace(in.EmergencyContactName)
	if in.EmergencyContactName == "" {
		in.EmergencyContactName = in.ParentName
	}
	in.EmergencyPhone = strings.TrimSpace(in.EmergencyPhone)

	if !in.Consent {
		return nil, nil, ErrConsentRequired
	}
	if in.Name == "" || in.PermanentAddress == "" || in.CurrentAddress == "" || in.EmergencyPhone == "" {
		return nil, nil, ErrProfileIncomplete
	}
	if len(in.IDPhotoBytes) == 0 {
		return nil, nil, ErrPhotoRequired
	}
	if len(in.IDPhotoBytes) > maxIDPhotoBytes {
		return nil, nil, fmt.Errorf("join: id photo too large (max 2MB)")
	}

	j, err := s.joins.GetPendingByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, ErrNotPending
		}
		return nil, nil, err
	}
	if j.TenantID != nil {
		return nil, nil, ErrAlreadyOnboarded
	}

	slot, settings, err := s.loadOnboardingContext(ctx, j)
	if err != nil {
		return nil, nil, ErrInvalidInvite
	}
	if j.SlotInviteID != nil {
		if slot == nil || slot.PropertyID != j.PropertyID || slot.Status != domain.SlotInviteOpen || !slot.ExpiresAt.After(s.now()) {
			return nil, nil, ErrInvalidInvite
		}
	}
	schemaVer := profileSchemaVersion(j, slot, settings)
	if err := ValidateProfileV1(in, j.Phone, schemaVer); err != nil {
		return nil, nil, err
	}

	var slotRoomID *uuid.UUID
	if slot != nil {
		slotRoomID = &slot.RoomID
	}
	roomNumber := ""
	if slotRoomID != nil {
		roomNumber, _ = s.roomNumber(ctx, *slotRoomID)
	}
	activate := resolveAutoActivateTerms(slot, settings, roomNumber)

	joinedOn := s.now().UTC().Truncate(24 * time.Hour)
	j.Name = in.Name
	j.PermanentAddress = in.PermanentAddress
	j.CurrentAddress = in.CurrentAddress
	j.ParentName = in.EmergencyContactName
	j.EmergencyPhone = in.EmergencyPhone
	j.EmergencyContactName = in.EmergencyContactName
	j.OccupationType = in.OccupationType
	j.DateOfBirth = in.DateOfBirth
	j.ProfileSchemaVersion = schemaVer
	j.OccupationDetails = in.Details.JSON()
	j.JoinedOn = &joinedOn

	phone := j.Phone
	var guardianName, guardianPhone *string
	if strings.TrimSpace(in.GuardianName) != "" {
		guardianName = &in.GuardianName
	}
	if strings.TrimSpace(in.GuardianPhone) != "" {
		guardianPhone = &in.GuardianPhone
	}
	tenantIn := domain.NewTenantInput{
		PropertyID:           j.PropertyID,
		Name:                 in.Name,
		Phone:                &phone,
		RoomID:               slotRoomID,
		PermanentAddress:     in.PermanentAddress,
		CurrentAddress:       in.CurrentAddress,
		ParentName:           in.EmergencyContactName,
		EmergencyPhone:       in.EmergencyPhone,
		EmergencyContactName: in.EmergencyContactName,
		OccupationType:       in.OccupationType,
		DateOfBirth:          in.DateOfBirth,
		OccupationDetails:    in.Details.JSON(),
		GuardianName:         guardianName,
		GuardianPhone:        guardianPhone,
		JoinedOn:             &joinedOn,
		IDPhotoBytes:         in.IDPhotoBytes,
		NoticePeriodDays:     30,
	}
	if in.DateOfBirth != nil && ageYears(*in.DateOfBirth, s.now()) < 18 {
		tenantIn.GuardianName = guardianName
		tenantIn.GuardianPhone = guardianPhone
	}

	finish := func(joins JoinStore, users UserLinker, pub events.Publisher, t *domain.Tenant) error {
		if err := users.SetTenantID(ctx, j.UserID, t.ID); err != nil {
			return fmt.Errorf("link user: %w", err)
		}
		j.Status = domain.JoinApproved
		j.TenantID = &t.ID
		if err := joins.Update(ctx, j); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"join_id": j.ID.String(), "tenant_id": t.ID.String()})
		_ = pub.Publish(ctx, domain.Event{
			TenantID:   domain.Ptr(t.ID),
			PropertyID: t.PropertyID,
			EventType:  domain.EvtJoinApproved,
			OccurredAt: s.now(),
			Payload:    payload,
		})
		_ = pub.Publish(ctx, domain.Event{
			TenantID:   domain.Ptr(t.ID),
			PropertyID: t.PropertyID,
			EventType:  domain.EvtConsentGiven,
			OccurredAt: s.now(),
			Payload:    json.RawMessage(`{"channel":"join_app","purpose":"id_photo_record"}`),
		})
		return nil
	}

	if s.pool != nil && s.joinDB != nil && s.userDB != nil && s.eventDB != nil && s.tenantSvc != nil {
		var out *domain.Tenant
		err := postgres.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
			t, err := s.tenantSvc.CreatePendingFromOnboardingTx(ctx, tx, tenantIn)
			if err != nil {
				return err
			}
			if j.SlotInviteID != nil && s.slotDB != nil {
				if _, err := s.slotDB.WithTx(tx).ConsumeOpenForJoin(ctx, *j.SlotInviteID, j.PropertyID, userID, t.ID); err != nil {
					return err
				}
			}
			if activate.Should {
				t, err = s.tenantSvc.AssignTermsTx(ctx, tx, t.ID, activate.RoomNumber, activate.RentPaise, activate.DueDay, activate.DepositPaise, 30)
				if err != nil {
					return err
				}
			}
			if err := finish(s.joinDB.WithTx(tx), s.userDB.WithTx(tx), events.NewPostgresPublisher(s.eventDB.WithTx(tx)), t); err != nil {
				return err
			}
			out = t
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		return j, out, nil
	}

	if s.tenantSvc == nil {
		return nil, nil, fmt.Errorf("join: pending onboarder not configured")
	}
	t, err := s.tenantSvc.CreatePendingFromOnboarding(ctx, tenantIn)
	if err != nil {
		return nil, nil, err
	}
	if j.SlotInviteID != nil && s.slotDB != nil {
		if _, err := s.slotDB.ConsumeOpenForJoin(ctx, *j.SlotInviteID, j.PropertyID, userID, t.ID); err != nil {
			return nil, nil, err
		}
	}
	if activate.Should {
		t, err = s.tenantSvc.AssignTerms(ctx, t.ID, activate.RoomNumber, activate.RentPaise, activate.DueDay, activate.DepositPaise, 30)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := finish(s.joins, s.users, s.pub, t); err != nil {
		return nil, nil, err
	}
	return j, t, nil
}

func (s *Service) Me(ctx context.Context, userID uuid.UUID) (*domain.JoinRequest, error) {
	j, err := s.joins.GetLatestByUser(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotPending
		}
		return nil, err
	}
	return j, nil
}

func (s *Service) List(ctx context.Context, propertyID uuid.UUID, status *domain.JoinStatus) ([]domain.JoinRequest, error) {
	return s.joins.ListByProperty(ctx, propertyID, status)
}

type ActivateInput struct {
	RoomNumber       *string
	RentAmount       int64
	DueDay           int16
	DepositAmount    int64
	NoticePeriodDays int16
}

// Activate assigns room/rent to an onboarded (approved) join awaiting allocation.
func (s *Service) Activate(ctx context.Context, propertyID, joinID uuid.UUID, in ActivateInput) (*domain.Tenant, error) {
	j, err := s.joins.GetByID(ctx, joinID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if j.PropertyID != propertyID {
		return nil, ErrNotFound
	}
	if j.Status != domain.JoinApproved || j.TenantID == nil {
		return nil, ErrNotAwaitingAssign
	}
	if in.RentAmount <= 0 || in.DueDay < 1 || in.DueDay > 28 {
		return nil, fmt.Errorf("join: rent_amount and due_day required")
	}

	if s.pool != nil && s.tenantSvc != nil {
		return s.tenantSvc.AssignTerms(ctx, *j.TenantID, in.RoomNumber, in.RentAmount, in.DueDay, in.DepositAmount, in.NoticePeriodDays)
	}
	if s.tenantSvc == nil {
		return nil, fmt.Errorf("join: terms assigner not configured")
	}
	return s.tenantSvc.AssignTerms(ctx, *j.TenantID, in.RoomNumber, in.RentAmount, in.DueDay, in.DepositAmount, in.NoticePeriodDays)
}

func (s *Service) Reject(ctx context.Context, propertyID, joinID uuid.UUID) error {
	j, err := s.joins.GetByID(ctx, joinID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if j.PropertyID != propertyID || j.Status != domain.JoinPending {
		return ErrNotPendingOwner
	}
	j.Status = domain.JoinRejected
	if err := s.joins.Update(ctx, j); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"join_id": j.ID.String()})
	_ = s.pub.Publish(ctx, domain.Event{
		PropertyID: propertyID,
		EventType:  domain.EvtJoinRejected,
		OccurredAt: s.now(),
		Payload:    payload,
	})
	return nil
}
