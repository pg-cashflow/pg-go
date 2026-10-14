package cmd_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exemptMarker = "//m12:validate-exempt"

// checkCmdDirectory validates that all binaries in cmdDir (except server)
// either call ValidateForRealDeployment or declare //m12:validate-exempt <reason>.
func checkCmdDirectory(cmdDir string) ([]string, error) {
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		return nil, fmt.Errorf("read cmd directory %s: %w", cmdDir, err)
	}

	var violations []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		binName := entry.Name()
		if binName == "server" {
			continue
		}

		mainFile := filepath.Join(cmdDir, binName, "main.go")
		contentBytes, err := os.ReadFile(mainFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", mainFile, err)
		}
		content := string(contentBytes)

		hasValidate := strings.Contains(content, "ValidateForRealDeployment")
		hasExempt := strings.Contains(content, exemptMarker)

		if !hasValidate && !hasExempt {
			violations = append(violations, fmt.Sprintf("%s: missing ValidateForRealDeployment and %s annotation", binName, exemptMarker))
			continue
		}

		if hasExempt {
			// Require a non-empty reason after the marker
			idx := strings.Index(content, exemptMarker)
			remainder := content[idx+len(exemptMarker):]
			eol := strings.Index(remainder, "\n")
			line := remainder
			if eol != -1 {
				line = remainder[:eol]
			}
			reason := strings.TrimSpace(line)
			if reason == "" {
				violations = append(violations, fmt.Sprintf("%s: %s marker requires a documented reason", binName, exemptMarker))
			}
		}
	}

	return violations, nil
}

func TestCommandValidationCoverage(t *testing.T) {
	violations, err := checkCmdDirectory(".")
	if err != nil {
		t.Fatalf("checkCmdDirectory failed: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("command validation coverage violations found:\n  - %s", strings.Join(violations, "\n  - "))
	}
}

func TestCommandValidationCoverage_NegativeFixture(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Unexempted binary without validation must fail
	badDir := filepath.Join(tmpDir, "bad-binary")
	if err := os.MkdirAll(badDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	badMain := filepath.Join(badDir, "main.go")
	badCode := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(badMain, []byte(badCode), 0644); err != nil {
		t.Fatalf("write bad main failed: %v", err)
	}

	violations, err := checkCmdDirectory(tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) == 0 {
		t.Fatalf("expected violation for unexempted binary without validation, got none")
	}
	if !strings.Contains(violations[0], "bad-binary") {
		t.Fatalf("expected violation mentioning bad-binary, got: %s", violations[0])
	}

	// 2. Binary with exempt marker but no reason must fail
	noReasonDir := filepath.Join(tmpDir, "no-reason-binary")
	if err := os.MkdirAll(noReasonDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	noReasonMain := filepath.Join(noReasonDir, "main.go")
	noReasonCode := "package main\n\n//m12:validate-exempt\n\nfunc main() {}\n"
	if err := os.WriteFile(noReasonMain, []byte(noReasonCode), 0644); err != nil {
		t.Fatalf("write no-reason main failed: %v", err)
	}

	violations, err = checkCmdDirectory(tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundNoReason := false
	for _, v := range violations {
		if strings.Contains(v, "no-reason-binary") && strings.Contains(v, "documented reason") {
			foundNoReason = true
			break
		}
	}
	if !foundNoReason {
		t.Fatalf("expected violation for marker without reason, got: %v", violations)
	}

	// 3. Binary with valid exempt marker and reason must pass
	_ = os.RemoveAll(badDir)
	_ = os.RemoveAll(noReasonDir)
	validExemptDir := filepath.Join(tmpDir, "valid-exempt")
	if err := os.MkdirAll(validExemptDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	validExemptMain := filepath.Join(validExemptDir, "main.go")
	validExemptCode := "package main\n\n//m12:validate-exempt maintenance tool with custom guards\n\nfunc main() {}\n"
	if err := os.WriteFile(validExemptMain, []byte(validExemptCode), 0644); err != nil {
		t.Fatalf("write valid exempt main failed: %v", err)
	}

	violations, err = checkCmdDirectory(tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected 0 violations for valid exempt binary, got: %v", violations)
	}
}
