package qr

import (
	"strings"
	"testing"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestGenerateDueCode_FormatLength(t *testing.T) {
	code, err := GenerateDueCode(func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 6 {
		t.Fatalf("len=%d want 6, code=%q", len(code), code)
	}
	for _, c := range code {
		ok := (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')
		if !ok {
			t.Fatalf("non-base36 char %q in %q", c, code)
		}
	}
}

func TestGenerateDueCode_RetriesOnConflict(t *testing.T) {
	n := 0
	code, err := GenerateDueCode(func(string) error {
		n++
		if n < 3 {
			return ErrConflict
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if code == "" {
		t.Fatal("empty code")
	}
	if n != 3 {
		t.Fatalf("tries=%d want 3", n)
	}
}

func TestUPINote_MaxLength(t *testing.T) {
	code, err := domain.GenerateDueCode()
	if err != nil {
		t.Fatal(err)
	}
	note := domain.UPINote(code)
	if len(note) > 30 {
		t.Fatalf("UPINote %q len=%d exceeds 30", note, len(note))
	}
	if note != "PG-"+code {
		t.Fatalf("note=%q", note)
	}
}

func TestGenerateUPILink_AmountAndNote(t *testing.T) {
	link := GenerateUPILink("owner@upi", "Owner", 150050, "A3X9KR", "12")
	if !strings.HasPrefix(link, "upi://pay?") {
		t.Fatalf("scheme: %s", link)
	}
	for _, part := range []string{"pa=owner%40upi", "am=1500.50", "tn=PG-A3X9KR", "cu=INR"} {
		if !strings.Contains(link, part) {
			t.Fatalf("missing %q in %s", part, link)
		}
	}
}
