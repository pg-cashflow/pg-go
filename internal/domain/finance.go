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
)

// Chart of accounts (operating + capital). Amounts always paise.
const (
	AcctCash                    = "cash"
	AcctBank                    = "bank"
	AcctOwnerCapital            = "owner_capital"
	AcctDepositLiability        = "deposit_liability"
	AcctTenantReceivable        = "tenant_receivable"
	AcctRentRevenue             = "rent_revenue"
	AcctUtilityRecoveryRevenue  = "utility_recovery_revenue"
	AcctOperatingExpense        = "operating_expense"
	AcctAccountsPayable         = "accounts_payable"
	AcctManagerAdvancePayable   = "manager_advance_payable"
	AcctRewardLiability         = "reward_liability"
	AcctLoyaltyExpense          = "loyalty_expense"
	AcctPaymentProcessingExpense = "payment_processing_expense"
	AcctGatewayClearing          = "gateway_clearing"
	AcctUnappliedReceipts        = "unapplied_receipts"
	AcctDamagesIncome            = "damages_income"
	AcctRefundPayable            = "refund_payable"
	AcctGatewayAdjustment        = "gateway_adjustment"
)

type CapitalKind string

const (
	CapitalInitial    CapitalKind = "initial"
	CapitalAdditional CapitalKind = "additional"
	CapitalWithdrawal CapitalKind = "withdrawal"
)

type ExpenseStatus string

const (
	ExpenseDraft            ExpenseStatus = "draft"
	ExpensePendingApproval  ExpenseStatus = "pending_approval"
	ExpenseApproved         ExpenseStatus = "approved"
	ExpensePaid             ExpenseStatus = "paid"
	ExpenseCancelled        ExpenseStatus = "cancelled"
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
	RoomID         *uuid.UUID    `json:"room_id,omitempty"`
	CreatedBy      uuid.UUID     `json:"created_by"`
	CreatedByRole  string        `json:"created_by_role"`
	IdempotencyKey string        `json:"-"`
	OccurredAt     time.Time     `json:"occurred_at"`
	CreatedAt      time.Time     `json:"created_at"`
	PaidPaise      int64         `json:"paid_paise,omitempty"`
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
	PropertyID                 uuid.UUID `json:"property_id"`
	ManagerDailyLimitPaise     int64     `json:"manager_daily_limit_paise"`
	SingleExpenseLimitPaise    int64     `json:"single_expense_limit_paise"`
	ManagerMonthlyLimitPaise   int64     `json:"manager_monthly_limit_paise"`
	OwnerApprovalThresholdPaise int64    `json:"owner_approval_threshold_paise"`
	ReimbursementThresholdPaise int64    `json:"reimbursement_threshold_paise"`
	EmergencyBypassEnabled     bool      `json:"emergency_bypass_enabled"`
}

type PropertyFinanceSettings struct {
	PropertyID             uuid.UUID `json:"property_id"`
	FiscalMonthStartDay    int16     `json:"fiscal_month_start_day"`
	ManagerCanViewCapital  bool      `json:"manager_can_view_capital"`
	ManagerCanViewROI      bool      `json:"manager_can_view_roi"`
	ManagerCanViewLeakage  bool      `json:"manager_can_view_leakage"`
	TDREffectiveBPS        int       `json:"tdr_effective_bps"`
	TDRIsEstimated         bool      `json:"tdr_is_estimated"`
}

type Budget struct {
	ID           uuid.UUID `json:"id"`
	PropertyID   uuid.UUID `json:"property_id"`
	CategoryCode string    `json:"category_code"`
	PeriodMonth  string    `json:"period_month"`
	AmountPaise  int64     `json:"amount_paise"`
}

type RewardLiabilityTxn struct {
	ID          uuid.UUID `json:"id"`
	PropertyID  uuid.UUID `json:"property_id"`
	TenantID    *uuid.UUID `json:"tenant_id,omitempty"`
	Kind        string    `json:"kind"`
	Points      int       `json:"points"`
	AmountPaise int64     `json:"amount_paise"`
	SourceType  string    `json:"source_type"`
	SourceID    uuid.UUID `json:"source_id"`
	OccurredAt  time.Time `json:"occurred_at"`
}

type TieOutItem struct {
	Category     string `json:"category"` // timing | adjustment | proration | reward_credit | investigate
	AmountPaise  int64  `json:"amount_paise"`
	SourceID     string `json:"source_id,omitempty"`
	Note         string `json:"note,omitempty"`
	UnresolvedMonths int `json:"unresolved_months,omitempty"`
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
	PeriodMonth            string         `json:"period_month"`
	BudgetedOCFPaise       int64          `json:"budgeted_ocf_paise"`
	ActualOCFPaise         int64          `json:"actual_ocf_paise"`
	Lines                  []VarianceLine `json:"lines"`
	ResidualPaise          int64          `json:"residual_paise"`
}

type KPISnapshot struct {
	PropertyID                 uuid.UUID       `json:"property_id"`
	PeriodMonth                string          `json:"period_month"`
	SnapshotDate               time.Time       `json:"snapshot_date"`
	OccupancyBPS               int             `json:"occupancy_bps"`
	OccupiedBeds               int             `json:"occupied_beds"`
	CapacityBeds               int             `json:"capacity_beds"`
	ContributionPerBedPaise    int64           `json:"contribution_per_bed_paise"`
	OpexPaise                  int64           `json:"opex_paise"`
	OCFPaise                   int64           `json:"ocf_paise"`
	LeakageTotalPaise          int64           `json:"leakage_total_paise"`
	VarianceBridge             json.RawMessage `json:"variance_bridge_json,omitempty"`
}

type ROISnapshot struct {
	PropertyID             uuid.UUID `json:"property_id"`
	PeriodMonth            string    `json:"period_month"`
	SnapshotDate           time.Time `json:"snapshot_date"`
	CapitalInvestedPaise   int64     `json:"capital_invested_paise"`
	CapitalRecoveredPaise  int64     `json:"capital_recovered_paise"`
	UnrecoveredPaise       int64     `json:"unrecovered_paise"`
	TBEMonthsMilli         *int      `json:"tbe_months_milli,omitempty"`
	BreakEvenOccupancyBPS  *int      `json:"break_even_occupancy_bps,omitempty"`
	Official               bool      `json:"official"`
}

type LeakageEvent struct {
	ID              uuid.UUID       `json:"id"`
	PropertyID      uuid.UUID       `json:"property_id"`
	Category        string          `json:"category"`
	EstimatedPaise  int64           `json:"estimated_paise"`
	ConfidenceBPS   int             `json:"confidence_bps"`
	Evidence        json.RawMessage `json:"evidence"`
	Severity        int             `json:"severity"`
	Status          string          `json:"status"`
	DetectedAt      time.Time       `json:"detected_at"`
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
