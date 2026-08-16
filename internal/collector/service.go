package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/qr"
)

type IntentStore interface {
	Create(ctx context.Context, p *domain.PaymentIntent) error
	LatestOpenForDue(ctx context.Context, dueID uuid.UUID) (*domain.PaymentIntent, error)
}

type CashfreeOrders interface {
	CreateUPIOrder(ctx context.Context, orderID string, amountPaise int, customerPhone, note string) (sessionID string, expiresAt *time.Time, err error)
}

type Service struct {
	intents  IntentStore
	cashfree CashfreeOrders
}

func New(intents IntentStore, cashfree CashfreeOrders) *Service {
	return &Service{intents: intents, cashfree: cashfree}
}

func (s *Service) PayIntent(ctx context.Context, due *domain.Due, prop *domain.Property, room, pngURL, customerPhone string) (*domain.PayIntent, []byte, error) {
	note := domain.UPINote(due.DueCode)
	payable := due.Status == domain.DueStatusPending || due.Status == domain.DueStatusPartial
	out := &domain.PayIntent{
		Mode:        domain.PaymentModeManual,
		Note:        note,
		DueCode:     due.DueCode,
		AmountPaise: due.Amount,
		QRPNGURL:    pngURL,
		Payable:     payable,
	}
	if !payable {
		return out, nil, nil
	}

	if prop.PaymentMode == domain.PaymentModeCashfree && s.cashfree != nil && customerPhone != "" {
		session, err := s.ensureCashfree(ctx, due, customerPhone)
		if err != nil {
			return nil, nil, err
		}
		out.Mode = domain.PaymentModeCashfree
		out.PaymentSessionID = session
		out.VPA = ""
		out.UPILink = ""
		out.QRPNGURL = ""
		return out, nil, nil
	}

	link := qr.GenerateUPILink(prop.UPIVPA, prop.OwnerName, int64(due.Amount), due.DueCode, room)
	png, err := qr.GenerateQR(link)
	if err != nil {
		return nil, nil, err
	}
	out.VPA = prop.UPIVPA
	out.UPILink = link
	return out, png, nil
}

func (s *Service) ensureCashfree(ctx context.Context, due *domain.Due, customerPhone string) (string, error) {
	if existing, err := s.intents.LatestOpenForDue(ctx, due.ID); err == nil && existing.PaymentSessionID != nil && existing.AmountPaise == due.Amount {
		return *existing.PaymentSessionID, nil
	} else if err != nil && !isNoRows(err) {
		return "", err
	}
	orderID := fmt.Sprintf("pg-%s-%d", due.DueCode, time.Now().Unix())
	session, exp, err := s.cashfree.CreateUPIOrder(ctx, orderID, due.Amount, customerPhone, domain.UPINote(due.DueCode))
	if err != nil {
		return "", err
	}
	intent := &domain.PaymentIntent{
		DueID:            due.ID,
		Provider:         "cashfree",
		ProviderOrderID:  orderID,
		PaymentSessionID: &session,
		AmountPaise:      due.Amount,
		Status:           domain.IntentCreated,
		ExpiresAt:        exp,
	}
	if err := s.intents.Create(ctx, intent); err != nil {
		return "", err
	}
	return session, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func PNGURL(role string, dueID uuid.UUID) string {
	if role == "owner" {
		return "/owner/dues/" + dueID.String() + "/qr"
	}
	return "/tenant/dues/" + dueID.String() + "/qr"
}
