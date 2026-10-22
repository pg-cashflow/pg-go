package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockPayoutRow struct {
	scanFn func(dest ...any) error
}

func (r *mockPayoutRow) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return nil
}

type mockPayoutRows struct {
	nextFn func() bool
	scanFn func(dest ...any) error
	err    error
}

func (r *mockPayoutRows) Close() {}
func (r *mockPayoutRows) Err() error { return r.err }
func (r *mockPayoutRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *mockPayoutRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *mockPayoutRows) Next() bool {
	if r.nextFn != nil {
		return r.nextFn()
	}
	return false
}
func (r *mockPayoutRows) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return nil
}
func (r *mockPayoutRows) Values() ([]any, error) { return nil, nil }
func (r *mockPayoutRows) RawValues() [][]byte { return nil }
func (r *mockPayoutRows) Conn() *pgx.Conn { return nil }

type mockPayoutDB struct {
	queryRowCalled bool
	queryCalled    bool
	execCalled     bool
	lastSQL        string
	lastArgs       []any
	queryRowFn     func(ctx context.Context, sql string, args ...any) pgx.Row
	queryFn        func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	execFn         func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (m *mockPayoutDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.execCalled = true
	m.lastSQL = sql
	m.lastArgs = args
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (m *mockPayoutDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	m.queryCalled = true
	m.lastSQL = sql
	m.lastArgs = args
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockPayoutRows{}, nil
}

func (m *mockPayoutDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	m.queryRowCalled = true
	m.lastSQL = sql
	m.lastArgs = args
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockPayoutRow{}
}

func TestPayoutRepo_ScopedDBRouting(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()
	batchID := uuid.New()
	itemID := uuid.New()
	payeeID := uuid.New()
	now := time.Now().UTC()

	tests := []struct {
		name      string
		action    func(repo *PayoutRepo, mockDB *mockPayoutDB) error
		wantQuery bool
		wantRow   bool
	}{
		{
			name: "GetBatchByID routes through r.db",
			action: func(repo *PayoutRepo, mockDB *mockPayoutDB) error {
				mockDB.queryRowFn = func(ctx context.Context, sql string, args ...any) pgx.Row {
					return &mockPayoutRow{
						scanFn: func(dest ...any) error {
							*dest[0].(*uuid.UUID) = batchID
							*dest[1].(*uuid.UUID) = propID
							*dest[2].(*string) = "BATCH-001"
							*dest[3].(*string) = "bank_neft"
							*dest[4].(*domain.PayoutBatchStatus) = domain.BatchDraft
							*dest[5].(*int64) = 500000
							*dest[6].(*int) = 1
							*dest[7].(*uuid.UUID) = uuid.New()
							*dest[8].(**uuid.UUID) = nil
							*dest[9].(**time.Time) = nil
							*dest[10].(**string) = nil
							*dest[11].(**string) = nil
							*dest[12].(*time.Time) = now
							*dest[13].(*time.Time) = now
							return nil
						},
					}
				}
				b, err := repo.GetBatchByID(ctx, batchID)
				if err != nil {
					return err
				}
				if b.ID != batchID {
					t.Fatalf("expected batch id %s, got %s", batchID, b.ID)
				}
				return nil
			},
			wantRow: true,
		},
		{
			name: "ListBatchesByProperty routes through r.db",
			action: func(repo *PayoutRepo, mockDB *mockPayoutDB) error {
				batches, err := repo.ListBatchesByProperty(ctx, propID)
				if err != nil {
					return err
				}
				if len(batches) != 0 {
					t.Fatalf("expected 0 batches, got %d", len(batches))
				}
				return nil
			},
			wantQuery: true,
		},
		{
			name: "GetPayoutItemByID routes through r.db",
			action: func(repo *PayoutRepo, mockDB *mockPayoutDB) error {
				mockDB.queryRowFn = func(ctx context.Context, sql string, args ...any) pgx.Row {
					return &mockPayoutRow{
						scanFn: func(dest ...any) error {
							*dest[0].(*uuid.UUID) = itemID
							*dest[1].(**uuid.UUID) = nil
							*dest[2].(*uuid.UUID) = payeeID
							*dest[3].(**uuid.UUID) = nil
							*dest[4].(*string) = "REF-001"
							*dest[5].(*int64) = 500000
							*dest[6].(*string) = "vendor"
							*dest[7].(*string) = "2026_01"
							*dest[8].(*domain.PayoutItemStatus) = domain.PayoutPending
							*dest[9].(**string) = nil
							*dest[10].(**time.Time) = nil
							*dest[11].(**string) = nil
							*dest[12].(**string) = nil
							*dest[13].(**uuid.UUID) = nil
							*dest[14].(*time.Time) = now
							*dest[15].(*time.Time) = now
							return nil
						},
					}
				}
				it, err := repo.GetPayoutItemByID(ctx, itemID)
				if err != nil {
					return err
				}
				if it.ID != itemID {
					t.Fatalf("expected item id %s, got %s", itemID, it.ID)
				}
				return nil
			},
			wantRow: true,
		},
		{
			name: "ListPayoutItemsByBatch routes through r.db",
			action: func(repo *PayoutRepo, mockDB *mockPayoutDB) error {
				items, err := repo.ListPayoutItemsByBatch(ctx, batchID)
				if err != nil {
					return err
				}
				if len(items) != 0 {
					t.Fatalf("expected 0 items, got %d", len(items))
				}
				return nil
			},
			wantQuery: true,
		},
		{
			name: "GetPayeeByID routes through r.db",
			action: func(repo *PayoutRepo, mockDB *mockPayoutDB) error {
				mockDB.queryRowFn = func(ctx context.Context, sql string, args ...any) pgx.Row {
					return &mockPayoutRow{
						scanFn: func(dest ...any) error {
							*dest[0].(*uuid.UUID) = payeeID
							*dest[1].(*uuid.UUID) = propID
							*dest[2].(*domain.PayeeType) = domain.PayeeTypeVendor
							*dest[3].(*string) = "Acme Supplies"
							*dest[4].(**string) = nil
							*dest[5].(*[]byte) = []byte("enc")
							last4 := "1234"
							*dest[6].(**string) = &last4
							*dest[7].(*string) = "hash"
							*dest[8].(**string) = nil
							*dest[9].(**string) = nil
							*dest[10].(**string) = nil
							*dest[11].(*int) = 1
							*dest[12].(*bool) = true
							*dest[13].(**uuid.UUID) = nil
							*dest[14].(**time.Time) = nil
							*dest[15].(*time.Time) = now
							*dest[16].(*time.Time) = now
							return nil
						},
					}
				}
				p, err := repo.GetPayeeByID(ctx, payeeID)
				if err != nil {
					return err
				}
				if p.ID != payeeID {
					t.Fatalf("expected payee id %s, got %s", payeeID, p.ID)
				}
				return nil
			},
			wantRow: true,
		},
		{
			name: "ListPayeesByProperty routes through r.db",
			action: func(repo *PayoutRepo, mockDB *mockPayoutDB) error {
				payees, err := repo.ListPayeesByProperty(ctx, propID)
				if err != nil {
					return err
				}
				if len(payees) != 0 {
					t.Fatalf("expected 0 payees, got %d", len(payees))
				}
				return nil
			},
			wantQuery: true,
		},
		{
			name: "ApproveBatch routes through r.db",
			action: func(repo *PayoutRepo, mockDB *mockPayoutDB) error {
				approverID := uuid.New()
				mockDB.queryRowFn = func(ctx context.Context, sql string, args ...any) pgx.Row {
					return &mockPayoutRow{
						scanFn: func(dest ...any) error {
							*dest[0].(*uuid.UUID) = batchID
							*dest[1].(*uuid.UUID) = propID
							*dest[2].(*string) = "BATCH-001"
							*dest[3].(*string) = "bank_neft"
							*dest[4].(*domain.PayoutBatchStatus) = domain.BatchApproved
							*dest[5].(*int64) = 500000
							*dest[6].(*int) = 1
							*dest[7].(*uuid.UUID) = uuid.New()
							*dest[8].(**uuid.UUID) = &approverID
							*dest[9].(**time.Time) = &now
							*dest[10].(**string) = nil
							*dest[11].(**string) = nil
							*dest[12].(*time.Time) = now
							*dest[13].(*time.Time) = now
							return nil
						},
					}
				}
				b, err := repo.ApproveBatch(ctx, batchID, approverID)
				if err != nil {
					return err
				}
				if b.Status != domain.BatchApproved {
					t.Fatalf("expected status approved, got %s", b.Status)
				}
				return nil
			},
			wantRow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := &mockPayoutDB{}
			repo := NewPayoutRepo(nil, mockDB)

			if err := tt.action(repo, mockDB); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantRow && !mockDB.queryRowCalled {
				t.Errorf("expected QueryRow to be called on mockDB, but was not")
			}
			if tt.wantQuery && !mockDB.queryCalled {
				t.Errorf("expected Query to be called on mockDB, but was not")
			}
		})
	}
}

func TestPayoutRepo_ConstructorCompatibility(t *testing.T) {
	mockDB := &mockPayoutDB{}
	var pool *pgxpool.Pool

	// Case 1: Pool only
	r1 := NewPayoutRepo(pool)
	if r1 == nil {
		t.Fatal("expected non-nil repo")
	}

	// Case 2: Pool + DBTX
	r2 := NewPayoutRepo(pool, mockDB)
	if r2.db != mockDB {
		t.Fatal("expected r.db to equal mockDB")
	}

	// Case 3: WithScopedDB
	mockDB2 := &mockPayoutDB{}
	r3 := r2.WithScopedDB(mockDB2)
	if r3.db != mockDB2 {
		t.Fatal("expected r3.db to equal mockDB2")
	}
}
