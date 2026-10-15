package referral

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type Service struct {
	repo *postgres.ReferralRepo
}

func NewService(repo *postgres.ReferralRepo) *Service {
	return &Service{repo: repo}
}

func (s *Service) loadSettings(ctx context.Context, propertyID uuid.UUID) (bool, int, int, error) {
	enabled, joinerBps, referrerBps, err := s.repo.PropertyReferralSettings(ctx, propertyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, 0, nil
		}
		return false, 0, 0, err
	}
	return enabled, joinerBps, referrerBps, nil
}

func (s *Service) CreateForTenant(ctx context.Context, propertyID, referrerTenantID uuid.UUID, phone, name string, dest domain.ReferralRewardDestination) (*domain.ReferralAdmission, error) {
	enabled, joinerBps, referrerBps, err := s.loadSettings(ctx, propertyID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, postgres.ErrReferralDisabled
	}
	code, err := joinsvc.GenerateInviteCode()
	if err != nil {
		return nil, err
	}
	return s.repo.CreatePending(ctx, postgres.CreateReferralInput{
		PropertyID:        propertyID,
		ReferrerTenantID:  &referrerTenantID,
		Phone:             phone,
		Name:              name,
		Code:              code,
		JoinerDiscountBps: joinerBps,
		ReferrerRewardBps: referrerBps,
		Destination:       dest,
		SetBy:             domain.ReferralSetByTenant,
	})
}

func (s *Service) CreateOffline(ctx context.Context, propertyID uuid.UUID, joinerPhone, joinerName, externalPhone, externalName string, dest domain.ReferralRewardDestination) (*domain.ReferralAdmission, error) {
	enabled, joinerBps, referrerBps, err := s.loadSettings(ctx, propertyID)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, postgres.ErrReferralDisabled
	}
	code, err := joinsvc.GenerateInviteCode()
	if err != nil {
		return nil, err
	}
	return s.repo.CreatePending(ctx, postgres.CreateReferralInput{
		PropertyID:            propertyID,
		Phone:                 joinerPhone,
		Name:                  joinerName,
		Code:                  code,
		JoinerDiscountBps:     joinerBps,
		ReferrerRewardBps:     referrerBps,
		Destination:           dest,
		SetBy:                 domain.ReferralSetByOwnerManual,
		ReferrerExternalPhone: externalPhone,
		ReferrerExternalName:  externalName,
	})
}

func (s *Service) BindAtJoin(ctx context.Context, propertyID, tenantID uuid.UUID, joinerPhone, code string) error {
	code = joinsvc.NormalizeInvite(code)
	if code == "" {
		return nil
	}
	ref, err := s.repo.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return postgres.ErrReferralNotFound
		}
		return err
	}
	if ref.PropertyID != propertyID || ref.Status != "pending" {
		return postgres.ErrReferralNotOpen
	}
	if ref.Phone != "" && ref.Phone != joinerPhone {
		return postgres.ErrReferralNotOpen
	}
	if ref.ReferrerTenantID != nil && *ref.ReferrerTenantID == tenantID {
		return postgres.ErrReferralSelf
	}
	return s.repo.BindToTenant(ctx, ref.ID, propertyID, tenantID, ref.Phone)
}

func bpsAmount(rentPaise int64, bps int) int64 {
	if rentPaise <= 0 || bps <= 0 {
		return 0
	}
	return rentPaise * int64(bps) / 10000
}

type firstRentPlan struct {
	refID       uuid.UUID
	joinerDisc  int64
	passPaise   int64
	rentBase    int64
}

func (s *Service) planFirstRent(ctx context.Context, tenantID uuid.UUID, rentBase int64) (*firstRentPlan, error) {
	paid, err := s.repo.CountPaidRentDues(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if paid > 0 {
		return nil, nil
	}
	ref, err := s.repo.GetBoundForTenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if ref.FirstRentDueID != nil {
		return nil, nil
	}
	joinerDisc := bpsAmount(rentBase, ref.JoinerDiscountBps)
	passPaise := int64(0)
	dest := domain.ReferralDestReferrerCredit
	if ref.RewardDestination != nil {
		dest = *ref.RewardDestination
	}
	if dest == domain.ReferralDestPassToJoiner {
		passPaise = bpsAmount(rentBase, ref.ReferrerRewardBps)
	}
	return &firstRentPlan{refID: ref.ID, joinerDisc: joinerDisc, passPaise: passPaise, rentBase: rentBase}, nil
}

// ApplyFirstRentDiscount mutates due.Amount before insert.
func (s *Service) ApplyFirstRentDiscount(ctx context.Context, tenant *domain.Tenant, due *domain.Due) error {
	if tenant == nil || due == nil || due.Kind != domain.DueKindRent {
		return nil
	}
	plan, err := s.planFirstRent(ctx, tenant.ID, due.OriginalAmount)
	if err != nil || plan == nil {
		return err
	}
	totalDisc := plan.joinerDisc + plan.passPaise
	if totalDisc > due.Amount {
		totalDisc = due.Amount
	}
	due.Amount -= totalDisc
	return nil
}

// RecordFirstRentAdjustment persists referral adjustment after due insert.
func (s *Service) RecordFirstRentAdjustment(ctx context.Context, tenantID, dueID uuid.UUID, originalAmount int64) error {
	plan, err := s.planFirstRent(ctx, tenantID, originalAmount)
	if err != nil || plan == nil {
		return err
	}
	if err := s.repo.InsertDueAdjustment(ctx, dueID, plan.refID, plan.joinerDisc, plan.passPaise, plan.rentBase); err != nil {
		return err
	}
	return s.repo.SetFirstRentDue(ctx, plan.refID, dueID)
}

// SettleFirstRentPayment credits referrer when first rent is paid.
func (s *Service) SettleFirstRentPayment(ctx context.Context, dueID uuid.UUID) error {
	referralID, _, passPaise, rentBase, err := s.repo.GetAdjustmentByDueID(ctx, dueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	ref, err := s.repo.GetByID(ctx, referralID)
	if err != nil {
		return err
	}
	dest := domain.ReferralDestReferrerCredit
	if ref.RewardDestination != nil {
		dest = *ref.RewardDestination
	}
	if dest == domain.ReferralDestPassToJoiner || passPaise > 0 {
		return s.repo.MarkRewarded(ctx, referralID, nil)
	}
	amount := bpsAmount(rentBase, ref.ReferrerRewardBps)
	if amount <= 0 {
		return s.repo.MarkRewarded(ctx, referralID, nil)
	}
	idem := fmt.Sprintf("referral:earn:%s", ref.ID.String())
	var tenantID *uuid.UUID
	var extPhone *string
	if ref.ReferrerTenantID != nil {
		tenantID = ref.ReferrerTenantID
	} else if ref.ReferrerExternalPhone != "" {
		extPhone = &ref.ReferrerExternalPhone
	}
	if err := s.repo.InsertCreditEntry(ctx, ref.PropertyID, tenantID, extPhone, ref.ID, "earn", amount, idem, &dueID, nil); err != nil {
		return err
	}
	if tenantID != nil {
		if err := s.repo.AddTenantCreditBalance(ctx, *tenantID, amount); err != nil {
			return err
		}
	}
	return s.repo.MarkRewarded(ctx, referralID, nil)
}

func (s *Service) ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.ReferralAdmission, error) {
	return s.repo.ListByProperty(ctx, propertyID)
}

func (s *Service) ListByReferrerTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.ReferralAdmission, error) {
	return s.repo.ListByReferrerTenant(ctx, tenantID)
}
