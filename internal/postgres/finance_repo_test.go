package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockFinanceRow struct {
	scanFn func(dest ...any) error
}

func (r *mockFinanceRow) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return nil
}

type mockFinanceRows struct {
	scanFn func(dest ...any) error
	count  int
	curr   int
	err    error
}

func (r *mockFinanceRows) Close() {}

func (r *mockFinanceRows) Err() error {
	return r.err
}

func (r *mockFinanceRows) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}

func (r *mockFinanceRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

func (r *mockFinanceRows) Next() bool {
	if r.curr < r.count {
		r.curr++
		return true
	}
	return false
}

func (r *mockFinanceRows) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return nil
}

func (r *mockFinanceRows) Values() ([]any, error) {
	return nil, nil
}

func (r *mockFinanceRows) RawValues() [][]byte {
	return nil
}

func (r *mockFinanceRows) Conn() *pgx.Conn {
	return nil
}

type mockFinanceDBTX struct {
	lastSQL  string
	lastArgs []any
	execFn   func(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	queryFn  func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	rowFn    func(ctx context.Context, sql string, args ...any) pgx.Row
}

func (m *mockFinanceDBTX) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	m.lastSQL = sql
	m.lastArgs = arguments
	if m.execFn != nil {
		return m.execFn(ctx, sql, arguments...)
	}
	return pgconn.CommandTag{}, nil
}

func (m *mockFinanceDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	m.lastSQL = sql
	m.lastArgs = args
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockFinanceRows{}, nil
}

func (m *mockFinanceDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	m.lastSQL = sql
	m.lastArgs = args
	if m.rowFn != nil {
		return m.rowFn(ctx, sql, args...)
	}
	return &mockFinanceRow{}
}

