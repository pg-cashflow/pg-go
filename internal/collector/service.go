package collector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/qr"
	"golang.org/x/sync/singleflight"
)

type IntentStore interface {
	Create(ctx context.Context, p *domain.PaymentIntent) error
	LatestOpenForDue(ctx context.Context, dueID uuid.UUID) (*domain.PaymentIntent, error)
}

type CashfreeOrders interface {
	CreateUPIOrder(ctx context.Context, orderID string, amountPaise int64, customerPhone, note string, idempotencyKey ...string) (sessionID string, expiresAt *time.Time, err error)
	GetOrder(ctx context.Context, orderID string) (*cashfree.OrderDetails, error)
}

type Service struct {
	intents  IntentStore
	cashfree CashfreeOrders
	sf       singleflight.Group
}

var ErrAtomicPaymentIntentManagerRequired = errors.New("cashfree checkout requires the transactional payment intent manager")

// ErrCashfreeOrderPending means an earlier provider order exists, but the
// application cannot prove that it is safe to create another order yet.
var ErrCashfreeOrderPending = errors.New("the previous Cashfree order is still being reconciled")

// ErrCashfreeOrderExpired means Cashfree confirmed that the previous order is
// terminal. The caller must retry the checkout request to claim a fresh order
// ID; it must not reuse the terminal merchant order ID.
var ErrCashfreeOrderExpired = errors.New("the previous Cashfree payment session has ended; retry checkout")

// ErrAmountBelowMinimum is returned when an online payment amount is below Cashfree's 100-paise (₹1) minimum.
var ErrAmountBelowMinimum = errors.New("amount is below minimum ₹1 (100 paise) for online payment")

// ErrInvalidCustomerPhone is returned when the tenant phone fails Cashfree adapter validation before intent claim.
var ErrInvalidCustomerPhone = errors.New("invalid customer phone for online payment")

func New(intents IntentStore, cashfree CashfreeOrders) *Service {
	return &Service{intents: intents, cashfree: cashfree}
}

