package referral

import (
	"testing"
)

func TestBpsAmount(t *testing.T) {
	if bpsAmount(100000, 250) != 2500 {
		t.Fatalf("expected 2500 paise, got %d", bpsAmount(100000, 250))
	}
	if bpsAmount(0, 100) != 0 {
		t.Fatal("zero rent")
	}
}
