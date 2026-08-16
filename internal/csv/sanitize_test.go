package csv

import (
	"strings"
	"testing"
)

func TestStripFormulaChars(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"=1+1", "1+1"},
		{"+cmd", "cmd"},
		{"-2", "2"},
		{"@SUM(A1)", "SUM(A1)"},
		{"  =HYPERLINK()", "HYPERLINK()"},
		{"=+@-safe", "safe"},
		{"normal", "normal"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := StripFormulaChars(tc.in); got != tc.want {
			t.Errorf("StripFormulaChars(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseKnownSchema(t *testing.T) {
	in := "Txn ID,Amount,Date,Narration\n" +
		"UTR123,1500.50,2026-08-01,PG-A3X9KR rent\n" +
		"=CMD,200,02-08-2026,=note\n"
	rows, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("len=%d", len(rows))
	}
	if rows[0].TxnID != "UTR123" || rows[0].AmountPaise != 150050 {
		t.Fatalf("row0=%+v", rows[0])
	}
	if rows[1].TxnID != "CMD" || rows[1].Note != "note" {
		t.Fatalf("row1 sanitized=%+v", rows[1])
	}
}

func TestParseUnknownSchema(t *testing.T) {
	_, err := Parse(strings.NewReader("foo,bar\n1,2\n"))
	if err != ErrUnknownSchema {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}
