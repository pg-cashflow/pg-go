package requestscope

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestWithPropertyID_RoundTrip(t *testing.T) {
	expected := uuid.New()
	ctx := WithPropertyID(context.Background(), expected)

	got, ok := PropertyIDFromContext(ctx)
	if !ok {
		t.Fatal("expected ok=true when propertyID is present")
	}
	if got != expected {
		t.Fatalf("expected propertyID %v, got %v", expected, got)
	}
}

func TestPropertyIDFromContext_Absent(t *testing.T) {
	ctx := context.Background()
	got, ok := PropertyIDFromContext(ctx)
	if ok {
		t.Fatal("expected ok=false on empty context")
	}
	if got != uuid.Nil {
		t.Fatalf("expected uuid.Nil, got %v", got)
	}
}

func TestWithPropertyID_NilUUIDRejected(t *testing.T) {
	ctx := WithPropertyID(context.Background(), uuid.Nil)
	got, ok := PropertyIDFromContext(ctx)
	if ok {
		t.Fatal("expected ok=false when attached propertyID is uuid.Nil")
	}
	if got != uuid.Nil {
		t.Fatalf("expected uuid.Nil, got %v", got)
	}
}

func TestWithPropertyID_ParentContextNotMutated(t *testing.T) {
	parent := context.Background()
	child := WithPropertyID(parent, uuid.New())

	if _, ok := PropertyIDFromContext(parent); ok {
		t.Fatal("parent context was mutated when child was created")
	}
	if _, ok := PropertyIDFromContext(child); !ok {
		t.Fatal("child context missing propertyID")
	}
}
