package search

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestServiceSearch_hybridFusesResults(t *testing.T) {
	pid := uuid.New()
	svc := &Service{
		Repo: &stubRepo{
			lexical: []Result{{Type: TypeDue, ID: "due-1", Title: "PG-ABCDEF"}},
			vector:  []Result{{Type: TypeDocument, ID: "doc-1", Title: "notes"}},
		},
		Embedder: HashEmbedder{},
	}
	_, mode, rs, err := svc.Search(context.Background(), domain.RoleOwner, pid, nil, "PG-ABCDEF", 10, ModeHybrid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeHybrid {
		t.Fatalf("mode=%s", mode)
	}
	if len(rs) < 2 {
		t.Fatalf("expected fused results, got %d", len(rs))
	}
}
