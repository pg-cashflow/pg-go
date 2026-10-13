package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSearchReindex_DeprecatedNoAction(t *testing.T) {
	if os.Getenv("BE_CMD_SEARCH_REINDEX_TEST") == "1" {
		if arg := os.Getenv("CMD_ARG"); arg != "" {
			os.Args = []string{os.Args[0], arg}
		}
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestSearchReindex_DeprecatedNoAction")
	cmd.Env = []string{
		"BE_CMD_SEARCH_REINDEX_TEST=1",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
		"TEMP=" + os.Getenv("TEMP"),
		"TMP=" + os.Getenv("TMP"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected search-reindex to exit 0 (deprecated), got err: %v, out: %s", err, string(out))
	}
	if !strings.Contains(string(out), "[DEPRECATED]") {
		t.Errorf("expected deprecation message, got: %s", string(out))
	}
}

func TestSearchReindex_HelpFlag(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestSearchReindex_DeprecatedNoAction")
	cmd.Env = []string{
		"BE_CMD_SEARCH_REINDEX_TEST=1",
		"CMD_ARG=-help",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
		"TEMP=" + os.Getenv("TEMP"),
		"TMP=" + os.Getenv("TMP"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected search-reindex -help to exit 0, got err: %v, out: %s", err, string(out))
	}
}

func TestSearchReindex_InvalidFlag(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestSearchReindex_DeprecatedNoAction")
	cmd.Env = []string{
		"BE_CMD_SEARCH_REINDEX_TEST=1",
		"CMD_ARG=-invalid-flag-xyz",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
		"TEMP=" + os.Getenv("TEMP"),
		"TMP=" + os.Getenv("TMP"),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected search-reindex with invalid flag to exit non-zero, got success: %s", string(out))
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit code 2 for flag error, got %v (out: %s)", err, string(out))
	}
}
