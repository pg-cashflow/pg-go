package search

import "testing"

func mk(t EntityType, ids ...string) []Result {
	var rs []Result
	for _, id := range ids {
		rs = append(rs, Result{Type: t, ID: id})
	}
	return rs
}

func TestMergeResults_noTypeStarvation(t *testing.T) {
	var in []Result
	in = append(in, mk(TypeTenant, "t1", "t2", "t3", "t4", "t5")...)
	in = append(in, mk(TypeDue, "d1", "d2", "d3", "d4", "d5")...)
	in = append(in, mk(TypePayment, "p1", "p2", "p3", "p4", "p5")...)
	in = append(in, mk(TypePaymentReport, "r1", "r2", "r3", "r4", "r5")...)
	in = append(in, mk(TypeSettlement, "s1", "s2")...)
	in = append(in, mk(TypePayout, "o1")...)

	out := MergeResults(in, TokenizeQuery("abc"), 8)
	if len(out) != 8 {
		t.Fatalf("len=%d", len(out))
	}
	got := map[EntityType]bool{}
	for _, r := range out {
		got[r.Type] = true
	}
	for _, et := range []EntityType{TypeTenant, TypeDue, TypePayment, TypePaymentReport, TypeSettlement, TypePayout} {
		if !got[et] {
			t.Fatalf("type %s starved: %+v", et, out)
		}
	}
}

func TestMergeResults_codeIntentPutsFinanceFirst(t *testing.T) {
	var in []Result
	in = append(in, mk(TypeTenant, "t1")...)
	in = append(in, mk(TypeSettlement, "s1")...)
	out := MergeResults(in, TokenizeQuery("UTR12345"), 5)
	if out[0].Type != TypeSettlement {
		t.Fatalf("want settlement first, got %s", out[0].Type)
	}
	out = MergeResults(in, TokenizeQuery("rahul"), 5)
	if out[0].Type != TypeTenant {
		t.Fatalf("want tenant first, got %s", out[0].Type)
	}
}

func TestMergeResults_preservesInTypeOrderAndLimit(t *testing.T) {
	in := mk(TypeTenant, "a", "b", "c")
	out := MergeResults(in, TokenizeQuery("zz"), 2)
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "b" {
		t.Fatalf("%+v", out)
	}
	if MergeResults(nil, nil, 5) != nil || MergeResults(in, nil, 0) != nil {
		t.Fatal("empty cases")
	}
}
