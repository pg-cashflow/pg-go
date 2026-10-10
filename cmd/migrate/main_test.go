package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMigrate_FailClosedWithoutDB(t *testing.T) {
	if os.Getenv("BE_CMD_MIGRATE_TEST") == "1" {
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMigrate_FailClosedWithoutDB")
	cmd.Env = []string{
		"BE_CMD_MIGRATE_TEST=1",
		"SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
	}
	out, err := cmd.CombinedOutput()
	outStr := string(out)
	if err == nil {
		t.Fatalf("expected migrate to exit non-zero without database URL, but succeeded: %s", outStr)
	}
	if strings.Contains(outStr, "panic:") {
		t.Fatalf("migrate panicked instead of failing closed: %s", outStr)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() == 0 {
		t.Fatalf("expected non-zero exit code: %v", err)
	}
	if !strings.Contains(outStr, "DATABASE_URL is required") {
		t.Fatalf("expected output to contain %q, got: %s", "DATABASE_URL is required", outStr)
	}
}
