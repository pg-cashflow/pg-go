package search

import "testing"

func TestFuseRRF_prefersItemsInBothLists(t *testing.T) {
	a := []Result{{Type: TypeDue, ID: "1", Title: "a"}}
	b := []Result{{Type: TypeDue, ID: "1", Title: "a"}, {Type: TypeDue, ID: "2", Title: "b"}}
	out := FuseRRF([][]Result{a, b})
	if len(out) != 2 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].ID != "1" {
		t.Fatalf("expected id 1 on top, got %s", out[0].ID)
	}
}