func TestFinanceRepo_ScopedDBWiring(t *testing.T) {
	propID := uuid.New()
	ctx := context.Background()

	t.Run("ListJournal executes via scopedDB", func(t *testing.T) {
		mockDB := &mockFinanceDBTX{
			queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
				if !strings.Contains(sql, "FROM financial_journal_entries") {
					t.Fatalf("expected query on financial_journal_entries, got %s", sql)
				}
				if len(args) == 0 || args[0] != propID {
					t.Fatalf("expected propertyID arg, got %v", args)
				}
				lineID := uuid.New()
				now := time.Now().UTC()
				return &mockFinanceRows{
					count: 1,
					scanFn: func(dest ...any) error {
						*dest[0].(*uuid.UUID) = lineID
						*dest[1].(*uuid.UUID) = propID
						*dest[2].(*string) = "1010"
						*dest[3].(*int64) = 500000
						*dest[4].(*int64) = 0
						*dest[5].(*string) = "payment"
						*dest[6].(*uuid.UUID) = lineID
						*dest[7].(*string) = "debit"
						*dest[8].(*time.Time) = now
						return nil
					},
				}, nil
			},
		}

		repo := NewFinanceRepo(nil, mockDB)
		lines, err := repo.ListJournal(ctx, propID, time.Time{}, time.Time{}, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(lines) != 1 {
			t.Fatalf("expected 1 journal line, got %d", len(lines))
		}
		if lines[0].DebitPaise != 500000 {
			t.Fatalf("expected 500000 debit, got %d", lines[0].DebitPaise)
		}
	})

	t.Run("SumAccount executes via scopedDB", func(t *testing.T) {
		mockDB := &mockFinanceDBTX{
			rowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				if !strings.Contains(sql, "SUM(debit_paise)") {
					t.Fatalf("expected query with SUM(debit_paise), got %s", sql)
				}
				return &mockFinanceRow{
					scanFn: func(dest ...any) error {
						*dest[0].(*int64) = 150000
						*dest[1].(*int64) = 300000
						return nil
					},
				}
			},
		}

		repo := NewFinanceRepo(nil, mockDB)
		dr, cr, err := repo.SumAccount(ctx, propID, "1010", time.Now().Add(-24*time.Hour), time.Now())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dr != 150000 || cr != 300000 {
			t.Fatalf("expected dr=150000, cr=300000, got dr=%d, cr=%d", dr, cr)
		}
	})

	t.Run("GetBudget executes via scopedDB", func(t *testing.T) {
		mockDB := &mockFinanceDBTX{
			rowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				if !strings.Contains(sql, "FROM budgets") {
					t.Fatalf("expected query on budgets, got %s", sql)
				}
				bID := uuid.New()
				return &mockFinanceRow{
					scanFn: func(dest ...any) error {
						*dest[0].(*uuid.UUID) = bID
						*dest[1].(*uuid.UUID) = propID
						*dest[2].(*string) = "MAINT"
						*dest[3].(*string) = "2026-10"
						*dest[4].(*int64) = 2500000
						return nil
					},
				}
			},
		}

		repo := NewFinanceRepo(nil, mockDB)
		b, err := repo.GetBudget(ctx, propID, "MAINT", "2026-10")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if b.AmountPaise != 2500000 {
			t.Fatalf("expected 2500000 amount, got %d", b.AmountPaise)
		}
	})

	t.Run("GetExpense executes via scopedDB", func(t *testing.T) {
		expID := uuid.New()
		mockDB := &mockFinanceDBTX{
			rowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				if !strings.Contains(sql, "FROM expenses") {
					t.Fatalf("expected query on expenses, got %s", sql)
				}
				now := time.Now().UTC()
				vendor := "Plumbing Supplies"
				desc := "Pipe fixes"
				return &mockFinanceRow{
					scanFn: func(dest ...any) error {
						*dest[0].(*uuid.UUID) = expID
						*dest[1].(*uuid.UUID) = propID
						*dest[2].(*string) = "MAINT"
						*dest[3].(**string) = &vendor
						*dest[4].(**string) = &desc
						*dest[5].(*int64) = 450000
						*dest[6].(*domain.ExpenseStatus) = domain.ExpenseApproved
						*dest[7].(*bool) = false
						*dest[8].(**uuid.UUID) = nil
						*dest[9].(*uuid.UUID) = uuid.New()
						*dest[10].(*string) = "manager"
						*dest[11].(*time.Time) = now
						*dest[12].(*time.Time) = now
						return nil
					},
				}
			},
		}

		repo := NewFinanceRepo(nil, mockDB)
		e, err := repo.GetExpense(ctx, expID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if e.AmountPaise != 450000 {
			t.Fatalf("expected amount 450000, got %d", e.AmountPaise)
		}
	})

	t.Run("GetTrialBalance executes via scopedDB", func(t *testing.T) {
		mockDB := &mockFinanceDBTX{
			queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
				if !strings.Contains(sql, "ledger_trial_balance") {
					t.Fatalf("expected call to ledger_trial_balance, got %s", sql)
				}
				return &mockFinanceRows{
					count: 1,
					scanFn: func(dest ...any) error {
						*dest[0].(*string) = "1010"
						*dest[1].(*string) = "ASSET"
						*dest[2].(*int64) = 100000
						*dest[3].(*int64) = 0
						*dest[4].(*int64) = 100000
						return nil
					},
				}, nil
			},
		}

		repo := NewFinanceRepo(nil, mockDB)
		tb, err := repo.GetTrialBalance(ctx, propID, time.Now())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(tb) != 1 || tb[0].AccountCode != "1010" {
			t.Fatalf("expected account 1010, got %v", tb)
		}
	})

	t.Run("WithScopedDB returns updated instance", func(t *testing.T) {
		mock1 := &mockFinanceDBTX{}
		mock2 := &mockFinanceDBTX{}
		repo1 := NewFinanceRepo(nil, mock1)
		repo2 := repo1.WithScopedDB(mock2)
		if repo2.db != mock2 {
			t.Fatalf("expected repo2 to have mock2, got %v", repo2.db)
		}
	})
}
