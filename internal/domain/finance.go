package domain

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound             = errors.New("not found")
	ErrDuplicateIdempotency = errors.New("duplicate idempotency key")
	ErrIdempotencyConflict  = errors.New("idempotency conflict: payload does not match existing record")
	// ErrPeriodClosed: a ledger write targets an accounting period whose tie-out is closed
	// (DB controls C-3/C-4, migration 044). Reopening is an audited, privileged act.
	ErrPeriodClosed = errors.New("accounting period is closed")
	// ErrPeriodNotReopenable is returned when attempting to reopen an already-open period.
	ErrPeriodNotReopenable = errors.New("accounting period is not closed")
	ErrForbidden           = errors.New("forbidden")
	ErrExpenseNotPayable   = errors.New("expense cannot accept payment")
	ErrOverpay             = errors.New("payment exceeds remaining expense")
	// ErrExpenseStateChanged: the expense status changed between read and write (optimistic lock failed).
	ErrExpenseStateChanged = errors.New("expense state changed during update; retry")
)

// Chart of accounts (operating + capital). Amounts always paise.
const (
	AcctCash                     = "cash"
	AcctBank                     = "bank"
	AcctOwnerCapital             = "owner_capital"
	AcctDepositLiability         = "deposit_liability"
	AcctTenantReceivable         = "tenant_receivable"
	AcctRentRevenue              = "rent_revenue"
	AcctUtilityRecoveryRevenue   = "utility_recovery_revenue"
	AcctOperatingExpense         = "operating_expense"
	AcctAccountsPayable          = "accounts_payable"
	AcctManagerAdvancePayable    = "manager_advance_payable"
	AcctRewardLiability          = "reward_liability"
	AcctLoyaltyExpense           = "loyalty_expense"
	AcctPaymentProcessingExpense = "payment_processing_expense"
	AcctGatewayClearing          = "gateway_clearing"
	AcctUnappliedReceipts        = "unapplied_receipts"
	AcctDamagesIncome            = "damages_income"
	AcctRefundPayable            = "refund_payable"
	AcctGatewayAdjustment        = "gateway_adjustment"
	AcctInterestIncome           = "interest_income"
	AcctNonPGOtherIncome         = "non_pg_other_income"
)

type CapitalKind string

const (
	CapitalInitial    CapitalKind = "initial"
	CapitalAdditional CapitalKind = "additional"
	CapitalWithdrawal CapitalKind = "withdrawal"
)

type ExpenseStatus string

const (
	ExpenseDraft           ExpenseStatus = "draft"
	ExpensePendingApproval ExpenseStatus = "pending_approval"
	ExpenseApproved        ExpenseStatus = "approved"
	ExpensePaid            ExpenseStatus = "paid"
	ExpenseCancelled       ExpenseStatus = "cancelled"
)

type PayerRole string

const (
	PayerOwner   PayerRole = "owner"
	PayerManager PayerRole = "manager"
)

type CapitalTransaction struct {
	ID             uuid.UUID   `json:"id"`
	PropertyID     uuid.UUID   `json:"property_id"`
	OwnerUserID    uuid.UUID   `json:"owner_user_id"`
	Kind           CapitalKind `json:"kind"`
	AmountPaise    int64       `json:"amount_paise"`
	Purpose        string      `json:"purpose,omitempty"`
	Reference      string      `json:"reference"`
	IdempotencyKey string      `json:"-"`
	OccurredAt     time.Time   `json:"occurred_at"`
	CreatedAt      time.Time   `json:"created_at"`
}

type Expense struct {
	ID             uuid.UUID     `json:"id"`
	PropertyID     uuid.UUID     `json:"property_id"`
	CategoryCode   string        `json:"category_code"`
	VendorName     string        `json:"vendor_name,omitempty"`
	Description    string        `json:"description,omitempty"`
	AmountPaise    int64         `json:"amount_paise"`
	Status         ExpenseStatus `json:"status"`
	Emergency      bool          `json:"emergency"`
	IsRecurring    bool          `json:"is_recurring"`
	RoomID         *uuid.UUID    `json:"room_id,omitempty"`
	CreatedBy      uuid.UUID     `json:"created_by"`
	CreatedByRole  string        `json:"created_by_role"`
	IdempotencyKey string        `json:"-"`
	OccurredAt     time.Time     `json:"occurred_at"`
	CreatedAt      time.Time     `json:"created_at"`
	PaidPaise      int64         `json:"paid_paise,omitempty"`
}