// HasCashfree returns true if the Cashfree orders client is initialized and operational.
func (s *Service) HasCashfree() bool {
	return s != nil && s.cashfree != nil
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

	// Cashfree configured but cannot be used — set fallback reason for UI
	if prop.PaymentMode == domain.PaymentModeCashfree {
		if customerPhone == "" {
			out.FallbackReason = "missing_phone"
		} else {
			out.FallbackReason = "gateway_down"
		}
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
	key := due.ID.String()
	res, err, _ := s.sf.Do(key, func() (any, error) {
		return s.doEnsureCashfree(ctx, due, customerPhone)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

type PaymentIntentManager interface {
	ClaimInitiatingIntent(ctx context.Context, tenantID uuid.UUID, dueIDs []uuid.UUID, amounts []int64, totalAmountPaise int64, orderID string, minRemaining time.Duration) (claimed *domain.PaymentIntent, isReused bool, isInFlight bool, err error)
	TransitionCreated(ctx context.Context, intentID uuid.UUID, sessionID string, expiresAt *time.Time) error
	TransitionFailed(ctx context.Context, intentID uuid.UUID) error
	TransitionDispatchUnknown(ctx context.Context, intentID uuid.UUID) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.PaymentIntent, error)
}

func (s *Service) doEnsureCashfree(ctx context.Context, due *domain.Due, customerPhone string) (string, error) {
	if due.Amount < cashfree.MinOrderAmountPaise {
		return "", ErrAmountBelowMinimum
	}
	if _, pErr := cashfree.NormalizeCustomerPhone(customerPhone); pErr != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCustomerPhone, pErr)
	}
	minRemaining := 10 * time.Minute

	if mgr, ok := s.intents.(PaymentIntentManager); ok {
		orderID := fmt.Sprintf("pg-%s-%d", due.DueCode, time.Now().UnixNano()/1000)
		claimed, isReused, isInFlight, err := mgr.ClaimInitiatingIntent(ctx, due.TenantID, []uuid.UUID{due.ID}, []int64{due.Amount}, due.Amount, orderID, minRemaining)
		if err != nil {
			return "", err
		}
		if isReused && claimed != nil && claimed.PaymentSessionID != nil {
			return *claimed.PaymentSessionID, nil
		}
		if isInFlight && claimed != nil {
			for i := 0; i < 15; i++ {
				time.Sleep(100 * time.Millisecond)
				polled, pErr := mgr.GetByID(ctx, claimed.ID)
				if pErr == nil && polled != nil {
					if polled.Status == domain.IntentCreated && polled.PaymentSessionID != nil {
						return *polled.PaymentSessionID, nil
					}
					if polled.Status == domain.IntentFailed || polled.Status == domain.IntentSuperseded {
						break
					}
				}
			}
			existingOrder, gErr := s.cashfree.GetOrder(ctx, claimed.ProviderOrderID)
			if gErr != nil || existingOrder == nil {
				// This is an already-issued provider command. A failed status read
				// is not evidence that the order does not exist or cannot be paid.
				return "", ErrCashfreeOrderPending
			}
			switch existingOrder.OrderStatus {
			case "ACTIVE":
				if existingOrder.OrderStatus == "ACTIVE" && existingOrder.PaymentSessionID != "" {
					if trErr := mgr.TransitionCreated(ctx, claimed.ID, existingOrder.PaymentSessionID, existingOrder.ExpiresAt); trErr == nil {
						return existingOrder.PaymentSessionID, nil
					}
				}
				return "", ErrCashfreeOrderPending
			case "EXPIRED", "TERMINATED":
				_ = mgr.TransitionFailed(ctx, claimed.ID)
				return "", ErrCashfreeOrderExpired
			case "PAID":
				return "", ErrCashfreeOrderPending
			default:
				// Includes USER_DROPPED and unknown future provider states. Do not
				// issue a second create request for an order whose finality is not
				// established by the documented terminal states.
				return "", ErrCashfreeOrderPending
			}
		}

		idemKeyStr := ""
		if claimed.IdempotencyKey != uuid.Nil {
			idemKeyStr = claimed.IdempotencyKey.String()
		}
		session, exp, err := s.cashfree.CreateUPIOrder(ctx, claimed.ProviderOrderID, due.Amount, customerPhone, domain.UPINote(due.DueCode), idemKeyStr)
		if err != nil {
			var apiErr *cashfree.APIError
			if errors.As(err, &apiErr) {
				// 1. Create Order 409: order already exists at gateway - reconcile existing order
				if apiErr.StatusCode == 409 {
					existingOrder, gErr := s.cashfree.GetOrder(ctx, claimed.ProviderOrderID)
					if gErr == nil && existingOrder != nil && existingOrder.OrderStatus == "ACTIVE" && existingOrder.PaymentSessionID != "" {
						if trErr := mgr.TransitionCreated(ctx, claimed.ID, existingOrder.PaymentSessionID, existingOrder.ExpiresAt); trErr == nil {
							return existingOrder.PaymentSessionID, nil
						}
					}
					_ = mgr.TransitionDispatchUnknown(ctx, claimed.ID)
					return "", err
				}
				// 2. Authoritative client request validation rejections on Create Order
				if apiErr.StatusCode == 400 || apiErr.StatusCode == 422 {
					_ = mgr.TransitionFailed(ctx, claimed.ID)
					return "", err
				}
			}

			existingOrder, gErr := s.cashfree.GetOrder(ctx, claimed.ProviderOrderID)
			if gErr == nil && existingOrder != nil {
				if existingOrder.OrderStatus == "ACTIVE" && existingOrder.PaymentSessionID != "" {
					if trErr := mgr.TransitionCreated(ctx, claimed.ID, existingOrder.PaymentSessionID, existingOrder.ExpiresAt); trErr == nil {
						return existingOrder.PaymentSessionID, nil
					}
				}
				if existingOrder.OrderStatus == "EXPIRED" || existingOrder.OrderStatus == "TERMINATED" {
					_ = mgr.TransitionFailed(ctx, claimed.ID)
					return "", err
				}
			}
			// Ambiguous outcome: do NOT mark failed on 404. Preserve dispatch_unknown for recovery.
			_ = mgr.TransitionDispatchUnknown(ctx, claimed.ID)
			return "", err
		}
		if err := mgr.TransitionCreated(ctx, claimed.ID, session, exp); err != nil {
			_ = mgr.TransitionDispatchUnknown(ctx, claimed.ID)
			return "", err
		}
		return session, nil
	}

	// A generic IntentStore cannot atomically reserve dues, persist the provider
	// command and create the intent. Do not create an untracked Cashfree order.
	return "", ErrAtomicPaymentIntentManagerRequired
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func (s *Service) MultiDuePayIntent(ctx context.Context, dues []*domain.Due, prop *domain.Property, room, customerPhone string) (*domain.PayIntent, error) {
	if prop == nil {
		return nil, errors.New("property is required")
	}
	if len(dues) == 0 {
		return nil, errors.New("no dues provided")
	}

	dueIDs := make([]uuid.UUID, len(dues))
	seen := make(map[uuid.UUID]struct{}, len(dues))
	totalAmountPaise := int64(0)
	payable := true
	for i, d := range dues {
		if d == nil || d.ID == uuid.Nil {
			return nil, errors.New("due and due ID are required")
		}
		if _, dup := seen[d.ID]; dup {
			return nil, fmt.Errorf("duplicate due ID %s", d.ID)
		}
		seen[d.ID] = struct{}{}
		if d.Amount <= 0 {
			return nil, fmt.Errorf("due %s has non-positive amount %d", d.ID, d.Amount)
		}
		if d.Amount > math.MaxInt64-totalAmountPaise {
			return nil, errors.New("total due amount overflows int64")
		}
		dueIDs[i] = d.ID
		totalAmountPaise += d.Amount
		if d.Status != domain.DueStatusPending && d.Status != domain.DueStatusPartial {
			payable = false
		}
	}

	note := fmt.Sprintf("PG-%s-BATCH-%d", dues[0].DueCode, len(dues))
	out := &domain.PayIntent{
		Mode:        domain.PaymentModeManual,
		Note:        note,
		DueCode:     dues[0].DueCode,
		AmountPaise: totalAmountPaise,
		DueCount:    len(dues),
		DueIDs:      dueIDs,
		Payable:     payable,
	}

	if !payable {
		return out, nil
	}

	if prop.PaymentMode == domain.PaymentModeCashfree && s.cashfree != nil && customerPhone != "" {
		session, err := s.ensureCashfreeMultiDue(ctx, dues, totalAmountPaise, customerPhone)
		if err != nil {
			return nil, err
		}
		out.Mode = domain.PaymentModeCashfree
		out.PaymentSessionID = session
		out.VPA = ""
		out.UPILink = ""
		out.QRPNGURL = ""
		return out, nil
	}

	link := qr.GenerateUPILink(prop.UPIVPA, prop.OwnerName, totalAmountPaise, note, room)
	out.VPA = prop.UPIVPA
	out.UPILink = link
	return out, nil
}

func (s *Service) ensureCashfreeMultiDue(ctx context.Context, dues []*domain.Due, totalAmountPaise int64, customerPhone string) (string, error) {
	var sb strings.Builder
	sb.WriteString("multi:")
	for _, d := range dues {
		sb.WriteString(d.ID.String())
		sb.WriteString(":")
	}
	key := sb.String()

	res, err, _ := s.sf.Do(key, func() (any, error) {
		return s.doEnsureCashfreeMultiDue(ctx, dues, totalAmountPaise, customerPhone)
	})
	if err != nil {
		return "", err
	}
	return res.(string), nil
}

func (s *Service) doEnsureCashfreeMultiDue(ctx context.Context, dues []*domain.Due, totalAmountPaise int64, customerPhone string) (string, error) {
	if totalAmountPaise < cashfree.MinOrderAmountPaise {
		return "", ErrAmountBelowMinimum
	}
	if _, pErr := cashfree.NormalizeCustomerPhone(customerPhone); pErr != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCustomerPhone, pErr)
	}
	minRemaining := 10 * time.Minute
	dueIDs := make([]uuid.UUID, len(dues))
	amounts := make([]int64, len(dues))
	for i, d := range dues {
		dueIDs[i] = d.ID
		amounts[i] = d.Amount
	}
	tenantID := dues[0].TenantID

	if mgr, ok := s.intents.(PaymentIntentManager); ok {
		orderID := fmt.Sprintf("pg-m-%s-%d", dues[0].DueCode, time.Now().UnixNano()/1000)
		note := fmt.Sprintf("PG-%s-BATCH-%d", dues[0].DueCode, len(dues))
		claimed, isReused, isInFlight, err := mgr.ClaimInitiatingIntent(ctx, tenantID, dueIDs, amounts, totalAmountPaise, orderID, minRemaining)
		if err != nil {
			return "", err
		}
		if isReused && claimed != nil && claimed.PaymentSessionID != nil {
			return *claimed.PaymentSessionID, nil
		}
		if isInFlight && claimed != nil {
			for i := 0; i < 15; i++ {
				time.Sleep(100 * time.Millisecond)
				polled, pErr := mgr.GetByID(ctx, claimed.ID)
				if pErr == nil && polled != nil {
					if polled.Status == domain.IntentCreated && polled.PaymentSessionID != nil {
						return *polled.PaymentSessionID, nil
					}
					if polled.Status == domain.IntentFailed || polled.Status == domain.IntentSuperseded {
						break
					}
				}
			}
			existingOrder, gErr := s.cashfree.GetOrder(ctx, claimed.ProviderOrderID)
			if gErr != nil || existingOrder == nil {
				return "", ErrCashfreeOrderPending
			}
			switch existingOrder.OrderStatus {
			case "ACTIVE":
				if existingOrder.PaymentSessionID != "" {
					if trErr := mgr.TransitionCreated(ctx, claimed.ID, existingOrder.PaymentSessionID, existingOrder.ExpiresAt); trErr == nil {
						return existingOrder.PaymentSessionID, nil
					}
				}
				return "", ErrCashfreeOrderPending
			case "EXPIRED", "TERMINATED":
				_ = mgr.TransitionFailed(ctx, claimed.ID)
				return "", ErrCashfreeOrderExpired
			case "PAID":
				return "", ErrCashfreeOrderPending
			default:
				return "", ErrCashfreeOrderPending
			}
		}

		idemKeyStr := ""
		if claimed.IdempotencyKey != uuid.Nil {
			idemKeyStr = claimed.IdempotencyKey.String()
		}
		session, exp, err := s.cashfree.CreateUPIOrder(ctx, claimed.ProviderOrderID, totalAmountPaise, customerPhone, note, idemKeyStr)
		if err != nil {
			var apiErr *cashfree.APIError
			if errors.As(err, &apiErr) {
				// 1. Create Order 409: order already exists at gateway - reconcile existing order
				if apiErr.StatusCode == 409 {
					existingOrder, gErr := s.cashfree.GetOrder(ctx, claimed.ProviderOrderID)
					if gErr == nil && existingOrder != nil && existingOrder.OrderStatus == "ACTIVE" && existingOrder.PaymentSessionID != "" {
						if trErr := mgr.TransitionCreated(ctx, claimed.ID, existingOrder.PaymentSessionID, existingOrder.ExpiresAt); trErr == nil {
							return existingOrder.PaymentSessionID, nil
						}
					}
					_ = mgr.TransitionDispatchUnknown(ctx, claimed.ID)
					return "", err
				}
				// 2. Authoritative client request validation rejections on Create Order
				if apiErr.StatusCode == 400 || apiErr.StatusCode == 422 {
					_ = mgr.TransitionFailed(ctx, claimed.ID)
					return "", err
				}
			}

			existingOrder, gErr := s.cashfree.GetOrder(ctx, claimed.ProviderOrderID)
			if gErr == nil && existingOrder != nil {
				if existingOrder.OrderStatus == "ACTIVE" && existingOrder.PaymentSessionID != "" {
					if trErr := mgr.TransitionCreated(ctx, claimed.ID, existingOrder.PaymentSessionID, existingOrder.ExpiresAt); trErr == nil {
						return existingOrder.PaymentSessionID, nil
					}
				}
				if existingOrder.OrderStatus == "EXPIRED" || existingOrder.OrderStatus == "TERMINATED" {
					_ = mgr.TransitionFailed(ctx, claimed.ID)
					return "", err
				}
			}
			// Ambiguous outcome: do NOT mark failed on 404. Preserve dispatch_unknown for recovery.
			_ = mgr.TransitionDispatchUnknown(ctx, claimed.ID)
			return "", err
		}
		if err := mgr.TransitionCreated(ctx, claimed.ID, session, exp); err != nil {
			_ = mgr.TransitionDispatchUnknown(ctx, claimed.ID)
			return "", err
		}
		return session, nil
	}

	// The same atomic claim contract is mandatory for multi-due checkout.
	return "", ErrAtomicPaymentIntentManagerRequired
}

func PNGURL(role string, dueID uuid.UUID) string {
	if role == "owner" {
		return "/api/owner/dues/" + dueID.String() + "/qr"
	}
	return "/api/tenant/dues/" + dueID.String() + "/qr"
}
