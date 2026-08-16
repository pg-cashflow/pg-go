package magiclink

import (
	"strings"
	"testing"
)

func TestBuildWALink_NilPhone(t *testing.T) {
	if got := BuildWALink(nil, "hello"); got != "" {
		t.Fatalf("got %q want empty", got)
	}
}

func TestBuildWALink_EmptyPhone(t *testing.T) {
	empty := ""
	if got := BuildWALink(&empty, "hello"); got != "" {
		t.Fatalf("got %q want empty", got)
	}
}

func TestBuildWALink_Format(t *testing.T) {
	phone := "+91 98765-43210"
	got := BuildWALink(&phone, "Pay rent: https://pay.example.com/p/abc")
	wantPrefix := "https://wa.me/919876543210?text="
	if len(got) < len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("got %q want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, "Pay") {
		t.Fatalf("missing message encoding: %s", got)
	}
}
