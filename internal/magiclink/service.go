package magiclink

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrTokenNotFound = errors.New("magiclink: token not found")
	ErrTokenUsed     = errors.New("magiclink: token already used")
	ErrTokenExpired  = errors.New("magiclink: token expired")
)

// DueView is the payment-page payload for GET /p/:token.
// Magic links stay valid after vacate — tenant status is never checked here.
type DueView struct {
	TokenID      uuid.UUID  `json:"token_id"`
	Due          domain.Due `json:"due"`
	AmountPaise  int64      `json:"amount_paise"`
	OwnerName    string     `json:"owner_name"`
	PropertyName string     `json:"property_name"`
	UPIVPA       string     `json:"-"` // for QR generation; not always exposed
	PaymentMode  string     `json:"-"`
	TenantName   string     `json:"tenant_name"`
	TenantPhone  string     `json:"-"`
	RoomNumber   *string    `json:"room_number,omitempty"`
	ExpiresAt    time.Time  `json:"expires_at"`
}

// Service creates and resolves payment magic links.
type Service struct {
	tokens     TokenRepository
	dues       DueRepository
	properties PropertyRepository
	tenants    TenantRepository
	secret     string
}

func NewService(tokens TokenRepository, dues DueRepository, properties PropertyRepository, tenants TenantRepository, hmacSecret string) *Service {
	return &Service{
		tokens:     tokens,
		dues:       dues,
		properties: properties,
		tenants:    tenants,
		secret:     hmacSecret,
	}
}

// CreatePaymentToken invalidates prior unused tokens for the due, creates a 72h token,
// and returns the raw URL path (/p/<raw>).
func (s *Service) CreatePaymentToken(ctx context.Context, dueID uuid.UUID) (string, error) {
	if _, err := s.dues.GetByID(ctx, dueID); err != nil {
		return "", fmt.Errorf("magiclink: due: %w", err)
	}
	if err := s.tokens.InvalidateUnusedForDue(ctx, dueID); err != nil {
		return "", fmt.Errorf("magiclink: invalidate: %w", err)
	}
	raw, err := GenerateToken()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	tok := &domain.PaymentToken{
		DueID:     dueID,
		TokenHash: HashToken(s.secret, raw),
		ExpiresAt: now.Add(domain.PaymentTokenTTL),
		Used:      false,
	}
	if err := s.tokens.Create(ctx, tok); err != nil {
		return "", fmt.Errorf("magiclink: create: %w", err)
	}
	return "/p/" + raw, nil
}

// ResolveToken loads DueView for a raw token. Does not check tenant.status (post-vacate OK).
func (s *Service) ResolveToken(ctx context.Context, raw string) (*DueView, error) {
	if raw == "" {
		return nil, ErrTokenNotFound
	}
	hash := HashToken(s.secret, raw)
	tok, err := s.tokens.GetByHash(ctx, hash)
	if err != nil {
		return nil, ErrTokenNotFound
	}
	if tok.Used {
		return nil, ErrTokenUsed
	}
	if time.Now().UTC().After(tok.ExpiresAt) {
		return nil, ErrTokenExpired
	}

	due, err := s.dues.GetByID(ctx, tok.DueID)
	if err != nil {
		return nil, fmt.Errorf("magiclink: due: %w", err)
	}
	if due.Status == domain.DueStatusPaid {
		return nil, ErrTokenUsed
	}
	prop, err := s.properties.GetByID(ctx, due.PropertyID)
	if err != nil {
		return nil, fmt.Errorf("magiclink: property: %w", err)
	}

	view := &DueView{
		TokenID:      tok.ID,
		Due:          *due,
		AmountPaise:  due.Amount,
		OwnerName:    prop.OwnerName,
		PropertyName: prop.Name,
		UPIVPA:       prop.UPIVPA,
		PaymentMode:  prop.PaymentMode,
		ExpiresAt:    tok.ExpiresAt,
	}

	if s.tenants != nil {
		// Intentionally no status check — collect final dues after vacate.
		if tenant, err := s.tenants.GetByID(ctx, due.TenantID); err == nil {
			view.TenantName = tenant.Name
			view.RoomNumber = tenant.RoomNumber
			if tenant.Phone != nil {
				view.TenantPhone = *tenant.Phone
			}
		}
	}
	return view, nil
}
