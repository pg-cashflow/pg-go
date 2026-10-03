package search

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestServiceSearch_hybridGracefullyDefaultsToLexical(t *testing.T) {
	pid := uuid.New()
	svc := &Service{
		Repo: &stubRepo{
			lexical: []Result{{Type: TypeDue, ID: "due-1", Title: "PG-ABCDEF"}},
		},
	}
	_, mode, rs, err := svc.Search(context.Background(), domain.RoleOwner, pid, nil, "PG-ABCDEF", 10, ModeHybrid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeLexical {
		t.Fatalf("expected mode=lexical per ADR-012, got %s", mode)
	}
	if len(rs) != 1 {
		t.Fatalf("expected lexical result, got %d", len(rs))
	}
}
