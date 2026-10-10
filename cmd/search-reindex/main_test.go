package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSearchReindex_DeprecatedNoAction(t *testing.T) {
	if os.Getenv("BE_CMD_SEARCH_REINDEX_TEST") == "1" {
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestSearchReindex_DeprecatedNoAction")
	cmd.Env = []string{
		"BE_CMD_SEARCH_REINDEX_TEST=1",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected search-reindex to exit 0 (deprecated), got err: %v, out: %s", err, string(out))
	}
	if !strings.Contains(string(out), "[DEPRECATED]") {
		t.Errorf("expected deprecation message, got: %s", string(out))
	}
}
