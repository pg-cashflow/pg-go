package postgres

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

type mockCapturingDB struct {
	lastQuery string
	lastArgs  []any
}

func (m *mockCapturingDB) Exec(_ context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	m.lastQuery = sql
	m.lastArgs = arguments
	return pgconn.NewCommandTag(""), nil
}

func (m *mockCapturingDB) Query(_ context.Context, sql string, arguments ...any) (pgx.Rows, error) {
	m.lastQuery = sql
	m.lastArgs = arguments
	return &emptyRows{}, nil
}

func (m *mockCapturingDB) QueryRow(_ context.Context, sql string, arguments ...any) pgx.Row {
	m.lastQuery = sql
	m.lastArgs = arguments
	return nil
}

type emptyRows struct{}

func (e *emptyRows) Close()                                       {}
func (e *emptyRows) Err() error                                   { return nil }
func (e *emptyRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("") }
func (e *emptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (e *emptyRows) Next() bool                                   { return false }
func (e *emptyRows) Scan(_ ...any) error                          { return nil }
func (e *emptyRows) Values() ([]any, error)                       { return nil, nil }
func (e *emptyRows) RawValues() [][]byte                          { return nil }
func (e *emptyRows) Conn() *pgx.Conn                              { return nil }

func TestSearchRepo_QueryScopingUnit(t *testing.T) {
	propID := uuid.New()
	tenantID := uuid.New()

	t.Run("Tenant_FailClosed_NilTenantID", func(t *testing.T) {
		mock := &mockCapturingDB{}
		repo := NewSearchRepo(mock)

		p := search.Params{
			Query:      "test",
			PropertyID: propID,
			Role:       string(domain.RoleTenant),
			TenantID:   nil, // nil tenant ID!
		}
		// Lexical
		res, err := repo.searchDocumentsLexical(context.Background(), p, "%test%", 10)
		if err != nil || res != nil {
			t.Fatalf("expected nil, nil on fail-closed tenant search, got res=%v, err=%v", res, err)
		}
		if mock.lastQuery != "" {
			t.Fatalf("expected no query executed on fail-closed tenant search, executed: %s", mock.lastQuery)
		}

		// Vector
		vecRes, err := repo.SearchVector(context.Background(), p, 10, make([]float32, 384))
		if err != nil || vecRes != nil {
			t.Fatalf("expected nil, nil on fail-closed vector search, got res=%v, err=%v", vecRes, err)
		}
	})

	t.Run("Tenant_StrictScoping_WithTenantID", func(t *testing.T) {
		mock := &mockCapturingDB{}
		repo := NewSearchRepo(mock)

		p := search.Params{
			Query:      "hazard",
			PropertyID: propID,
			Role:       string(domain.RoleTenant),
			TenantID:   &tenantID,
		}
		_, _ = repo.searchDocumentsLexical(context.Background(), p, "%hazard%", 10)

		if !strings.Contains(mock.lastQuery, "entity_type = ANY($4)") {
			t.Errorf("query missing entity_type = ANY($4): %s", mock.lastQuery)
		}
		if !strings.Contains(mock.lastQuery, "AND tenant_id = $5") {
			t.Errorf("query missing strict AND tenant_id = $5: %s", mock.lastQuery)
		}
		if strings.Contains(mock.lastQuery, "tenant_id IS NULL") {
			t.Errorf("query must NEVER contain 'tenant_id IS NULL': %s", mock.lastQuery)
		}

		// Check args
		expectedTypes := []string{"hazard", "violation"}
		if !reflect.DeepEqual(mock.lastArgs[3], expectedTypes) {
			t.Errorf("expected allowed types %v, got %v", expectedTypes, mock.lastArgs[3])
		}
		if mock.lastArgs[4] != tenantID {
			t.Errorf("expected tenantID %v, got %v", tenantID, mock.lastArgs[4])
		}
	})

	t.Run("Manager_NoPaymentNotes", func(t *testing.T) {
		mock := &mockCapturingDB{}
		repo := NewSearchRepo(mock)

		p := search.Params{
			Query:      "note",
			PropertyID: propID,
			Role:       string(domain.RoleManager),
			TenantID:   nil,
		}
		_, _ = repo.searchDocumentsLexical(context.Background(), p, "%note%", 10)

		if !strings.Contains(mock.lastQuery, "entity_type = ANY($4)") {
			t.Errorf("query missing entity_type = ANY($4): %s", mock.lastQuery)
		}
		expectedTypes := []string{"inspection", "hazard", "violation"}
		if !reflect.DeepEqual(mock.lastArgs[3], expectedTypes) {
			t.Errorf("expected manager allowed types %v, got %v", expectedTypes, mock.lastArgs[3])
		}
		for _, arg := range mock.lastArgs[3].([]string) {
			if arg == "payment_note" {
				t.Errorf("manager query MUST NOT contain payment_note in entity_type filter")
			}
		}
	})
}
