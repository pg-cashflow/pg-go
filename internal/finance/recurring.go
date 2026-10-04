package finance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// PropertyBudgetsLister lists properties for recurring OPEX accrual processing.
type PropertyBudgetsLister interface {
	List(ctx context.Context) ([]domain.Property, error)
}

// RecurringExpenseScheduler processes monthly recurring expenses (lease, internet, vendor retainers).
type RecurringExpenseScheduler struct {
	Finance    *Service
	Store      Store
	Properties PropertyBudgetsLister
	Log        *slog.Logger
}

// NewRecurringExpenseScheduler constructs a RecurringExpenseScheduler.
func NewRecurringExpenseScheduler(fin *Service, store Store, props PropertyBudgetsLister) *RecurringExpenseScheduler {
	return &RecurringExpenseScheduler{
		Finance:    fin,
		Store:      store,
		Properties: props,
		Log:        slog.Default(),
	}
}

// ProcessRecurringExpenses accrues scheduled monthly fixed OPEX for all properties.
// For any budget configured for fixed OPEX categories (e.g. lease, internet, salary, vendor retainer),
// it generates an accrued expense if not already generated for the period.
func (s *RecurringExpenseScheduler) ProcessRecurringExpenses(ctx context.Context, asOf time.Time) error {
	if s == nil || s.Finance == nil || s.Store == nil {
		return nil
	}
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	period := asOf.Format("2006-01")
	var properties []domain.Property
	if s.Properties != nil {
		var err error
		properties, err = s.Properties.List(ctx)
		if err != nil {
			return fmt.Errorf("list properties for recurring expenses: %w", err)
		}
	}

	for _, prop := range properties {
		ownerID, err := s.Store.GetPropertyOwnerUserID(ctx, prop.ID)
		if err != nil {
			log.Warn("skipping recurring expense processing: owner user not found", "property_id", prop.ID, "err", err)
			continue
		}
		budgets, err := s.Store.ListBudgets(ctx, prop.ID, period)
		if err != nil {
			log.Error("failed to list budgets for recurring expenses", "property_id", prop.ID, "err", err)
			continue
		}
		for _, b := range budgets {
			if b.AmountPaise <= 0 {
				continue
			}
			idem := fmt.Sprintf("recurring:budget:%s:%s:%s", b.PropertyID, b.CategoryCode, period)
			in := CreateExpenseInput{
				PropertyID:     b.PropertyID,
				ActorID:        ownerID,
				ActorRole:      string(domain.RoleOwner),
				CategoryCode:   b.CategoryCode,
				VendorName:     fmt.Sprintf("Recurring: %s", b.CategoryCode),
				Description:    fmt.Sprintf("Scheduled recurring budget expense for %s (%s)", b.CategoryCode, period),
				AmountPaise:    b.AmountPaise,
				IdempotencyKey: idem,
				OccurredAt:     asOf,
			}
			_, _, err := s.Finance.CreateExpense(ctx, in)
			if err != nil {
				if errors.Is(err, domain.ErrDuplicateIdempotency) || errors.Is(err, ErrDuplicateIdempotency) {
					// Already created for this cycle, idempotent skip
					continue
				}
				log.Error("failed to process recurring expense", "property_id", b.PropertyID, "category", b.CategoryCode, "err", err)
			} else {
				log.Info("processed recurring expense", "property_id", b.PropertyID, "category", b.CategoryCode, "amount_paise", b.AmountPaise)
			}
		}
	}
	return nil
}
