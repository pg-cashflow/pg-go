package search

import (
	"testing"
)

func TestTokenizeQuery(t *testing.T) {
	tokens := TokenizeQuery("rahul 201 +919876543210 DUE-JAN-01")
	if len(tokens) != 4 {
		t.Fatalf("expected 4 tokens, got %d", len(tokens))
	}

	if tokens[0].Kind != TokenText || tokens[0].Value != "rahul" {
		t.Errorf("token 0 expected Text rahul, got %+v", tokens[0])
	}
	if tokens[1].Kind != TokenShortNumber || tokens[1].Value != "201" {
		t.Errorf("token 1 expected ShortNumber 201, got %+v", tokens[1])
	}
	if tokens[2].Kind != TokenPhone || tokens[2].Value != "9876543210" {
		t.Errorf("token 2 expected Phone 9876543210, got %+v", tokens[2])
	}
	if tokens[3].Kind != TokenCode || tokens[3].Value != "DUE-JAN-01" {
		t.Errorf("token 3 expected Code DUE-JAN-01, got %+v", tokens[3])
	}
}
