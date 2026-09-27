package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type SettlementStore interface {
	UpsertSettlement(ctx context.Context, s *domain.GatewaySettlement) error
	GetSettlement(ctx context.Context, id uuid.UUID) (*domain.GatewaySettlement, error)
	GetSettlementByCFID(ctx context.Context, cfSettlementID string, orderID string, cfPaymentID string) (*domain.GatewaySettlement, error)
	ListSettlements(ctx context.Context, propertyID *uuid.UUID, filter domain.SettlementFilter) ([]*domain.GatewaySettlement, int, error)
	ResolveDiscrepancy(ctx context.Context, id uuid.UUID, resolvedBy uuid.UUID, notes string) error
}

type IntentLookupStore interface {
	GetByOrderID(ctx context.Context, orderID string) (*domain.PaymentIntent, error)
}

type DueLookupStore interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Due, error)
}

type PaymentLookupStore interface {
	GetByCFPaymentID(ctx context.Context, cfID string) (*domain.Payment, error)
}

type SettlementReconciler struct {
	Store    SettlementStore
	Intents  IntentLookupStore
	Dues     DueLookupStore
	Payments PaymentLookupStore
	Finance  *Service
	Now      func() time.Time
}

func NewSettlementReconciler(
	store SettlementStore,
	intents IntentLookupStore,
	dues DueLookupStore,
	payments PaymentLookupStore,
	financeSvc *Service,
) *SettlementReconciler {
	return &SettlementReconciler{
		Store:    store,
		Intents:  intents,
		Dues:     dues,
		Payments: payments,
		Finance:  financeSvc,
		Now:      func() time.Time { return time.Now().UTC() },
	}
}

// ReconcileOrderSettlement processes an individual order settlement record from Cashfree.
func (r *SettlementReconciler) ReconcileOrderSettlement(
	ctx context.Context,
	rec cashfree.OrderSettlementRecord,
	source domain.IngestionSource,
) (*domain.GatewaySettlement, error) {
	if r.Store == nil {
		return nil, errors.New("finance: settlement store not configured")
	}

	rawPayload := rec.RawPayload
	if len(rawPayload) == 0 {
		b, _ := json.Marshal(rec)
		rawPayload = b
	}

	stlm := &domain.GatewaySettlement{
		CFSettlementID:     rec.CFSettlementID,
		OrderID:            &rec.OrderID,
		IngestionSource:    source,
		UTR:                rec.UTR,
		Currency:           "INR",
		GrossAmountPaise:   rec.GrossAmountPaise,
		ServiceChargePaise: rec.ServiceChargePaise,
		ServiceTaxPaise:    rec.ServiceTaxPaise,
		AdjustmentPaise:    rec.AdjustmentPaise,
		NetAmountPaise:     rec.NetAmountPaise,
		SettlementStatus:   rec.Status,
		RawPayload:         rawPayload,
	}
	if rec.CFPaymentID != "" {
		stlm.CFPaymentID = &rec.CFPaymentID
	}
	if !rec.TransferTime.IsZero() {
		stlm.TransferTime = &rec.TransferTime
	}

	// 1. Assert Conservation Invariant: Gross == Net + Fee + Tax + Adj
	expectedGross := rec.NetAmountPaise + rec.ServiceChargePaise + rec.ServiceTaxPaise + rec.AdjustmentPaise
	if rec.GrossAmountPaise != expectedGross {
		reason := fmt.Sprintf("arithmetic imbalance: gross %d != net %d + fee %d + tax %d + adj %d",
			rec.GrossAmountPaise, rec.NetAmountPaise, rec.ServiceChargePaise, rec.ServiceTaxPaise, rec.AdjustmentPaise)
		stlm.ReconciliationStatus = domain.ReconDiscrepancy
		stlm.DiscrepancyReason = &reason
		_ = r.Store.UpsertSettlement(ctx, stlm)
		return stlm, nil
	}

	// 2. Lookup Payment Intent
	var intent *domain.PaymentIntent
	if rec.OrderID != "" && r.Intents != nil {
		intent, _ = r.Intents.GetByOrderID(ctx, rec.OrderID)
	}

	if intent == nil {
		reason := fmt.Sprintf("payment intent not found for order_id %q", rec.OrderID)
		stlm.ReconciliationStatus = domain.ReconUnmatched
		stlm.DiscrepancyReason = &reason
		_ = r.Store.UpsertSettlement(ctx, stlm)
		return stlm, nil
	}

	stlm.PaymentIntentID = &intent.ID

	// Resolve Property ID via Due
	var propertyID uuid.UUID
	if r.Dues != nil && intent.DueID != uuid.Nil {
		due, _ := r.Dues.GetByID(ctx, intent.DueID)
		if due != nil {
			propertyID = due.PropertyID
			stlm.PropertyID = &due.PropertyID
		}
	}

	// 3. Assert Intent Amount Equality
	if rec.GrossAmountPaise != int64(intent.AmountPaise) {
		reason := fmt.Sprintf("intent amount mismatch: settlement gross %d != intent %d",
			rec.GrossAmountPaise, intent.AmountPaise)
		stlm.ReconciliationStatus = domain.ReconDiscrepancy
		stlm.DiscrepancyReason = &reason
		_ = r.Store.UpsertSettlement(ctx, stlm)
		return stlm, nil
	}

	// 4. Lookup associated internal payment record
	if rec.CFPaymentID != "" && r.Payments != nil {
		p, _ := r.Payments.GetByCFPaymentID(ctx, rec.CFPaymentID)
		if p != nil {
			stlm.PaymentID = &p.ID
		}
	}

	// 5. Post to Double-Entry Ledger via pure ProcessSettlement
	if r.Finance != nil && propertyID != uuid.Nil {
		financeRec := SettlementRecord{
			SettlementID:     rec.CFSettlementID,
			TransferUTR:      rec.UTR,
			TransferTime:     rec.TransferTime,
			GrossAmountPaise: rec.GrossAmountPaise,
			NetAmountPaise:   rec.NetAmountPaise,
			ServiceFeePaise:  rec.ServiceChargePaise,
			ServiceTaxPaise:  rec.ServiceTaxPaise,
			AdjustmentPaise:  rec.AdjustmentPaise,
		}
		err := r.Finance.ProcessSettlement(ctx, propertyID, financeRec)
		if err != nil && !errors.Is(err, ErrDuplicateSettlement) {
			reason := fmt.Sprintf("ledger posting error: %v", err)
			stlm.ReconciliationStatus = domain.ReconDiscrepancy
			stlm.DiscrepancyReason = &reason
			_ = r.Store.UpsertSettlement(ctx, stlm)
			return stlm, nil
		}

		// Deterministic journal entry UUID
		sourceID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("cashfree_settlement:"+rec.CFSettlementID))
		stlm.JournalEntryID = &sourceID
	}

	stlm.ReconciliationStatus = domain.ReconMatched
	stlm.DiscrepancyReason = nil
	if err := r.Store.UpsertSettlement(ctx, stlm); err != nil {
		return nil, fmt.Errorf("upsert reconciled settlement: %w", err)
	}

	return stlm, nil
}

