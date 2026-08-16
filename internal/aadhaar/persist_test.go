package aadhaar

import "testing"

func TestPersistableLast4(t *testing.T) {
	if got := PersistableLast4(true, false, "9012", "1111"); got != "" {
		t.Fatalf("unverified QR must not persist, got %q", got)
	}
	if got := PersistableLast4(true, true, "9012", "1111"); got != "9012" {
		t.Fatalf("verified QR last4=%q", got)
	}
	if got := PersistableLast4(false, false, "", "5678"); got != "5678" {
		t.Fatalf("manual last4=%q", got)
	}
}

func TestGuardOverwrite(t *testing.T) {
	existing := "9012"
	if err := GuardOverwrite(&existing, "9012"); err != nil {
		t.Fatal(err)
	}
	if err := GuardOverwrite(&existing, "1111"); err != ErrLast4AlreadySet {
		t.Fatalf("got %v", err)
	}
	if err := GuardOverwrite(nil, "1111"); err != nil {
		t.Fatal(err)
	}
}
