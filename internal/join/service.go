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
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/tenant"
)

var (
	ErrInvalidInvite   = errors.New("join: invalid invite code")
	ErrAlreadyActive   = errors.New("join: already an active tenant")
	ErrNotPending      = errors.New("join: no pending join request")
	ErrNameRequired    = errors.New("join: name is required")
	ErrNotFound        = errors.New("join: not found")
	ErrNotPendingOwner = errors.New("join: request is not pending")
)

const inviteAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

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
	CreateTenant(ctx context.Context, in domain.NewTenantInput, depositPaise int) (*domain.Tenant, error)
}

type TenantTxCreator interface {
	CreateTenantTx(ctx context.Context, tx pgx.Tx, in domain.NewTenantInput, depositPaise int) (*domain.Tenant, error)
}

type Service struct {
	props   PropertyStore
	joins   JoinStore
	users   UserLinker
	tenants TenantCreator
	pub     events.Publisher
	now     func() time.Time

	// Optional TX wiring for Activate atomicity (tenant + user link + join approved).
	pool     *pgxpool.Pool
	joinDB   *postgres.JoinRepo
	userDB   *postgres.UserRepo
	eventDB  *postgres.EventRepo
	tenantTx TenantTxCreator
}

func NewService(props PropertyStore, joins JoinStore, users UserLinker, tenants TenantCreator, pub events.Publisher) *Service {
	return &Service{props: props, joins: joins, users: users, tenants: tenants, pub: pub, now: func() time.Time { return time.Now().UTC() }}
}

// NewServiceWithPool enables transactional Activate (tenant + deposit + user link + join approved).
func NewServiceWithPool(
	pool *pgxpool.Pool,
	props PropertyStore,
	joins *postgres.JoinRepo,
	users *postgres.UserRepo,
	tenants *tenant.Service,
	eventsRepo *postgres.EventRepo,
) *Service {
	pub := events.NewPostgresPublisher(eventsRepo)
	s := NewService(props, joins, users, tenants, pub)
	s.pool = pool
	s.joinDB = joins
	s.userDB = users
	s.eventDB = eventsRepo
	s.tenantTx = tenants
	return s
}

func (s *Service) LookupInvite(ctx context.Context, code string) (*domain.Property, error) {
	code = NormalizeInvite(code)
	if code == "" {
		return nil, ErrInvalidInvite
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
func (s *Service) EnsurePending(ctx context.Context, user *domain.User, propertyID uuid.UUID) (*domain.JoinRequest, error) {
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
	j := &domain.JoinRequest{
		PropertyID: propertyID,
		UserID:     user.ID,
		Phone:      user.Phone,
		Name:       "",
		Status:     domain.JoinPending,
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
	RentAmount       int
	DueDay           int16
	DepositAmount    int
	NoticePeriodDays int16
}

func (s *Service) Activate(ctx context.Context, propertyID, joinID uuid.UUID, in ActivateInput) (*domain.Tenant, error) {
	j, tenantIn, deposit, err := s.prepareActivate(ctx, propertyID, joinID, in)
	if err != nil {
		return nil, err
	}

	if s.pool != nil && s.joinDB != nil && s.userDB != nil && s.eventDB != nil && s.tenantTx != nil {
		var out *domain.Tenant
		err := postgres.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
			t, err := s.tenantTx.CreateTenantTx(ctx, tx, tenantIn, deposit)
			if err != nil {
				return err
			}
			if err := s.finishActivate(ctx, s.joinDB.WithTx(tx), s.userDB.WithTx(tx), events.NewPostgresPublisher(s.eventDB.WithTx(tx)), j, t); err != nil {
				return err
			}
			out = t
			return nil
		})
		return out, err
	}

	t, err := s.tenants.CreateTenant(ctx, tenantIn, deposit)
	if err != nil {
		return nil, err
	}
	if err := s.finishActivate(ctx, s.joins, s.users, s.pub, j, t); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Service) prepareActivate(ctx context.Context, propertyID, joinID uuid.UUID, in ActivateInput) (*domain.JoinRequest, domain.NewTenantInput, int, error) {
	j, err := s.joins.GetByID(ctx, joinID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewTenantInput{}, 0, ErrNotFound
		}
		return nil, domain.NewTenantInput{}, 0, err
	}
	if j.PropertyID != propertyID {
		return nil, domain.NewTenantInput{}, 0, ErrNotFound
	}
	if j.Status != domain.JoinPending {
		return nil, domain.NewTenantInput{}, 0, ErrNotPendingOwner
	}
	name := strings.TrimSpace(j.Name)
	if name == "" {
		return nil, domain.NewTenantInput{}, 0, ErrNameRequired
	}
	if in.NoticePeriodDays <= 0 {
		in.NoticePeriodDays = 30
	}
	phone := j.Phone
	return j, domain.NewTenantInput{
		PropertyID:       propertyID,
		Name:             name,
		Phone:            &phone,
		RoomNumber:       in.RoomNumber,
		AadhaarLast4:     j.AadhaarLast4,
		RentAmount:       in.RentAmount,
		DueDay:           in.DueDay,
		NoticePeriodDays: in.NoticePeriodDays,
	}, in.DepositAmount, nil
}

func (s *Service) finishActivate(ctx context.Context, joins JoinStore, users UserLinker, pub events.Publisher, j *domain.JoinRequest, t *domain.Tenant) error {
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
	return nil
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