// ExpenseVoid carries the audit data written when an expense is voided.
type ExpenseVoid struct {
	Reason   string
	VoidedBy uuid.UUID
	VoidedAt time.Time
}

type DailyFinancialRollup struct {
	ID               uuid.UUID `json:"id"`
	PropertyID       uuid.UUID `json:"property_id"`
	RollupDate       time.Time `json:"rollup_date"` // YYYY-MM-DD
	CollectedPaise   int64     `json:"collected_paise"`
	DuePaise         int64     `json:"due_paise"`
	ExpensePaise     int64     `json:"expense_paise"`
	NetCashFlowPaise int64     `json:"net_cash_flow_paise"`
	TotalRooms       int       `json:"total_rooms"`
	OccupiedRooms    int       `json:"occupied_rooms"`
	CapacityBeds     int       `json:"capacity_beds"`
	OccupiedBeds     int       `json:"occupied_beds"`
	OccupancyRatePct float64   `json:"occupancy_rate_pct"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type ExpensePayment struct {
	ID             uuid.UUID `json:"id"`
	ExpenseID      uuid.UUID `json:"expense_id"`
	PropertyID     uuid.UUID `json:"property_id"`
	AmountPaise    int64     `json:"amount_paise"`
	PayerRole      PayerRole `json:"payer_role"`
	PayerUserID    uuid.UUID `json:"payer_user_id"`
	Method         string    `json:"method"`
	IdempotencyKey string    `json:"-"`
	OccurredAt     time.Time `json:"occurred_at"`
}

type ManagerAdvance struct {
	ID               uuid.UUID `json:"id"`
	PropertyID       uuid.UUID `json:"property_id"`
	ManagerUserID    uuid.UUID `json:"manager_user_id"`
	ExpensePaymentID uuid.UUID `json:"expense_payment_id"`
	AmountPaise      int64     `json:"amount_paise"`
	OccurredAt       time.Time `json:"occurred_at"`
}

type ManagerReimbursement struct {
	ID             uuid.UUID `json:"id"`
	PropertyID     uuid.UUID `json:"property_id"`
	ManagerUserID  uuid.UUID `json:"manager_user_id"`
	AmountPaise    int64     `json:"amount_paise"`
	RecordedBy     uuid.UUID `json:"recorded_by"`
	IdempotencyKey string    `json:"-"`
	OccurredAt     time.Time `json:"occurred_at"`
}

type JournalLine struct {
	ID          uuid.UUID `json:"id"`
	PropertyID  uuid.UUID `json:"property_id"`
	AccountCode string    `json:"account_code"`
	DebitPaise  int64     `json:"debit_paise"`
	CreditPaise int64     `json:"credit_paise"`
	SourceType  string    `json:"source_type"`
	SourceID    uuid.UUID `json:"source_id"`
	LineKind    string    `json:"line_kind"`
	OccurredAt  time.Time `json:"occurred_at"`
}

type ApprovalPolicy struct {
	PropertyID                  uuid.UUID `json:"property_id"`
	ManagerDailyLimitPaise      int64     `json:"manager_daily_limit_paise"`
	SingleExpenseLimitPaise     int64     `json:"single_expense_limit_paise"`
	ManagerMonthlyLimitPaise    int64     `json:"manager_monthly_limit_paise"`
	OwnerApprovalThresholdPaise int64     `json:"owner_approval_threshold_paise"`
	ReimbursementThresholdPaise int64     `json:"reimbursement_threshold_paise"`
	EmergencyBypassEnabled      bool      `json:"emergency_bypass_enabled"`
}

type PropertyFinanceSettings struct {
	PropertyID            uuid.UUID `json:"property_id"`
	FiscalMonthStartDay   int16     `json:"fiscal_month_start_day"`
	ManagerCanViewCapital bool      `json:"manager_can_view_capital"`
	ManagerCanViewROI     bool      `json:"manager_can_view_roi"`
	ManagerCanViewLeakage bool      `json:"manager_can_view_leakage"`
	TDREffectiveBPS       int       `json:"tdr_effective_bps"`
	TDRIsEstimated        bool      `json:"tdr_is_estimated"`
}

type FinanceSettingsPatch struct {
	FiscalMonthStartDay   *int16 `json:"fiscal_month_start_day"`
	ManagerCanViewCapital *bool  `json:"manager_can_view_capital"`
	ManagerCanViewROI     *bool  `json:"manager_can_view_roi"`
	ManagerCanViewLeakage *bool  `json:"manager_can_view_leakage"`
	TDREffectiveBPS       *int   `json:"tdr_effective_bps"`
	TDRIsEstimated        *bool  `json:"tdr_is_estimated"`
}

type ApprovalPolicyPatch struct {
	ManagerDailyLimitPaise      *int64 `json:"manager_daily_limit_paise"`
	SingleExpenseLimitPaise     *int64 `json:"single_expense_limit_paise"`
	ManagerMonthlyLimitPaise    *int64 `json:"manager_monthly_limit_paise"`
	OwnerApprovalThresholdPaise *int64 `json:"owner_approval_threshold_paise"`
	ReimbursementThresholdPaise *int64 `json:"reimbursement_threshold_paise"`
	EmergencyBypassEnabled      *bool  `json:"emergency_bypass_enabled"`
}

type Budget struct {
	ID           uuid.UUID `json:"id"`
	PropertyID   uuid.UUID `json:"property_id"`
	CategoryCode string    `json:"category_code"`
	PeriodMonth  string    `json:"period_month"`
	AmountPaise  int64     `json:"amount_paise"`
}

type RewardLiabilityTxn struct {
	ID          uuid.UUID  `json:"id"`
	PropertyID  uuid.UUID  `json:"property_id"`
	TenantID    *uuid.UUID `json:"tenant_id,omitempty"`
	Kind        string     `json:"kind"`
	Points      int        `json:"points"`
	AmountPaise int64      `json:"amount_paise"`
	SourceType  string     `json:"source_type"`
	SourceID    uuid.UUID  `json:"source_id"`
	OccurredAt  time.Time  `json:"occurred_at"`
}

type TieOutItem struct {
	Category         string `json:"category"` // timing | adjustment | proration | reward_credit | investigate
	AmountPaise      int64  `json:"amount_paise"`
	SourceID         string `json:"source_id,omitempty"`
	Note             string `json:"note,omitempty"`
	UnresolvedMonths int    `json:"unresolved_months,omitempty"`
}

type PeriodTieOut struct {
	ID               uuid.UUID    `json:"id"`
	PropertyID       uuid.UUID    `json:"property_id"`
	PeriodMonth      string       `json:"period_month"`
	ReconTotalPaise  int64        `json:"recon_total_paise"`
	LedgerTotalPaise int64        `json:"ledger_total_paise"`
	DifferencePaise  int64        `json:"difference_paise"`
	Items            []TieOutItem `json:"items"`
	Status           string       `json:"status"`
	ClosedAt         *time.Time   `json:"closed_at,omitempty"`
}

type ApprovalRequest struct {
	ID          uuid.UUID  `json:"id"`
	PropertyID  uuid.UUID  `json:"property_id"`
	Kind        string     `json:"kind"`
	SubjectID   uuid.UUID  `json:"subject_id"`
	AmountPaise int64      `json:"amount_paise"`
	RequestedBy uuid.UUID  `json:"requested_by"`
	Status      string     `json:"status"`
	DecidedBy   *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	Note        string     `json:"note,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

type VarianceLine struct {
	Driver      string `json:"driver"`
	AmountPaise int64  `json:"amount_paise"`
	Kind        string `json:"kind"` // actual | estimated
}

type VarianceBridge struct {
	PeriodMonth      string         `json:"period_month"`
	BudgetedOCFPaise int64          `json:"budgeted_ocf_paise"`
	ActualOCFPaise   int64          `json:"actual_ocf_paise"`
	Lines            []VarianceLine `json:"lines"`
	ResidualPaise    int64          `json:"residual_paise"`
}

type KPISnapshot struct {
	PropertyID              uuid.UUID       `json:"property_id"`
	PeriodMonth             string          `json:"period_month"`
	SnapshotDate            time.Time       `json:"snapshot_date"`
	OccupancyBPS            int             `json:"occupancy_bps"`
	OccupiedBeds            int             `json:"occupied_beds"`
	CapacityBeds            int             `json:"capacity_beds"`
	ContributionPerBedPaise int64           `json:"contribution_per_bed_paise"`
	OpexPaise               int64           `json:"opex_paise"`
	OCFPaise                int64           `json:"ocf_paise"`
	LeakageTotalPaise       int64           `json:"leakage_total_paise"`
	VarianceBridge          json.RawMessage `json:"variance_bridge_json,omitempty"`
}

type ROISnapshot struct {
	PropertyID            uuid.UUID `json:"property_id"`
	PeriodMonth           string    `json:"period_month"`
	SnapshotDate          time.Time `json:"snapshot_date"`
	CapitalInvestedPaise  int64     `json:"capital_invested_paise"`
	CapitalRecoveredPaise int64     `json:"capital_recovered_paise"`
	UnrecoveredPaise      int64     `json:"unrecovered_paise"`
	TBEMonthsMilli        *int      `json:"tbe_months_milli,omitempty"`
	BreakEvenOccupancyBPS *int      `json:"break_even_occupancy_bps,omitempty"`
	Official              bool      `json:"official"`
}

type LeakageEvent struct {
	ID             uuid.UUID       `json:"id"`
	PropertyID     uuid.UUID       `json:"property_id"`
	Category       string          `json:"category"`
	EstimatedPaise int64           `json:"estimated_paise"`
	ConfidenceBPS  int             `json:"confidence_bps"`
	Evidence       json.RawMessage `json:"evidence"`
	Severity       int             `json:"severity"`
	Status         string          `json:"status"`
	DetectedAt     time.Time       `json:"detected_at"`
}

type Recommendation struct {
	ID                   uuid.UUID  `json:"id"`
	PropertyID           uuid.UUID  `json:"property_id"`
	LeakageEventID       *uuid.UUID `json:"leakage_event_id,omitempty"`
	Issue                string     `json:"issue"`
	SuggestedAction      string     `json:"suggested_action"`
	ExpectedSavingsPaise int64      `json:"expected_savings_paise"`
	Effort               string     `json:"effort"`
	Risk                 string     `json:"risk"`
	ConfidenceBPS        int        `json:"confidence_bps"`
	SafetyOK             bool       `json:"safety_ok"`
	QualityOK            bool       `json:"quality_ok"`
	Status               string     `json:"status"`
	RealizedSavingsPaise *int64     `json:"realized_savings_paise,omitempty"`
	AcceptedAt           *time.Time `json:"accepted_at,omitempty"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

type ExpenseImportSuggestion struct {
	ID          uuid.UUID `json:"id"`
	PropertyID  uuid.UUID `json:"property_id"`
	TxnID       string    `json:"txn_id"`
	AmountPaise int64     `json:"amount_paise"`
	TxnDate     time.Time `json:"txn_date"`
	Note        string    `json:"note"`
	Status      string    `json:"status"`
}

type MealPrepActual struct {
	PropertyID     uuid.UUID `json:"property_id"`
	MealDate       time.Time `json:"meal_date"`
	MealSlot       string    `json:"meal_slot"`
	PreparedCount  int       `json:"prepared_count"`
	DiscardedCount int       `json:"discarded_count"`
}

type ForecastSnapshot struct {
	PropertyID  uuid.UUID       `json:"property_id"`
	HorizonDays int             `json:"horizon_days"`
	AsOf        time.Time       `json:"as_of"`
	Payload     json.RawMessage `json:"payload"`
}

type IngestionSource string

const (
	IngestionWebhook    IngestionSource = "webhook"
	IngestionOrderFetch IngestionSource = "order_fetch"
	IngestionCSVImport  IngestionSource = "csv_import"
)

type SettlementReconStatus string

const (
	ReconMatched            SettlementReconStatus = "matched"
	ReconUnmatched          SettlementReconStatus = "unmatched"
	ReconDiscrepancy        SettlementReconStatus = "discrepancy"
	ReconManuallyReconciled SettlementReconStatus = "manually_reconciled"
)

type GatewaySettlement struct {
	ID                    uuid.UUID             `json:"id"`
	PropertyID            *uuid.UUID            `json:"property_id,omitempty"`
	CFSettlementID        string                `json:"cf_settlement_id"`
	OrderID               *string               `json:"order_id,omitempty"`
	CFPaymentID           *string               `json:"cf_payment_id,omitempty"`
	PaymentIntentID       *uuid.UUID            `json:"payment_intent_id,omitempty"`
	PaymentID             *uuid.UUID            `json:"payment_id,omitempty"`
	IngestionSource       IngestionSource       `json:"ingestion_source"`
	UTR                   string                `json:"utr"`
	Currency              string                `json:"currency"`
	GrossAmountPaise      int64                 `json:"gross_amount_paise"`
	ServiceChargePaise    int64                 `json:"service_charge_paise"`
	ServiceTaxPaise       int64                 `json:"service_tax_paise"`
	AdjustmentPaise       int64                 `json:"adjustment_paise"`
	NetAmountPaise        int64                 `json:"net_amount_paise"`
	SettlementStatus      string                `json:"settlement_status"`
	SettledOn             *time.Time            `json:"settled_on,omitempty"`
	SettlementInitiatedOn *time.Time            `json:"settlement_initiated_on,omitempty"`
	TransferTime          *time.Time            `json:"transfer_time,omitempty"`
	ReconciliationStatus  SettlementReconStatus `json:"reconciliation_status"`
	DiscrepancyReason     *string               `json:"discrepancy_reason,omitempty"`
	JournalEntryID        *uuid.UUID            `json:"journal_entry_id,omitempty"`
	ResolutionNotes       *string               `json:"resolution_notes,omitempty"`
	ResolvedBy            *uuid.UUID            `json:"resolved_by,omitempty"`
	ResolvedAt            *time.Time            `json:"resolved_at,omitempty"`
	RawPayload            json.RawMessage       `json:"raw_payload"`
	CreatedAt             time.Time             `json:"created_at"`
	UpdatedAt             time.Time             `json:"updated_at"`
}

type SettlementFilter struct {
	Status   *SettlementReconStatus
	Source   *IngestionSource
	FromDate *time.Time
	ToDate   *time.Time
	Limit    int
	Offset   int
}

type DiscrepancyItem struct {
	Category    string `json:"category"` // gateway_in_transit, bank_unmatched, unbalanced_ledger, missing_settlement_deposit
	AmountPaise int64  `json:"amount_paise"`
	Note        string `json:"note"`
	Severity    string `json:"severity"` // info, warning, critical
}

type DailySettlementBalance struct {
	ID                       uuid.UUID         `json:"id"`
	PropertyID               uuid.UUID         `json:"property_id"`
	ReconDate                time.Time         `json:"recon_date"`
	GatewayGrossPaise        int64             `json:"gateway_gross_paise"`
	GatewayNetSettledPaise   int64             `json:"gateway_net_settled_paise"`
	GatewayFeesPaise         int64             `json:"gateway_fees_paise"`
	GatewayTaxPaise          int64             `json:"gateway_tax_paise"`
	GatewayAdjustmentPaise   int64             `json:"gateway_adjustment_paise"`
	GatewayInTransitPaise    int64             `json:"gateway_in_transit_paise"`
	BankCreditsPaise         int64             `json:"bank_credits_paise"`
	BankDebitsPaise          int64             `json:"bank_debits_paise"`
	UnappliedQuarantinePaise int64             `json:"unapplied_quarantine_paise"`
	LedgerBankDrPaise        int64             `json:"ledger_bank_dr_paise"`
	LedgerBankCrPaise        int64             `json:"ledger_bank_cr_paise"`
	IsBalanced               bool              `json:"is_balanced"`
	DiscrepancyPaise         int64             `json:"discrepancy_paise"`
	Discrepancies            []DiscrepancyItem `json:"discrepancies"`
	Metadata                 map[string]any    `json:"metadata"`
	CreatedAt                time.Time         `json:"created_at"`
	UpdatedAt                time.Time         `json:"updated_at"`
}

// EvaluateBalance performs deterministic multi-way mathematical checks across gateway, bank, and ledger legs.
func (b *DailySettlementBalance) EvaluateBalance() {
	var items []DiscrepancyItem
	var totalDiscrepancy int64

	// 1. Gateway settlement decomposition check
	gwExpectedGross := b.GatewayNetSettledPaise + b.GatewayFeesPaise + b.GatewayTaxPaise + b.GatewayAdjustmentPaise
	if b.GatewayGrossPaise != gwExpectedGross {
		diff := b.GatewayGrossPaise - gwExpectedGross
		absDiff := diff
		if absDiff < 0 {
			absDiff = -absDiff
		}
		items = append(items, DiscrepancyItem{
			Category:    "gateway_settlement_imbalance",
			AmountPaise: absDiff,
			Note:        "Gateway gross does not equal net settled + fees + tax + adjustment",
			Severity:    "critical",
		})
		totalDiscrepancy += absDiff
	}

	// 2. Bank Cleared Receipts vs Ledger Bank Debits drift
	bankCreditDiff := b.BankCreditsPaise - b.LedgerBankDrPaise
	if bankCreditDiff != 0 {
		absDiff := bankCreditDiff
		if absDiff < 0 {
			absDiff = -absDiff
		}
		items = append(items, DiscrepancyItem{
			Category:    "bank_receipt_vs_ledger_drift",
			AmountPaise: absDiff,
			Note:        "Cleared bank statement credits differ from general ledger bank debits",
			Severity:    "warning",
		})
		totalDiscrepancy += absDiff
	}

	// 3. Bank Cleared Disbursements vs Ledger Bank Credits drift
	bankDebitDiff := b.BankDebitsPaise - b.LedgerBankCrPaise
	if bankDebitDiff != 0 {
		absDiff := bankDebitDiff
		if absDiff < 0 {
			absDiff = -absDiff
		}
		items = append(items, DiscrepancyItem{
			Category:    "bank_disbursement_vs_ledger_drift",
			AmountPaise: absDiff,
			Note:        "Cleared bank statement debits differ from general ledger bank credits",
			Severity:    "warning",
		})
		totalDiscrepancy += absDiff
	}

	// 4. Gateway In-Transit status
	if b.GatewayInTransitPaise < 0 {
		absDiff := -b.GatewayInTransitPaise
		items = append(items, DiscrepancyItem{
			Category:    "negative_gateway_in_transit",
			AmountPaise: absDiff,
			Note:        "Cumulative settlements exceed cumulative gateway collections",
			Severity:    "critical",
		})
		totalDiscrepancy += absDiff
	} else if b.GatewayInTransitPaise > 0 {
		items = append(items, DiscrepancyItem{
			Category:    "gateway_in_transit",
			AmountPaise: b.GatewayInTransitPaise,
			Note:        "Collections in gateway clearing awaiting standard settlement",
			Severity:    "info",
		})
	}

	// 5. Unapplied Quarantine status
	if b.UnappliedQuarantinePaise > 0 {
		items = append(items, DiscrepancyItem{
			Category:    "unapplied_receipts_quarantine",
			AmountPaise: b.UnappliedQuarantinePaise,
			Note:        "Bank deposits quarantined pending owner confirmation",
			Severity:    "info",
		})
	}

	b.Discrepancies = items
	b.DiscrepancyPaise = totalDiscrepancy
	b.IsBalanced = (totalDiscrepancy == 0)
}

// StatementLine represents a line in the income statement or balance sheet.
type StatementLine struct {
	Section     string  `json:"section"`
	AccountCode *string `json:"account_code,omitempty"`
	AmountPaise int64   `json:"amount_paise"`
	SortOrder   int     `json:"sort_order"`
}

// TrialBalanceLine represents an account row in the trial balance.
type TrialBalanceLine struct {
	AccountCode  string `json:"account_code"`
	AccountClass string `json:"account_class"`
	DebitPaise   int64  `json:"debit_paise"`
	CreditPaise  int64  `json:"credit_paise"`
	BalancePaise int64  `json:"balance_paise"`
}

// CashFlowLine represents a line in the direct-method cash flow statement.
type CashFlowLine struct {
	Section     string  `json:"section"`
	Label       *string `json:"label,omitempty"`
	AmountPaise int64   `json:"amount_paise"`
	SortOrder   int     `json:"sort_order"`
}

// ReconcilingItem represents an open control or money-integrity item in the aged ledger review.
type ReconcilingItem struct {
	ItemType     string    `json:"item_type"`
	PropertyID   uuid.UUID `json:"property_id"`
	Ref          string    `json:"ref"`
	AmountPaise  *int64    `json:"amount_paise,omitempty"`
	OriginatedOn string    `json:"originated_on"`
	AgeDays      int       `json:"age_days"`
	AgeBucket    string    `json:"age_bucket"`
	Category     string    `json:"category"`
	Escalation   string    `json:"escalation"`
}