// ReconcileWebhookSettlement processes an aggregate batch settlement from a webhook.
func (r *SettlementReconciler) ReconcileWebhookSettlement(
	ctx context.Context,
	rec *cashfree.SettlementWebhookRecord,
) (*domain.GatewaySettlement, error) {
	if r.Store == nil || rec == nil {
		return nil, errors.New("finance: invalid webhook reconciliation call")
	}

	stlm := &domain.GatewaySettlement{
		CFSettlementID:        rec.CFSettlementID,
		IngestionSource:       domain.IngestionWebhook,
		UTR:                   rec.UTR,
		Currency:              "INR",
		GrossAmountPaise:      rec.GrossAmountPaise,
		ServiceChargePaise:    rec.ServiceChargePaise,
		ServiceTaxPaise:       rec.ServiceTaxPaise,
		AdjustmentPaise:       rec.AdjustmentPaise,
		NetAmountPaise:        rec.NetAmountPaise,
		SettlementStatus:      rec.Status,
		SettledOn:             rec.SettledOn,
		SettlementInitiatedOn: rec.SettlementInitiatedOn,
		RawPayload:            rec.RawPayload,
	}

	// 1. Assert Conservation Invariant: Gross == Net + Fee + Tax + Adj
	expectedGross := rec.NetAmountPaise + rec.ServiceChargePaise + rec.ServiceTaxPaise + rec.AdjustmentPaise
	if rec.GrossAmountPaise != expectedGross {
		reason := fmt.Sprintf("arithmetic imbalance: gross %d != net %d + fee %d + tax %d + adj %d",
			rec.GrossAmountPaise, rec.NetAmountPaise, rec.ServiceChargePaise, rec.ServiceTaxPaise, rec.AdjustmentPaise)
		stlm.ReconciliationStatus = domain.ReconDiscrepancy
		stlm.DiscrepancyReason = &reason
		_ = r.Store.UpsertSettlement(ctx, stlm)
		return stlm, nil
	}

	stlm.ReconciliationStatus = domain.ReconMatched
	if err := r.Store.UpsertSettlement(ctx, stlm); err != nil {
		return nil, fmt.Errorf("upsert batch settlement: %w", err)
	}

	return stlm, nil
}
