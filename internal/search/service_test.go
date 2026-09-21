package search

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubRepo struct {
	lexical []Result
	vector  []Result
}

func (s *stubRepo) SearchLexical(_ context.Context, _ Params, _ []EntityType, _ int) ([]Result, error) {
	return s.lexical, nil
}

func (s *stubRepo) SearchVector(_ context.Context, _ Params, _ int, _ []float32) ([]Result, error) {
	return s.vector, nil
}

func TestServiceSearch_ownerGetsLexical(t *testing.T) {
	pid := uuid.New()
	svc := &Service{
		Repo: &stubRepo{lexical: []Result{{Type: TypeTenant, ID: "t1", Title: "Ravi"}}},
	}
	_, mode, rs, err := svc.Search(context.Background(), domain.RoleOwner, pid, nil, "ravi", 10, ModeLexical, nil)
	if err != nil || mode != ModeLexical || len(rs) != 1 {
		t.Fatalf("err=%v mode=%s len=%d", err, mode, len(rs))
	}
}

func TestServiceSearch_tenantScopePassed(t *testing.T) {
	pid := uuid.New()
	tid := uuid.New()
	var captured *uuid.UUID
	svc := &Service{
		Repo: &captureRepo{tenantID: &captured},
	}
	_, _, _, err := svc.Search(context.Background(), domain.RoleTenant, pid, &tid, "ab", 10, ModeLexical, nil)
	if err != nil {
		t.Fatal(err)
	}
	if captured == nil || *captured != tid {
		t.Fatalf("tenant scope not passed")
	}
}

type captureRepo struct {
	tenantID **uuid.UUID
}

func (c *captureRepo) SearchLexical(_ context.Context, p Params, _ []EntityType, _ int) ([]Result, error) {
	*c.tenantID = p.TenantID
	return nil, nil
}

func (c *captureRepo) SearchVector(context.Context, Params, int, []float32) ([]Result, error) {
	return nil, nil
}
