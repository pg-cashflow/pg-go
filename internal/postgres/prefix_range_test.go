package postgres

import (
	"strings"
	"testing"
)

func TestPrefixRange(t *testing.T) {
	cases := []struct {
		in, lo, hi string
		ok         bool
	}{
		{"TXN-79-50", "txn-79-50", "txn-79-51", true},
		{"  UTR9z ", "utr9z", "utr9{", true},
		{"abc~", "abc~", "abc\x7f", true},
		{"ab", "", "", false},     // too short
		{"ÉCL-1", "", "", false},  // non-ASCII -> trigram path
		{"ab\x7f", "", "", false}, // DEL not printable
		{"ab\x00", "", "", false}, // control byte
		{"", "", "", false},
	}
	for _, c := range cases {
		lo, hi, ok := prefixRange(c.in)
		if ok != c.ok || lo != c.lo || hi != c.hi {
			t.Errorf("prefixRange(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, lo, hi, ok, c.lo, c.hi, c.ok)
		}
	}
}

// Every string that has lo as a prefix must fall in [lo, hi) under byte order,
// and strings that don't must fall outside it. This is the property the btree
// range scan depends on.
func TestPrefixRangeBracketsExactlyThePrefix(t *testing.T) {
	lo, hi, ok := prefixRange("txn-79-5")
	if !ok {
		t.Fatal("expected ok")
	}
	in := []string{"txn-79-5", "txn-79-50", "txn-79-5~", "txn-79-5zzzz"}
	out := []string{"txn-79-4", "txn-79-6", "txn-79", "txn-79-", "txn-79-4zzz", "txn-79-6a"}
	for _, s := range in {
		if !(s >= lo && s < hi) || !strings.HasPrefix(s, lo) {
			t.Errorf("%q should be inside [%q,%q)", s, lo, hi)
		}
	}
	for _, s := range out {
		if s >= lo && s < hi {
			t.Errorf("%q should be outside [%q,%q)", s, lo, hi)
		}
	}
}
