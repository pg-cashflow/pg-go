package kyc

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// TestConsentPurposeAllowlistMatchesMigration fails when the Go allowlist and the
// kyc_consent_purpose_check constraint in migration 110 differ. A mismatch means a
// valid purpose is rejected by one layer, or an invalid purpose reaches the database.
func TestConsentPurposeAllowlistMatchesMigration(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "110_kyc_consent_purpose_constraint.sql"))
	if err != nil {
		t.Fatalf("read migration 110: %v", err)
	}
	var fromSQL []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(string(raw), -1) {
		fromSQL = append(fromSQL, m[1])
	}
	var fromGo []string
	for k := range AllowedConsentPurposes {
		fromGo = append(fromGo, k)
	}
	sort.Strings(fromSQL)
	sort.Strings(fromGo)
	if len(fromSQL) != len(fromGo) {
		t.Fatalf("allowlist drift: sql=%v go=%v", fromSQL, fromGo)
	}
	for i := range fromSQL {
		if fromSQL[i] != fromGo[i] {
			t.Fatalf("allowlist drift: sql=%v go=%v", fromSQL, fromGo)
		}
	}
}

func TestValidateConsentPurposeRejectsUnknown(t *testing.T) {
	for p := range AllowedConsentPurposes {
		if err := ValidateConsentPurpose(p); err != nil {
			t.Errorf("allowed purpose %q rejected: %v", p, err)
		}
	}
	for _, p := range []string{"", "marketing", "ONBOARDING", "onboarding "} {
		if err := ValidateConsentPurpose(p); !errors.Is(err, ErrUnsupportedConsentPurpose) {
			t.Errorf("purpose %q: want ErrUnsupportedConsentPurpose, got %v", p, err)
		}
	}
}
