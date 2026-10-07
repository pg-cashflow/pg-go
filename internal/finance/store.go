package finance

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// Store persists finance records. Implementations: MemoryStore (tests) and postgres.FinanceRepo.
type Store interface {
	EnsureDefaults(ctx context.Context, propertyID uuid.UUID) error

	GetPolicy(ctx context.Context, propertyID uuid.UUID) (domain.ApprovalPolicy, error)
	SavePolicy(ctx context.Context, p domain.ApprovalPolicy) error
	GetSettings(ctx context.Context, propertyID uuid.UUID) (domain.PropertyFinanceSettings, error)
	SaveSettings(ctx context.Context, s domain.PropertyFinanceSettings) error
	SaveUnifiedSettings(ctx context.Context, propertyID uuid.UUID, settings *domain.PropertyFinanceSettings, policy *domain.ApprovalPolicy, loyalty *domain.PropertyGamificationSettings) error

	GetPropertyOwnerUserID(ctx context.Context, propertyID uuid.UUID) (uuid.UUID, error)

	InsertCapital(ctx context.Context, tx *domain.CapitalTransaction) error
	InsertCapitalAtomic(ctx context.Context, tx *domain.CapitalTransaction, lines []domain.JournalLine) error
	ListCapital(ctx context.Context, propertyID uuid.UUID) ([]domain.CapitalTransaction, error)
	CountCapital(ctx context.Context, propertyID uuid.UUID) (int, error)

	InsertExpense(ctx context.Context, e *domain.Expense) error
	InsertExpenseAtomic(ctx context.Context, e *domain.Expense, lines []domain.JournalLine, approval *domain.ApprovalRequest) error
	// VoidExpenseAtomic cancels the expense only if its status still equals expected (optimistic lock),
	// cancels its pending approval, and writes the reversal lines in the same transaction.
	VoidExpenseAtomic(ctx context.Context, expenseID uuid.UUID, expected domain.ExpenseStatus, lines []domain.JournalLine, v domain.ExpenseVoid) error
	GetExpense(ctx context.Context, id uuid.UUID) (*domain.Expense, error)
	ListExpenses(ctx context.Context, propertyID uuid.UUID) ([]domain.Expense, error)
	UpdateExpenseStatus(ctx context.Context, id uuid.UUID, status domain.ExpenseStatus) error

	InsertExpensePayment(ctx context.Context, p *domain.ExpensePayment) error
	RecordExpensePaymentAtomic(ctx context.Context, p *domain.ExpensePayment, lines []domain.JournalLine, adv *domain.ManagerAdvance) (*domain.Expense, error)
	ListExpensePayments(ctx context.Context, expenseID uuid.UUID) ([]domain.ExpensePayment, error)
	SumExpensePayments(ctx context.Context, expenseID uuid.UUID) (int64, error)
	SumManagerSpend(ctx context.Context, propertyID, managerID uuid.UUID, from, to time.Time) (int64, error)

	InsertAdvance(ctx context.Context, a *domain.ManagerAdvance) error
	InsertReimbursement(ctx context.Context, r *domain.ManagerReimbursement) error
	InsertReimbursementAtomic(ctx context.Context, r *domain.ManagerReimbursement, lines []domain.JournalLine) error
	AdvanceOutstanding(ctx context.Context, propertyID uuid.UUID) (int64, error)
	ManagerAdvanceOutstanding(ctx context.Context, propertyID, managerID uuid.UUID) (int64, error)
	ListAdvances(ctx context.Context, propertyID uuid.UUID) ([]domain.ManagerAdvance, error)

	InsertJournal(ctx context.Context, lines []domain.JournalLine) error
	ListJournal(ctx context.Context, propertyID uuid.UUID, from, to time.Time, account string) ([]domain.JournalLine, error)
	SumAccount(ctx context.Context, propertyID uuid.UUID, account string, from, to time.Time) (debit, credit int64, err error)
	SumAccountNetCredit(ctx context.Context, propertyID uuid.UUID, account string, from, to time.Time) (int64, error)

	UpsertBudget(ctx context.Context, b *domain.Budget) error
	GetBudget(ctx context.Context, propertyID uuid.UUID, category, period string) (*domain.Budget, error)
	ListBudgets(ctx context.Context, propertyID uuid.UUID, period string) ([]domain.Budget, error)

	InsertRewardLiability(ctx context.Context, t *domain.RewardLiabilityTxn) error
	SumRewardLiability(ctx context.Context, propertyID uuid.UUID, kind string, from, to time.Time) (int64, error)
	SumRewardPointsIssued(ctx context.Context, propertyID uuid.UUID, from, to time.Time) (int, error)

	GetTieOut(ctx context.Context, propertyID uuid.UUID, period string) (*domain.PeriodTieOut, error)
	SaveTieOut(ctx context.Context, t *domain.PeriodTieOut) error
	ReopenTieOut(ctx context.Context, propertyID uuid.UUID, period string, actor string) error
	ListTieOuts(ctx context.Context, propertyID uuid.UUID, limit int) ([]domain.PeriodTieOut, error)

	InsertApproval(ctx context.Context, a *domain.ApprovalRequest) error
	GetApproval(ctx context.Context, id uuid.UUID) (*domain.ApprovalRequest, error)
	ListApprovals(ctx context.Context, propertyID uuid.UUID, status string) ([]domain.ApprovalRequest, error)
	UpdateApproval(ctx context.Context, a *domain.ApprovalRequest) error
	DecideApprovalAtomic(ctx context.Context, a *domain.ApprovalRequest, expenseStatus *domain.ExpenseStatus, lines []domain.JournalLine) error

	InsertKPI(ctx context.Context, s *domain.KPISnapshot) error
	InsertROI(ctx context.Context, s *domain.ROISnapshot) error
	LatestROI(ctx context.Context, propertyID uuid.UUID) (*domain.ROISnapshot, error)

	InsertLeakage(ctx context.Context, e *domain.LeakageEvent) error
	ListLeakage(ctx context.Context, propertyID uuid.UUID) ([]domain.LeakageEvent, error)
	GetLeakage(ctx context.Context, id uuid.UUID) (*domain.LeakageEvent, error)

	InsertRecommendation(ctx context.Context, r *domain.Recommendation) error
	ListRecommendations(ctx context.Context, propertyID uuid.UUID) ([]domain.Recommendation, error)
	GetRecommendation(ctx context.Context, id uuid.UUID) (*domain.Recommendation, error)
	UpdateRecommendation(ctx context.Context, r *domain.Recommendation) error

	InsertForecast(ctx context.Context, f *domain.ForecastSnapshot) error
	LatestForecast(ctx context.Context, propertyID uuid.UUID, horizon int) (*domain.ForecastSnapshot, error)

	UpsertImportSuggestion(ctx context.Context, s *domain.ExpenseImportSuggestion) error
	ListImportSuggestions(ctx context.Context, propertyID uuid.UUID) ([]domain.ExpenseImportSuggestion, error)

	UpsertMealPrep(ctx context.Context, m *domain.MealPrepActual) error
	GetMealPrep(ctx context.Context, propertyID uuid.UUID, date time.Time) ([]domain.MealPrepActual, error)

	GetTrialBalance(ctx context.Context, propertyID uuid.UUID, to time.Time) ([]domain.TrialBalanceLine, error)
	GetIncomeStatement(ctx context.Context, propertyID uuid.UUID, from, to time.Time) ([]domain.StatementLine, error)
	GetBalanceSheet(ctx context.Context, propertyID uuid.UUID, to time.Time) ([]domain.StatementLine, error)
	GetCashFlow(ctx context.Context, propertyID uuid.UUID, from, to time.Time) ([]domain.CashFlowLine, error)
	GetReconcilingItems(ctx context.Context, propertyID *uuid.UUID, asOf time.Time) ([]domain.ReconcilingItem, error)
}
